# Step 02 — 広告選択ロジック

> 在庫から「どれを返すか」を決める意思決定の土台。**Strategy パターン** + 決定理由のログ化。

---

## このステップで学ぶこと

- **配信戦略 (Strategy パターン)** — `Selector` という interface に複数実装を持たせる
- 配信アルゴリズム 3 系統
  - ランダム選択 (`uniform random`)
  - 重み付け選択 (`weighted random` / 累積分布関数で実装)
  - 最高入札 (`highest bid`)
- **Decision (意思決定ログ)** — 「なぜこの広告を選んだか」「他に何があったか」を残す習慣
- テスト容易性のための **deterministic random** (固定 seed)

関連座学: [docs/02-terminology.md](../../docs/02-terminology.md) §課金モデル, [docs/06-auction.md](../../docs/06-auction.md)

---

## 前提

Step 01 で組んだ土台 (slog, middleware, graceful shutdown, Server, Inventory) を **そのまま継承** します。
Step 01 の `main.go` をコピーしてから差分を当てる、というのが楽：

```bash
cp -r steps/step01-hello-ad steps/step02-ad-selection
# main.go を以下に従って書き換え
```

---

## 何を作るか

| Method & Path | 役割 |
|---------------|------|
| `GET /` | publisher ページ (3 戦略を切り替えるボタン) |
| `GET /ad?slot=...&strategy=random` | 一様ランダム |
| `GET /ad?slot=...&strategy=weighted` | bid_cpm 重み付け |
| `GET /ad?slot=...&strategy=highest` | 最高入札 |
| `GET /admin/inventory` | 現在の在庫を JSON で返す (デバッグ用) |
| `GET /healthz` | (Step 01 と同じ) |

---

## 実装

### A. パッケージ宣言とインポート

```go
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
```

`math/rand` を `mrand` にエイリアスするのは Step 04 以降で `crypto/rand` も併用するため早めに癖をつける。

### B. 設定 — strategy を環境変数で初期化

```go
type Config struct {
	Addr             string
	LogLevel         slog.Level
	DefaultStrategy  string
	RandomSeed       int64 // 0 = time-based
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
	// RANDOM_SEED で再現可能にする (テスト用)
	if v := os.Getenv("RANDOM_SEED"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.RandomSeed)
	}
	return cfg
}
```

### C. ドメイン型 — Ad に bid_cpm を追加

```go
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
	}
}
```

### D. Selector interface と 3 実装

Strategy パターン: 「選び方」を interface にして実装差し替え可能にする。

```go
// Selector は ads から 1 つ広告を選ぶ。
// 各実装はランダム性のために *mrand.Rand を内部で持つ。
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
```

ポイント：
- **`*mrand.Rand` は並行 unsafe** なので `sync.Mutex` で囲う (テストで固定 seed を使うと顕著に問題になる)
- `Name()` を持たせて decision log に乗せる
- `HighestBidSelector` には乱数源が要らないので zero value で使える

#### Selector レジストリ

```go
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

// Resolve: name が空 or 未知の場合は default を返す。
func (r *SelectorRegistry) Resolve(name string) Selector {
	if name == "" {
		name = r.defaultName
	}
	if s, ok := r.selectors[name]; ok {
		return s
	}
	return r.selectors[r.defaultName]
}
```

### E. Decision log — 「なぜこの広告を選んだか」

```go
// Decision は配信時の意思決定スナップショット。後で分析可能。
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
```

これを slog で吐けば、後段の集計基盤で「strategy 別の no-fill 率」とか「candidate=0 の割合」とか出せる。

### F. middleware — Step 01 と同じ

middleware セクション (`Chain`, `RequestID`, `AccessLog`, `Recover`, `statusRecorder` 周り) はそのまま継承。**変更不要なので Step 01 から丸ごとコピー**。

### G. Server — Selector を依存に持つ

```go
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
```

### H. HTML テンプレート — strategy 切替 UI

```go
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
```

### I. main — Selector を組み立てて Server に渡す

```go
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
		logger.Info("server starting", slog.String("addr", cfg.Addr), slog.String("default_strategy", cfg.DefaultStrategy))
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

// (debug.Stack は Step 01 から継承)
var _ = debug.Stack
var _ atomic.Int64
```

### J. middleware は Step 01 と同型なので継承

`Chain`, `RequestID`, `AccessLog`, `Recover`, `statusRecorder`, `GetRequestID`, `ctxKey` を Step 01 からそのままコピーすること。

---

## 動作確認

```bash
go run ./steps/step02-ad-selection/

# 戦略別に分布を確認
for s in random weighted highest; do
  echo "=== $s ==="
  for i in $(seq 1 50); do
    curl -s "http://localhost:8080/ad?strategy=$s" | jq -r '.id'
  done | sort | uniq -c
done

# 在庫を覗く
curl -s http://localhost:8080/admin/inventory | jq

# decision log を tail (JSON)
go run ./steps/step02-ad-selection/ 2>&1 | jq -c 'select(.msg=="decision")'
```

deterministic な分布検証：

```bash
RANDOM_SEED=42 go run ./steps/step02-ad-selection/ &
PID=$!
sleep 0.5
for i in $(seq 1 30); do
  curl -s 'http://localhost:8080/ad?strategy=weighted' | jq -r '.id'
done | sort | uniq -c
kill $PID
```

期待: 同じ seed なら同じ分布が出る (再現可能)。

---

## 実験してみよう

- `bid_cpm = 0` の広告を 1 つ入れて、`weighted` で選ばれないことを確認
- `RANDOM_SEED=42` と `RANDOM_SEED=99` で 100 回ずつ叩いて分布の違いを観察
- `Selector` を実装して **eCPM-based** (`bid_cpm × 仮想 CTR × 1000`) のセレクタを足す
- `Selector.Pick` をテストする `_test.go` を書く ([試案](#))
  - `WeightedSelector` を `RANDOM_SEED=0` 固定で 10000 回回し、χ二乗検定っぽく分布を確認
- `/admin/inventory` に Basic 認証をかけてみる

---

## 設計上のメモ

- **「選び方」をロジックではなく interface にする** のは大事。Step 5 で frequency cap が入った時、「frequency-aware なセレクタ」を別実装として並べられる
- decision log を残す習慣は **ML 系の特徴量集めにも直結**。「なぜこの imp はこの ad を出したか」が後から復元できないと、CTR モデルの学習データが作れない
- 乱数を deterministic にできる作りは **テスト時の利益が絶大**。`time.Now().UnixNano()` をそのままぶっこむ "なんとなくランダム" は避ける

---

## 次へ

→ [Step 03 — ターゲティング](../step03-targeting/)
