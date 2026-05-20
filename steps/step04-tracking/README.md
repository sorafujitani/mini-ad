# Step 04 — トラッキング

> 「広告を返した」≠「広告が見られた」。imp pixel + click redirect + **async event writer** + **imp_id dedup** + **viewability**。

---

## このステップで学ぶこと

- **impression pixel** (1x1 透明画像) を使った表示計測
- **click redirect** (アドサーバ経由で LP へ) によるクリック計測
- リクエストごとの `imp_id` で **ad request → imp → click を紐付ける** 設計
- **非同期イベントライター** (buffered chan + worker goroutine)
  - ホットパス (HTTP ハンドラ) を I/O で詰まらせない
- **imp_id dedup** (TTL 付き in-memory set) で二重計上を防ぐ
- **Viewability** の入り口: `IntersectionObserver` で「50% / 1 秒見えた」を判定する JS
- **オープンリダイレクト** 対策

関連座学: [docs/05-tracking.md](../../docs/05-tracking.md)

---

## 前提

Step 03 の構造 (middleware / Selector / Targeting / Inventory / Server) を継承。
ファイルをさらに分けます：

```
steps/step04-tracking/
├── main.go        ← config + main + graceful shutdown
├── middleware.go  ← (Step 01 と同じ)
├── domain.go      ← LineItem / Inventory (Step 03 と同じ)
├── targeting.go   ← Targeting / Matcher (Step 03 と同じ)
├── ua.go          ← UA パース (Step 03 と同じ)
├── selector.go    ← Selector (Step 03 と同じ)
├── ids.go         ← imp_id 生成
├── events.go      ← Event / EventWriter / dedup
└── server.go      ← Server + handlers + HTML
```

---

## 何を作るか

| Method & Path | レスポンス | 役割 |
|---------------|-----------|------|
| `GET /` | HTML | publisher ページ。 imp pixel + viewability JS |
| `GET /ad?slot=...` | JSON | 広告 + **`imp_id` / `imp_url` / `click_url`** + `view_url` |
| `GET /imp?imp_id=...&t=imp` | 1x1 GIF | impression 計上 |
| `GET /imp?imp_id=...&t=view` | 1x1 GIF | viewable impression 計上 |
| `GET /click?imp_id=...&dest=...` | 302 | click 計上 + LP リダイレクト |
| `GET /admin/events?n=50` | JSON | 最近のイベント (in-memory ring buffer) |
| `GET /healthz` / `/admin/inventory` | (同) | |

イベントはファイル (`logs/events.jsonl`) と in-memory ring buffer に書く。
**ハンドラはチャネルに投げて即返す**、書き込みは別 goroutine が担当。

---

## 実装

### A. `ids.go` — imp_id 生成

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newImpID: 16 byte hex (32 文字)。
// crypto/rand を使う = 推測困難 = impression を後から偽装しにくい
func newImpID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

### B. `events.go` — Event / EventWriter / dedup

#### B-1. Event 型

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type EventType string

const (
	EventAdRequest   EventType = "ad_request"
	EventImpression  EventType = "impression"
	EventViewable    EventType = "viewable"
	EventClick       EventType = "click"
)

