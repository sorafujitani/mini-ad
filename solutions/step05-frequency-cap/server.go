// server.go — HTTP handlers + 配信パイプライン (targeting → freq → selector)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	logger    *slog.Logger
	inventory Inventory
	selectors *SelectorRegistry
	events    *EventWriter
	dedup     *Dedup
	freq      *FreqStore
}

func newServer(
	logger *slog.Logger,
	inv Inventory,
	sel *SelectorRegistry,
	ev *EventWriter,
	dd *Dedup,
	fs *FreqStore,
) *Server {
	return &Server{
		logger:    logger,
		inventory: inv,
		selectors: sel,
		events:    ev,
		dedup:     dd,
		freq:      fs,
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/admin/inventory", s.handleAdminInventory)
	mux.HandleFunc("/admin/events", s.handleAdminEvents)
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/ad", s.handleAd)
	mux.HandleFunc("/imp", s.handleImpression)
	mux.HandleFunc("/click", s.handleClick)
	return mux
}

// =====================================================================
// ヘルパ
// =====================================================================

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// 1x1 透明 GIF。imp pixel 用。
var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00,
	0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02,
	0x44, 0x01, 0x00, 0x3b,
}

func writePixel(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_, _ = w.Write(transparentGIF)
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.SplitN(ip, ",", 2)[0]
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}

func defaultStr(v, fb string) string {
	if v == "" {
		return fb
	}
	return v
}

// =====================================================================
// /healthz, /admin/*
// =====================================================================

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleAdminInventory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.inventory)
}

func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v := r.URL.Query().Get("n"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	writeJSON(w, http.StatusOK, s.events.Recent(n))
}

// =====================================================================
// /
// =====================================================================

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// 初回訪問から cookie を付ける
	_ = ensureUID(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, publisherPageHTML)
}

// =====================================================================
// /ad — 配信パイプライン
// =====================================================================

func buildContext(r *http.Request, now time.Time) Context {
	q := r.URL.Query()
	device, osName, browser := ParseUA(r.UserAgent())
	return Context{
		Country:   defaultStr(q.Get("country"), "JP"),
		Device:    defaultStr(q.Get("device"), device),
		OS:        defaultStr(q.Get("os"), osName),
		Browser:   defaultStr(q.Get("browser"), browser),
		DayOfWeek: defaultStr(q.Get("day"), dayOfWeekUTC(now)),
	}
}

type AdResponse struct {
	LineItemID string  `json:"line_item_id"`
	ImpID      string  `json:"imp_id"`
	BidCPM     int     `json:"bid_cpm"`
	Slot       SlotID  `json:"slot"`
	Context    Context `json:"context"`

	ImageURL string `json:"image_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`

	ImpURL   string `json:"imp_url"`
	ViewURL  string `json:"view_url"`
	ClickURL string `json:"click_url"`
}

