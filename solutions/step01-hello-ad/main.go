// Step 01 — Hello, Ad! / HTTP サーバの土台
//
// 参照実装: steps/step01-hello-ad/README.md に対応する完成形コード。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// =====================================================================
// Config
// =====================================================================

type Config struct {
	Addr     string
	LogLevel slog.Level
}

func loadConfig() Config {
	cfg := Config{
		Addr:     ":8080",
		LogLevel: slog.LevelInfo,
	}
	if v := os.Getenv("ADDR"); v != "" {
		cfg.Addr = v
	}
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	}
	return cfg
}

// =====================================================================
// Domain — Ad / SlotID / Inventory
// =====================================================================

type Ad struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	ClickURL string `json:"click_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type SlotID string

const (
	SlotMainRectangle  SlotID = "main-rectangle"  // 300 x 250
	SlotTopBanner      SlotID = "top-banner"      // 728 x 90
	SlotSideSkyscraper SlotID = "side-skyscraper" // 160 x 600
)

type Inventory map[SlotID][]Ad

func defaultInventory() Inventory {
	return Inventory{
		SlotMainRectangle: {
			{
				ID:       "ad-rect-001",
				Title:    "Acme Shoes",
				ImageURL: "https://placehold.co/300x250/orange/white?text=Acme+Shoes",
				ClickURL: "https://example.com/acme",
				Width:    300, Height: 250,
			},
		},
		SlotTopBanner: {
			{
				ID:       "ad-banner-001",
				Title:    "Globex Coffee",
				ImageURL: "https://placehold.co/728x90/brown/white?text=Globex+Coffee",
				ClickURL: "https://example.com/globex",
				Width:    728, Height: 90,
			},
		},
		// side-skyscraper は意図的に在庫を持たせない → no-fill (204) の確認用。
	}
}

// =====================================================================
// Middleware — Chain / RequestID / AccessLog / Recover
// =====================================================================

type Middleware func(http.Handler) http.Handler

func Chain(mws ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

type ctxKey int

const ctxKeyRequestID ctxKey = iota

var reqIDCounter uint64

func nextRequestID() string {
	n := atomic.AddUint64(&reqIDCounter, 1)
	return fmt.Sprintf("req-%d-%06d", time.Now().UnixNano(), n)
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = nextRequestID()
		}
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func GetRequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("req_id", GetRequestID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("query", r.URL.RawQuery),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("elapsed", time.Since(start)),
				slog.String("ua", r.UserAgent()),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					logger.ErrorContext(r.Context(), "panic in handler",
						slog.String("req_id", GetRequestID(r.Context())),
						slog.Any("recover", rv),
						slog.String("stack", string(debug.Stack())),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// =====================================================================
// Server
// =====================================================================

type Server struct {
	logger    *slog.Logger
	inventory Inventory
}

func newServer(logger *slog.Logger, inv Inventory) *Server {
	return &Server{logger: logger, inventory: inv}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/ad", s.handleAd)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, publisherPageHTML)
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	slot := SlotID(r.URL.Query().Get("slot"))
	if slot == "" {
		slot = SlotMainRectangle
	}

	ads, ok := s.inventory[slot]
	if !ok || len(ads) == 0 {
		s.logger.InfoContext(r.Context(), "no fill",
			slog.String("req_id", GetRequestID(r.Context())),
			slog.String("slot", string(slot)),
		)
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	// Step 02 で random / weighted / highest を導入するまでは先頭固定。
	ad := ads[0]
	s.logger.InfoContext(r.Context(), "ad served",
		slog.String("req_id", GetRequestID(r.Context())),
		slog.String("slot", string(slot)),
		slog.String("ad_id", ad.ID),
	)
	writeJSON(w, http.StatusOK, ad)
}

// =====================================================================
// HTML — publisher ページ
// =====================================================================

const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <title>Sample Publisher</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
    .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; display: flex; align-items: center; justify-content: center; }
    .ad-slot small { color: #999; }
    .meta { color: #aaa; font-size: 11px; }
  </style>
</head>
<body>
  <h1>Sample Publisher — 朝のコーヒー入門</h1>
  <p>コーヒー豆の選び方について。…</p>

  <!-- 広告枠: top banner -->
  <div class="ad-slot" data-slot="top-banner"><small>loading top-banner…</small></div>

  <p>(本文の続き…)</p>

  <!-- 広告枠: main rectangle -->
  <div class="ad-slot" data-slot="main-rectangle"><small>loading main-rectangle…</small></div>

  <p>(さらに続き…)</p>

  <!-- 広告枠: side skyscraper (在庫なし: no-fill を確認するための枠) -->
  <div class="ad-slot" data-slot="side-skyscraper"><small>loading side-skyscraper…</small></div>

  <script>
    document.querySelectorAll('.ad-slot').forEach(el => {
      const slot = el.getAttribute('data-slot');
      fetch('/ad?slot=' + encodeURIComponent(slot))
        .then(r => r.status === 204 ? null : r.json())
        .then(ad => {
          if (!ad) {
            el.innerHTML = '<small>no fill (' + slot + ')</small>';
            return;
          }
          el.innerHTML =
            '<div>' +
              '<a href="' + ad.click_url + '" target="_blank">' +
                '<img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '" alt="' + ad.title + '">' +
              '</a>' +
              '<div class="meta">' + slot + ' / ' + ad.id + '</div>' +
            '</div>';
        })
        .catch(err => {
          el.innerHTML = '<small style="color:#c00">error: ' + err + '</small>';
        });
    });
  </script>
</body>
</html>
`

// =====================================================================
// main — 起動 + graceful shutdown
// =====================================================================

func main() {
	cfg := loadConfig()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	s := newServer(logger, defaultInventory())

	handler := Chain(
		RequestID,
		AccessLog(logger),
		Recover(logger),
	)(s.routes())

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
		logger.Info("server starting", slog.String("addr", cfg.Addr))
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", slog.Any("err", err))
		_ = srv.Close()
		os.Exit(1)
	}
	logger.Info("server stopped cleanly")
}