type Event struct {
	Type       EventType `json:"type"`
	ImpID      string    `json:"imp_id"`
	LineItemID string    `json:"line_item_id,omitempty"`
	Slot       SlotID    `json:"slot,omitempty"`
	BidCPM     int       `json:"bid_cpm,omitempty"`
	RequestID  string    `json:"request_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	IP         string    `json:"ip,omitempty"`
	UA         string    `json:"ua,omitempty"`
	Dest       string    `json:"dest,omitempty"`
}
```

#### B-2. Dedup (TTL 付き in-memory set)

```go
// Dedup は imp_id × event-type の組み合わせを TTL 付きで覚えておく。
// 「同じ pixel が二度読まれた」を 1 回扱いにする。
type Dedup struct {
	mu  sync.Mutex
	m   map[string]time.Time // key = type + ":" + impID
	ttl time.Duration
}

func NewDedup(ttl time.Duration) *Dedup {
	d := &Dedup{m: make(map[string]time.Time), ttl: ttl}
	go d.gcLoop()
	return d
}

// Mark: 初出なら true (= 今回処理して OK)、既に見たことあるなら false。
func (d *Dedup) Mark(eventType EventType, impID string) bool {
	if impID == "" {
		return true
	}
	k := string(eventType) + ":" + impID
	d.mu.Lock()
	defer d.mu.Unlock()
	if exp, ok := d.m[k]; ok && time.Now().Before(exp) {
		return false
	}
	d.m[k] = time.Now().Add(d.ttl)
	return true
}

func (d *Dedup) gcLoop() {
	t := time.NewTicker(d.ttl / 2)
	defer t.Stop()
	for range t.C {
		d.mu.Lock()
		now := time.Now()
		for k, exp := range d.m {
			if now.After(exp) {
				delete(d.m, k)
			}
		}
		d.mu.Unlock()
	}
}
```

#### B-3. EventWriter (async + ring buffer)

```go
// EventWriter は Event を非同期にファイルに append しつつ、
// /admin/events で覗ける in-memory ring buffer にも保存する。
type EventWriter struct {
	logger *slog.Logger
	ch     chan Event
	done   chan struct{}

	// ring buffer
	mu   sync.Mutex
	ring []Event
	head int
	cap  int

	f *os.File
}

func NewEventWriter(logger *slog.Logger, path string, bufSize, ringCap int) (*EventWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &EventWriter{
		logger: logger,
		ch:     make(chan Event, bufSize),
		done:   make(chan struct{}),
		ring:   make([]Event, ringCap),
		cap:    ringCap,
		f:      f,
	}
	go w.run()
	return w, nil
}

// Submit: ハンドラから呼ぶ。chan が満杯ならログだけ出して捨てる (ホットパスをブロックしない)。
func (w *EventWriter) Submit(e Event) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	select {
	case w.ch <- e:
	default:
		// chan 満杯: ad 配信の継続を最優先、計測は捨てる。
		// 本物のシステムでは「捨てる」ではなく「Kafka / S3 にスピル」する。
		w.logger.Warn("event channel full, dropping event",
			slog.String("type", string(e.Type)),
			slog.String("imp_id", e.ImpID),
		)
	}
}

func (w *EventWriter) Close(ctx context.Context) error {
	close(w.ch)
	select {
	case <-w.done:
		return w.f.Close()
	case <-ctx.Done():
		return errors.New("event writer flush timed out")
	}
}

func (w *EventWriter) Recent(n int) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n > w.cap {
		n = w.cap
	}
	out := make([]Event, 0, n)
	// head は次に書く位置。直近 n 件は head の左側。
	for i := 0; i < w.cap && len(out) < n; i++ {
		idx := (w.head - 1 - i + w.cap) % w.cap
		if w.ring[idx].OccurredAt.IsZero() {
			break
		}
		out = append(out, w.ring[idx])
	}
	return out
}

// === internals ===

func (w *EventWriter) run() {
	defer close(w.done)
	enc := json.NewEncoder(w.f)
	for e := range w.ch {
		if err := enc.Encode(e); err != nil {
			w.logger.Error("event write failed", slog.Any("err", err))
		}
		w.appendRing(e)
	}
}

func (w *EventWriter) appendRing(e Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ring[w.head] = e
	w.head = (w.head + 1) % w.cap
}
```

ポイント：
- **chan が満杯 → 落とす** が現代的なホットパス設計。配信を止めるくらいなら計測を欠損させる
- ring buffer は **直近 N 件だけ覗ければよい** デバッグ用途。永続化はファイル側で
- `Close` で chan を閉じて worker の終了を context 付きで待つ

### C. `server.go` — handlers (Step 03 + tracking 系)

`Server` に EventWriter / Dedup を追加。

```go
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
}

func newServer(logger *slog.Logger, inv Inventory, sel *SelectorRegistry, ev *EventWriter, dd *Dedup) *Server {
	return &Server{logger: logger, inventory: inv, selectors: sel, events: ev, dedup: dd}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/admin/inventory", s.handleAdminInventory)
	mux.HandleFunc("/admin/events", s.handleAdminEvents)
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/ad", s.handleAd)
	mux.HandleFunc("/imp", s.handleImpression) // ?t=imp or ?t=view
	mux.HandleFunc("/click", s.handleClick)
	return mux
}

// === ヘルパ ===

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

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

// === admin ===

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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

// === pages ===

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, publisherPageHTML)
}

// === /ad ===

type AdResponse struct {
	LineItemID string  `json:"line_item_id"`
	ImpID      string  `json:"imp_id"`
	BidCPM     int     `json:"bid_cpm"`
	Slot       SlotID  `json:"slot"`
	Context    Context `json:"context"`

	// クリエイティブ (画像 URL とサイズ)
	ImageURL string `json:"image_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`

	// ad server 経由 URL
	ImpURL   string `json:"imp_url"`   // ?t=imp の pixel
	ViewURL  string `json:"view_url"`  // ?t=view の pixel (viewability)
	ClickURL string `json:"click_url"` // 302 redirect
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slot := SlotID(defaultStr(q.Get("slot"), string(SlotMainRectangle)))
	selector := s.selectors.Resolve(q.Get("strategy"))
	rctx := buildContext(r, time.Now())

	candidates := filterByTargeting(s.inventory.BySlot(slot), rctx)
	winner, ok := selector.Pick(candidates)
	if !ok {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	impID := newImpID()

	// ad_request イベントを記録
	s.events.Submit(Event{
		Type:       EventAdRequest,
		ImpID:      impID,
		LineItemID: winner.ID,
		Slot:       slot,
		BidCPM:     winner.BidCPM,
		RequestID:  GetRequestID(r.Context()),
		IP:         clientIP(r),
		UA:         r.UserAgent(),
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

// === /imp ===

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

	if !s.dedup.Mark(et, impID) {
		// 二重発火: pixel は返すが記録しない
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
	})
	writePixel(w)
}

// === /click ===

func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	impID := q.Get("imp_id")
	lineItem := q.Get("line_item")
	dest := q.Get("dest")

	// オープンリダイレクト対策
	if !isAllowedDest(dest) {
		http.Error(w, "invalid dest", http.StatusBadRequest)
		return
	}

	if s.dedup.Mark(EventClick, impID) {
		s.events.Submit(Event{
			Type:       EventClick,
			ImpID:      impID,
			LineItemID: lineItem,
			RequestID:  GetRequestID(r.Context()),
			IP:         clientIP(r),
			UA:         r.UserAgent(),
			Dest:       dest,
		})
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// isAllowedDest: 最低限のスキーム制限。本番では host の whitelist が必要。
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
```

ポイント：
- `?t=imp` と `?t=view` でエンドポイント 1 本に 2 つのイベントを兼用 (URL が増えるのを避ける)
- `EventAdRequest` を **配信時に必ず 1 件出す** ことで「ad request → impression → click」の漏斗が後から作れる
- 「dedup 失敗 → pixel 自体は返す」のは重要。返さないと CDN や browser cache が拗ねる

### D. `server.go` (続き) — HTML テンプレート (viewability JS 入り)

```go
const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8"><title>Step 04 — Tracking</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
  .ad-slot { margin: 32px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
  .meta { font-size: 11px; color: #999; }
  .spacer { height: 80vh; background: #fafafa; margin: 16px 0; display: flex; align-items: center; justify-content: center; color: #ccc; }
</style></head><body>
  <h1>Step 04 — Tracking</h1>
  <p>imp pixel + click redirect + viewability (IntersectionObserver)。<br>
  下にスクロールしないと <code>view</code> イベントが発火しないことを確認できる。</p>

  <div class="ad-slot" data-slot="top-banner"><small>top-banner</small></div>

  <div class="spacer">↓ スクロールしてね</div>

  <div class="ad-slot" data-slot="main-rectangle"><small>main-rectangle</small></div>

  <script>
  (function () {
    // IntersectionObserver: 要素の 50% が表示されて 1 秒経過したら view を発火。
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
            // 1 秒以上 50% 以上見えてる → viewable
            const img = new Image(1, 1);
            img.src = viewURL;
            io.disconnect();
          }, 1000);
          observed.set(el, tid);
        }
      }, { threshold: [0, 0.25, 0.5, 0.75, 1.0] });
      io.observe(el);
    }

    document.querySelectorAll('.ad-slot').forEach(el => {
      const slot = el.getAttribute('data-slot');
      fetch('/ad?slot=' + slot)
        .then(r => r.status === 204 ? null : r.json())
        .then(ad => {
          if (!ad) { el.innerHTML = '<small>no fill</small>'; return; }
          el.innerHTML =
            '<a href="' + ad.click_url + '">' +
              '<img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '" alt="">' +
            '</a>' +
            '<img src="' + ad.imp_url + '" width="1" height="1" style="display:none">' +
            '<div class="meta">imp_id=' + ad.imp_id + ' line_item=' + ad.line_item_id + ' bid_cpm=' + ad.bid_cpm + '</div>';
          fireViewWhenVisible(el, ad.view_url);
        });
    });
  })();
  </script>