// eligibleLineItems: targeting → frequency cap を通過した LineItem を返す。
func (s *Server) eligibleLineItems(ctx context.Context, uid string, slot SlotID, reqCtx Context) ([]LineItem, error) {
	var out []LineItem
	for _, li := range s.inventory.BySlot(slot) {
		if !li.Targeting.Matches(reqCtx) {
			continue
		}
		if li.FreqCapPerDay > 0 && uid != "" {
			cnt, err := s.freq.Count(ctx, uid, li.ID)
			if err != nil {
				return nil, err
			}
			if cnt >= li.FreqCapPerDay {
				s.logger.DebugContext(ctx, "freq cap skip",
					slog.String("uid", uid),
					slog.String("line_item", li.ID),
					slog.Int("count", cnt),
					slog.Int("cap", li.FreqCapPerDay),
				)
				continue
			}
		}
		out = append(out, li)
	}
	return out, nil
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slot := SlotID(defaultStr(q.Get("slot"), string(SlotMainRectangle)))
	selector := s.selectors.Resolve(q.Get("strategy"))
	rctx := buildContext(r, time.Now())

	uid := ensureUID(w, r)

	candidates, err := s.eligibleLineItems(r.Context(), uid, slot, rctx)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "freq lookup failed",
			slog.String("uid", uid),
			slog.Any("err", err),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	winner, ok := selector.Pick(candidates)
	if !ok {
		s.logger.InfoContext(r.Context(), "no fill",
			slog.String("req_id", GetRequestID(r.Context())),
			slog.String("slot", string(slot)),
			slog.String("uid", uid),
		)
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	impID := newImpID()

	s.events.Submit(Event{
		Type:       EventAdRequest,
		ImpID:      impID,
		LineItemID: winner.ID,
		Slot:       slot,
		BidCPM:     winner.BidCPM,
		RequestID:  GetRequestID(r.Context()),
		IP:         clientIP(r),
		UA:         r.UserAgent(),
		UID:        uid,
	})

	base := fmt.Sprintf("?imp_id=%s&line_item=%s", impID, winner.ID)
	resp := AdResponse{
		LineItemID: winner.ID,
		ImpID:      impID,
		BidCPM:     winner.BidCPM,
		Slot:       slot,
		Context:    rctx,
		ImageURL:   winner.Creative.ImageURL,
		Width:      winner.Creative.Width,
		Height:     winner.Creative.Height,
		ImpURL:     "/imp" + base + "&t=imp",
		ViewURL:    "/imp" + base + "&t=view",
		ClickURL:   fmt.Sprintf("/click%s&dest=%s", base, url.QueryEscape(winner.Creative.ClickURL)),
	}
	writeJSON(w, http.StatusOK, resp)
}

// =====================================================================
// /imp
// =====================================================================

func (s *Server) handleImpression(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	impID := q.Get("imp_id")
	lineItem := q.Get("line_item")
	t := q.Get("t")

	var et EventType
	switch t {
	case "view":
		et = EventViewable
	default:
		et = EventImpression
	}

	uid := ensureUID(w, r)

	if !s.dedup.Mark(et, impID) {
		s.logger.DebugContext(r.Context(), "duplicate impression suppressed",
			slog.String("imp_id", impID),
			slog.String("type", string(et)),
		)
		writePixel(w)
		return
	}

	s.events.Submit(Event{
		Type:       et,
		ImpID:      impID,
		LineItemID: lineItem,
		RequestID:  GetRequestID(r.Context()),
		IP:         clientIP(r),
		UA:         r.UserAgent(),
		UID:        uid,
	})

	// 表示確定後にカウント。view 用 pixel は重複カウント避け、impression のみ。
	if et == EventImpression && lineItem != "" && uid != "" {
		if err := s.freq.Inc(r.Context(), uid, lineItem); err != nil {
			s.logger.WarnContext(r.Context(), "freq inc failed",
				slog.String("uid", uid),
				slog.String("line_item", lineItem),
				slog.Any("err", err),
			)
		}
	}

	writePixel(w)
}

// =====================================================================
// /click
// =====================================================================

func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	impID := q.Get("imp_id")
	lineItem := q.Get("line_item")
	dest := q.Get("dest")

	if !isAllowedDest(dest) {
		http.Error(w, "invalid dest", http.StatusBadRequest)
		return
	}

	uid := ensureUID(w, r)

	if s.dedup.Mark(EventClick, impID) {
		s.events.Submit(Event{
			Type:       EventClick,
			ImpID:      impID,
			LineItemID: lineItem,
			RequestID:  GetRequestID(r.Context()),
			IP:         clientIP(r),
			UA:         r.UserAgent(),
			UID:        uid,
			Dest:       dest,
		})
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// isAllowedDest: 最低限のスキーム制限 (オープンリダイレクト対策)。
func isAllowedDest(dest string) bool {
	if dest == "" {
		return false
	}
	u, err := url.Parse(dest)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// =====================================================================
// HTML — credentials: 'same-origin' で cookie を送る
// =====================================================================

const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8"><title>Step 05 — Frequency Cap</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 760px; margin: 40px auto; padding: 0 16px; }
  .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
  .controls > * { margin-right: 8px; }
  .meta { font-size: 11px; color: #999; }
  pre { background: #f4f4f4; padding: 8px; font-size: 11px; }
  .spacer { height: 60vh; background: #fafafa; margin: 16px 0; display: flex; align-items: center; justify-content: center; color: #ccc; }
</style></head><body>
  <h1>Step 05 — Frequency Cap</h1>
  <p>同じ <code>mini_ad_uid</code> cookie で何度もリロードすると、<code>FreqCapPerDay</code> に達した LineItem が候補から外れる。</p>

  <div class="controls">
    country <select id="country"><option value="">(auto)</option><option>JP</option><option>US</option></select>
    device  <select id="device"><option value="">(auto)</option><option>mobile</option><option>desktop</option><option>tablet</option></select>
    <button onclick="loadAll()">reload all</button>
  </div>

  <div class="ad-slot" data-slot="top-banner"><small>top-banner</small></div>

  <div class="spacer">↓ スクロールして view を発火</div>

  <div class="ad-slot" data-slot="main-rectangle"><small>main-rectangle</small></div>

  <pre id="meta">resolved context will show here</pre>

  <script>
  (function () {
    const observed = new WeakMap();
    function fireViewWhenVisible(el, viewURL) {
      const io = new IntersectionObserver(entries => {
        for (const ent of entries) {
          if (ent.intersectionRatio < 0.5) {
            clearTimeout(observed.get(el));
            observed.delete(el);
            continue;
          }
          if (observed.has(el)) continue;
          const tid = setTimeout(() => {
            const img = new Image(1, 1);
            img.src = viewURL;
            io.disconnect();
          }, 1000);
          observed.set(el, tid);
        }
      }, { threshold: [0, 0.25, 0.5, 0.75, 1.0] });
      io.observe(el);
    }

    function qstr() {
      const ids = ['country','device'];
      return ids.map(id => {
        const v = document.getElementById(id).value;
        return v ? id + '=' + encodeURIComponent(v) : '';
      }).filter(Boolean).join('&');
    }

    function loadOne(slot) {
      const el = document.querySelector('[data-slot="' + slot + '"]');
      const q = qstr();
      fetch('/ad?slot=' + slot + (q ? '&' + q : ''), { credentials: 'same-origin' })
        .then(r => r.status === 204 ? null : r.json())
        .then(ad => {
          if (!ad) { el.innerHTML = '<small>no fill (' + slot + ')</small>'; return; }
          el.innerHTML =
            '<a href="' + ad.click_url + '">' +
              '<img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '" alt="">' +
            '</a>' +
            '<img src="' + ad.imp_url + '" width="1" height="1" style="display:none">' +
            '<div class="meta">imp_id=' + ad.imp_id + ' line_item=' + ad.line_item_id + ' bid_cpm=' + ad.bid_cpm + '</div>';
          document.getElementById('meta').textContent = JSON.stringify(ad.context, null, 2);
          fireViewWhenVisible(el, ad.view_url);
        });
    }

    function loadAll() {
      document.querySelectorAll('.ad-slot').forEach(el => loadOne(el.dataset.slot));
    }
    window.loadAll = loadAll;
    loadAll();
  })();
  </script>
</body></html>
`
