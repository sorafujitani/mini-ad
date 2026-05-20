// Step 02 — 広告選択ロジック
//
// 参照実装: steps/step02-ad-selection/README.md に対応する完成形コード。
// Step 01 の HTTP サーバ土台 (middleware / graceful shutdown) を継承し、
// Selector interface + 3 戦略 (random / weighted / highest) と
// Decision ログを追加している。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// =====================================================================
// Config
// =====================================================================

type Config struct {
	Addr            string
	LogLevel        slog.Level
	DefaultStrategy string
	RandomSeed      int64 // 0 = time-based
}

func loadConfig() Config {
	cfg := Config{
		Addr:            ":8080",
		LogLevel:        slog.LevelInfo,
		DefaultStrategy: "weighted",
	}
	if v := os.Getenv("ADDR"); v != "" {
		cfg.Addr = v
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
	BidCPM   int    `json:"bid_cpm"` // 整数セント / 1000 imp
}

type SlotID string

const (
	SlotMainRectangle  SlotID = "main-rectangle"
	SlotTopBanner      SlotID = "top-banner"
	SlotSideSkyscraper SlotID = "side-skyscraper"
)

type Inventory map[SlotID][]Ad

func defaultInventory() Inventory {
	return Inventory{
		SlotMainRectangle: {
			{ID: "ad-rect-acme", Title: "Acme Shoes", ImageURL: "https://placehold.co/300x250/orange/white?text=Acme", ClickURL: "https://example.com/acme", Width: 300, Height: 250, BidCPM: 300},
			{ID: "ad-rect-globex", Title: "Globex Coffee", ImageURL: "https://placehold.co/300x250/brown/white?text=Globex", ClickURL: "https://example.com/globex", Width: 300, Height: 250, BidCPM: 250},
			{ID: "ad-rect-initech", Title: "Initech Insurance", ImageURL: "https://placehold.co/300x250/teal/white?text=Initech", ClickURL: "https://example.com/initech", Width: 300, Height: 250, BidCPM: 150},
			{ID: "ad-rect-hooli", Title: "Hooli Cloud", ImageURL: "https://placehold.co/300x250/blue/white?text=Hooli", ClickURL: "https://example.com/hooli", Width: 300, Height: 250, BidCPM: 80},
		},
		SlotTopBanner: {
			{ID: "ad-banner-acme", Title: "Acme Banner", ImageURL: "https://placehold.co/728x90/orange/white?text=Acme+Banner", ClickURL: "https://example.com/acme/banner", Width: 728, Height: 90, BidCPM: 200},
			{ID: "ad-banner-house", Title: "house ad", ImageURL: "https://placehold.co/728x90/gray/white?text=House+Ad", ClickURL: "https://example.com/house", Width: 728, Height: 90, BidCPM: 10},
		},
		// side-skyscraper は意図的に在庫を持たせない → no-fill (204) 確認用。
	}
}

// =====================================================================
// Selector — Strategy パターン
// =====================================================================

// Selector は ads から 1 つ広告を選ぶ。各実装はランダム性のために *mrand.Rand を内部で持つ。
type Selector interface {
	Name() string
	Pick(ads []Ad) (Ad, bool)
}

// --- 1. uniform random ---

type RandomSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex // *mrand.Rand は並行アクセス不可なので保護
}

func NewRandomSelector(rng *mrand.Rand) *RandomSelector {
	return &RandomSelector{rng: rng}
}

func (s *RandomSelector) Name() string { return "random" }