</body></html>
`
```

ポイント：
- `IntersectionObserver` で **50% 以上見えた状態が 1 秒継続** したら view pixel を発火 (MRC ガイドラインの基本)
- `<img src="…imp_url…">` は即時発火、`view_url` は条件発火 — **「ad server に届いた = 表示された」と「実際にユーザーに見えた」を区別** できる

### E. `main.go` — EventWriter のライフサイクル管理込み

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Addr            string
	LogLevel        slog.Level
	DefaultStrategy string
	RandomSeed      int64
	EventLogPath    string
	EventBufSize    int
	EventRingCap    int
	DedupTTL        time.Duration
}

func loadConfig() Config {
	cfg := Config{
		Addr:            ":8080",
		LogLevel:        slog.LevelInfo,
		DefaultStrategy: "weighted",
		EventLogPath:    "logs/events.jsonl",
		EventBufSize:    1024,
		EventRingCap:    256,
		DedupTTL:        10 * time.Minute,
	}
	if v := os.Getenv("ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("EVENT_LOG"); v != "" {
		cfg.EventLogPath = v
	}
	if v := os.Getenv("DEFAULT_STRATEGY"); v != "" {
		cfg.DefaultStrategy = v
	}
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	}
	if v := os.Getenv("RANDOM_SEED"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.RandomSeed)
	}
	return cfg
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	seed := cfg.RandomSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	rng := mrand.New(mrand.NewSource(seed))

	events, err := NewEventWriter(logger, cfg.EventLogPath, cfg.EventBufSize, cfg.EventRingCap)
	if err != nil {
		logger.Error("event writer init", slog.Any("err", err))
		os.Exit(1)
	}
	dedup := NewDedup(cfg.DedupTTL)

	s := newServer(logger,
		defaultInventory(),
		NewSelectorRegistry(cfg.DefaultStrategy, rng),
		events, dedup)

	handler := Chain(RequestID, AccessLog(logger), Recover(logger))(s.routes())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting",
			slog.String("addr", cfg.Addr),
			slog.String("event_log", cfg.EventLogPath))
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown requested by signal")
	case err := <-serverErr:
		if err != nil {
			logger.Error("server crashed", slog.Any("err", err))
			os.Exit(1)
		}
	}

	// 1) サーバの新規受付停止 + in-flight 完了待ち
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", slog.Any("err", err))
	}
	// 2) EventWriter の buffered chan を flush
	flushCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := events.Close(flushCtx); err != nil {
		logger.Error("event writer close", slog.Any("err", err))
	}
	logger.Info("server stopped cleanly")
}
```

ポイント：
- **shutdown は 2 段階**: まず HTTP server を止める (新規 imp が来なくなる) → 次に EventWriter を flush
- 順序を逆にすると「サーバが受け続けて chan に積み続ける」状態でラインを締めることになる

### F. `middleware.go` / `domain.go` / `targeting.go` / `ua.go` / `selector.go`

Step 03 からそのままコピー。**変更不要**。

`Server` が受け取る引数の数は増えたので、`newServer` のシグネチャ変更だけ反映する。

---

## 動作確認

```bash
go run ./steps/step04-tracking/

# ブラウザで http://localhost:8080 を開く (スクロールして viewability を確認)

# in-memory ring buffer で直近のイベントを覗く
curl -s 'http://localhost:8080/admin/events?n=20' | jq

# ファイル側
tail -f logs/events.jsonl | jq -c

# dedup の動作確認: 同じ imp_id で 2 回叩いて、ログには 1 件しか来ない
curl -s 'http://localhost:8080/ad?slot=main-rectangle' > /tmp/ad.json
IMP=$(jq -r .imp_id /tmp/ad.json)
LI=$(jq -r .line_item_id /tmp/ad.json)
curl -s "http://localhost:8080/imp?imp_id=$IMP&line_item=$LI&t=imp" -o /dev/null
curl -s "http://localhost:8080/imp?imp_id=$IMP&line_item=$LI&t=imp" -o /dev/null
curl -s 'http://localhost:8080/admin/events?n=10' | jq '[.[] | select(.imp_id=="'$IMP'") | .type]'
# → ["impression","ad_request"]  (impression は 1 回だけ)

# オープンリダイレクトのブロック確認
curl -sI 'http://localhost:8080/click?imp_id=x&line_item=y&dest=javascript:alert(1)' | head -1
# → HTTP/1.1 400 Bad Request

# Ctrl-C で graceful shutdown を観察 (event log の flush ログが出ること)
```

---

## 実験してみよう

- `EventBufSize=4` まで小さくして、`hey -n 1000 -c 50 'http://localhost:8080/ad?slot=main-rectangle'` で chan 飽和を起こす → `event channel full, dropping event` の WARN ログが出るのを確認
- `Dedup.TTL` を `5 * time.Second` に変えて、5 秒後に同じ imp_id が再度通ることを確認
- `IntersectionObserver` の `threshold` を 0.75 に変えて発火しづらくする
- `EventAdRequest` を **常に 1 件** 出すロジックを使って、「ad request 全体に対する impression 率」を後から集計するクエリを作る
- `isAllowedDest` を host whitelist にする (`example.com`, `placehold.co` だけ許可)
- `EventWriter` のファイル書き込みを **batch** にする (chan から最大 N 件 or M 秒待って一括 write)

---

## 設計上のメモ

- **ホットパス (ad/imp/click) は chan に投げて即返す** のが鉄則。同期 I/O は遅延を直接ユーザに見せてしまう
- **dedup** は「**何を一意とみなすか**」の設計問題。imp_id 単独 / (imp_id, type) / (imp_id, ip) などバリエーション
- 「**ad_request → impression → viewable → click**」の漏斗を全部取れると、各段階のロス率が分析できる (アドサーバの定番ダッシュボード)
- viewability は **MRC** が標準化。ディスプレイは「50% / 1 秒」、動画は「50% / 2 秒」が境界

---

## 次へ

→ [Step 05 — フリークエンシーキャップ (Redis)](../step05-frequency-cap/)