func (s *RandomSelector) Pick(ads []Ad) (Ad, bool) {
	if len(ads) == 0 {
		return Ad{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return ads[s.rng.Intn(len(ads))], true
}

// --- 2. weighted random (CDF サンプリング) ---

type WeightedSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex
}

func NewWeightedSelector(rng *mrand.Rand) *WeightedSelector {
	return &WeightedSelector{rng: rng}
}

func (s *WeightedSelector) Name() string { return "weighted" }

func (s *WeightedSelector) Pick(ads []Ad) (Ad, bool) {
	if len(ads) == 0 {
		return Ad{}, false
	}
	total := 0
	for _, a := range ads {
		if a.BidCPM > 0 {
			total += a.BidCPM
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if total == 0 {
		// 全員 bid_cpm = 0 → fallback で uniform random
		return ads[s.rng.Intn(len(ads))], true
	}
	r := s.rng.Intn(total)
	cum := 0
	for _, a := range ads {
		if a.BidCPM <= 0 {
			continue
		}
		cum += a.BidCPM
		if r < cum {
			return a, true
		}
	}
	return ads[len(ads)-1], true
}

// --- 3. highest bid ---

type HighestBidSelector struct{}

func (HighestBidSelector) Name() string { return "highest" }

func (HighestBidSelector) Pick(ads []Ad) (Ad, bool) {
	if len(ads) == 0 {
		return Ad{}, false
	}
	best := ads[0]
	for _, a := range ads[1:] {
		if a.BidCPM > best.BidCPM {
			best = a
		}
	}
	return best, true
}

// --- registry ---

type SelectorRegistry struct {
	defaultName string
	selectors   map[string]Selector
}

func NewSelectorRegistry(defaultName string, rng *mrand.Rand) *SelectorRegistry {
	return &SelectorRegistry{
		defaultName: defaultName,
		selectors: map[string]Selector{
			"random":   NewRandomSelector(rng),
			"weighted": NewWeightedSelector(rng),
			"highest":  HighestBidSelector{},
		},
	}
}

func (r *SelectorRegistry) Resolve(name string) Selector {
	if name == "" {
		name = r.defaultName
	}
	if s, ok := r.selectors[name]; ok {
		return s
	}
	return r.selectors[r.defaultName]
}

// =====================================================================
// Decision — 「なぜこの広告を選んだか」のログ
// =====================================================================

type Decision struct {
	RequestID    string    `json:"request_id"`
	OccurredAt   time.Time `json:"occurred_at"`
	Slot         SlotID    `json:"slot"`
	Strategy     string    `json:"strategy"`
	Candidates   int       `json:"candidates"`
	WinnerID     string    `json:"winner_id,omitempty"`
	WinnerBidCPM int       `json:"winner_bid_cpm,omitempty"`
	NoFill       bool      `json:"no_fill,omitempty"`
}

// =====================================================================
// Middleware — Chain / RequestID / AccessLog / Recover
// (Step 01 から継承)
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

var reqIDCounter atomic.Uint64

func nextRequestID() string {
	n := reqIDCounter.Add(1)
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
	selectors *SelectorRegistry
}

func newServer(logger *slog.Logger, inv Inventory, sel *SelectorRegistry) *Server {
	return &Server{logger: logger, inventory: inv, selectors: sel}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/admin/inventory", s.handleAdminInventory)
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
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleAdminInventory(w http.ResponseWriter, r *http.Request) {
	// 学習用なので認証なし。本番では基本書かない。
	writeJSON(w, http.StatusOK, s.inventory)
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
	q := r.URL.Query()
	slot := SlotID(q.Get("slot"))
	if slot == "" {
		slot = SlotMainRectangle
	}
	selector := s.selectors.Resolve(q.Get("strategy"))

	ads := s.inventory[slot]
	winner, ok := selector.Pick(ads)

	dec := Decision{
		RequestID:  GetRequestID(r.Context()),
		OccurredAt: time.Now().UTC(),
		Slot:       slot,
		Strategy:   selector.Name(),
		Candidates: len(ads),
	}
	if !ok {
		dec.NoFill = true
		s.logDecision(r.Context(), dec)
		writeJSON(w, http.StatusNoContent, nil)
		return
	}
	dec.WinnerID = winner.ID
	dec.WinnerBidCPM = winner.BidCPM
	s.logDecision(r.Context(), dec)

	writeJSON(w, http.StatusOK, winner)
}

func (s *Server) logDecision(ctx context.Context, d Decision) {
	s.logger.InfoContext(ctx, "decision",
		slog.String("req_id", d.RequestID),
		slog.String("slot", string(d.Slot)),
		slog.String("strategy", d.Strategy),
		slog.Int("candidates", d.Candidates),
		slog.String("winner_id", d.WinnerID),
		slog.Int("winner_bid_cpm", d.WinnerBidCPM),
		slog.Bool("no_fill", d.NoFill),
	)
}

// =====================================================================
// HTML — strategy 切替 UI
// =====================================================================

const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <title>Step 02 — Ad Selection</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
    .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
    .ad-slot small { color: #999; }
    .controls button { margin-right: 8px; }
    .meta { font-size: 11px; color: #aaa; }
  </style>
</head>
<body>
  <h1>Step 02 — Ad Selection</h1>

  <div class="controls">
    strategy:
    <button onclick="loadAll('random')">random</button>
    <button onclick="loadAll('weighted')">weighted</button>
    <button onclick="loadAll('highest')">highest</button>
  </div>

  <div class="ad-slot" data-slot="top-banner"><small>top-banner</small></div>
  <div class="ad-slot" data-slot="main-rectangle"><small>main-rectangle</small></div>
  <div class="ad-slot" data-slot="side-skyscraper"><small>side-skyscraper</small></div>

  <script>
    function loadOne(slot, strategy) {
      const el = document.querySelector('[data-slot="' + slot + '"]');
      fetch('/ad?slot=' + slot + '&strategy=' + strategy)
        .then(r => r.status === 204 ? null : r.json())
        .then(ad => {
          if (!ad) { el.innerHTML = '<small>no fill (' + slot + ')</small>'; return; }
          el.innerHTML =
            '<a href="' + ad.click_url + '"><img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '"></a>' +
            '<div class="meta">' + slot + ' / ' + ad.id + ' / bid_cpm=' + ad.bid_cpm + '</div>';
        });
    }
    function loadAll(strategy) {
      document.querySelectorAll('.ad-slot').forEach(el => loadOne(el.dataset.slot, strategy));
    }
    loadAll('weighted');
  </script>
</body>
</html>
`

// =====================================================================
// main — 起動 + graceful shutdown
// =====================================================================

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// 乱数源。RANDOM_SEED が指定されていれば deterministic に。
	seed := cfg.RandomSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	rng := mrand.New(mrand.NewSource(seed))
	logger.Info("random source initialized", slog.Int64("seed", seed))

	selectors := NewSelectorRegistry(cfg.DefaultStrategy, rng)
	s := newServer(logger, defaultInventory(), selectors)

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
			slog.String("default_strategy", cfg.DefaultStrategy),
		)
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
