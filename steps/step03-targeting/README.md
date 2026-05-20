# Step 03 — ターゲティング

> 「全員に同じ広告」から「条件にマッチする人だけ」へ。**LineItem 階層** + **include/exclude マッチング** + **UA パース**。

---

## このステップで学ぶこと

- **データモデルの進化**: `Ad 単体` → `LineItem (Targeting + bid + Creative)` 階層
- マッチング規則
  - 同じフィールドの中は **OR**
  - フィールド間は **AND**
  - **空 = 制約なし**、**include / exclude** で意味を持たせる
- 複数の targeting 次元
  - country (geo)
  - device (mobile / desktop / tablet)
  - os (iOS / Android / Windows / macOS / Linux)
  - browser (Chrome / Safari / Firefox / Edge)
  - day_of_week (mon..sun) — day parting の入り口
- リクエスト Context の **段階的構築**
  - query param 最優先 (テスト用)
  - 無ければ UA / IP / 時刻から自動推定
- **UA パース** を標準ライブラリだけで書く

関連座学: [docs/04-targeting.md](../../docs/04-targeting.md)

---

## 前提

Step 02 で組んだ Selector / Decision / middleware 群はそのまま継承。
ファイルが膨らんできたので、**機能ごとに別 .go ファイルに分割** します（Go の慣習：同一パッケージ内なら自由に分割可能）。

```
steps/step03-targeting/
├── main.go        ← config + main + graceful shutdown
├── middleware.go  ← Chain / RequestID / AccessLog / Recover
├── domain.go      ← Ad / LineItem / Inventory
├── targeting.go   ← Targeting / Context / Matcher
├── ua.go          ← UA パース
├── selector.go    ← Selector & 3 実装
└── server.go      ← Server + handlers + HTML template
```

複数ファイルでも `package main` のままで OK。

---

## 何を作るか

| Method & Path | 役割 |
|---------------|------|
| `GET /` | country / device / os / browser を切替できる UI |
| `GET /ad?slot=...&country=JP&device=mobile&os=ios` | マッチした LineItem から選定 |
| `GET /ad?...` (param 無し) | UA / 時刻から自動推定 |
| `GET /admin/inventory` | LineItem の targeting 込みで JSON 表示 |
| `GET /healthz` | (同) |

該当広告が無ければ **HTTP 204 No Content**。

---

## 実装

### A. `domain.go` — LineItem 階層へ拡張

```go
package main

type Creative struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	ClickURL string `json:"click_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type SlotID string

const (
	SlotMainRectangle  SlotID = "main-rectangle"
	SlotTopBanner      SlotID = "top-banner"
	SlotSideSkyscraper SlotID = "side-skyscraper"
)

// LineItem: ターゲティング + 入札額 + Creative を 1 つにまとめた配信単位。
type LineItem struct {
	ID        string    `json:"id"`
	Slot      SlotID    `json:"slot"`
	Targeting Targeting `json:"targeting"`
	BidCPM    int       `json:"bid_cpm"`
	Creative  Creative  `json:"creative"`
}

// Inventory: フラットな LineItem の集合。
// 取り出すときは Slot で絞って LineItem ごとに Targeting マッチを評価する。
type Inventory []LineItem

// BySlot: slot で絞り込み (フィルタ前段)
func (inv Inventory) BySlot(slot SlotID) []LineItem {
	out := make([]LineItem, 0, len(inv))
	for _, li := range inv {
		if li.Slot == slot {
			out = append(out, li)
		}
	}
	return out
}

func defaultInventory() Inventory {
	return Inventory{
		{
			ID:   "li-acme-jp-mobile",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
				Devices:   Include([]string{"mobile"}),
			},
			BidCPM: 300,
			Creative: Creative{ID: "cr-1", Title: "Acme JP モバイル", ImageURL: "https://placehold.co/300x250/orange/white?text=Acme+JP+mobile", ClickURL: "https://example.com/acme/jp", Width: 300, Height: 250},
		},
		{
			ID:   "li-acme-jp-desktop",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
				Devices:   Include([]string{"desktop"}),
				OS:        Include([]string{"macOS", "Windows", "Linux"}),
			},
			BidCPM: 250,
			Creative: Creative{ID: "cr-2", Title: "Acme JP デスクトップ", ImageURL: "https://placehold.co/300x250/orange/white?text=Acme+JP+desktop", ClickURL: "https://example.com/acme/jp", Width: 300, Height: 250},
		},
		{
			ID:   "li-globex-jp-weekend",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries:  Include([]string{"JP"}),
				DayOfWeek:  Include([]string{"sat", "sun"}),
				Browsers:   Exclude([]string{"firefox"}), // Firefox は除外
			},
			BidCPM: 220,
			Creative: Creative{ID: "cr-3", Title: "Globex JP 週末", ImageURL: "https://placehold.co/300x250/brown/white?text=Globex+JP+weekend", ClickURL: "https://example.com/globex/jp", Width: 300, Height: 250},
		},
		{
			ID:   "li-us-any",
			Slot: SlotMainRectangle,
			Targeting: Targeting{
				Countries: Include([]string{"US"}),
			},
			BidCPM: 200,
			Creative: Creative{ID: "cr-4", Title: "Globex US", ImageURL: "https://placehold.co/300x250/brown/white?text=Globex+US", ClickURL: "https://example.com/globex/us", Width: 300, Height: 250},
		},
		{
			ID:   "li-house-any",
			Slot: SlotMainRectangle,
			// targeting なし → 全マッチ (house ad / フォールバック)
			BidCPM:   10,
			Creative: Creative{ID: "cr-house", Title: "House Ad", ImageURL: "https://placehold.co/300x250/gray/white?text=House+Ad", ClickURL: "https://example.com/house", Width: 300, Height: 250},
		},
		{
			ID:   "li-banner-acme",
			Slot: SlotTopBanner,
			Targeting: Targeting{
				Countries: Include([]string{"JP"}),
			},
			BidCPM:   180,
			Creative: Creative{ID: "cr-banner-acme", Title: "Acme Banner", ImageURL: "https://placehold.co/728x90/orange/white?text=Acme+Banner", ClickURL: "https://example.com/acme/banner", Width: 728, Height: 90},
		},
	}
}
```

### B. `targeting.go` — Targeting / Context / Matcher

include/exclude を **「同じ表現で扱える」フィールド型** にして、Targeting 全体を綺麗にする。

```go
package main

import (
	"strings"
	"time"
)

// StringSet は「include / exclude のいずれか」を表現するターゲティングフィールド。
//   Mode=""        → 制約なし (常にマッチ)
//   Mode="include" → Values のいずれかに一致が必要
//   Mode="exclude" → Values のいずれにも一致しないことが必要
type StringSet struct {
	Mode   string   `json:"mode,omitempty"`
	Values []string `json:"values,omitempty"`
}

func Include(v []string) StringSet { return StringSet{Mode: "include", Values: v} }
func Exclude(v []string) StringSet { return StringSet{Mode: "exclude", Values: v} }

// Match: v が StringSet にマッチするか。
//   - Mode が空 (= 制約なし) → 常に true
//   - Mode=include → values に v が含まれれば true
//   - Mode=exclude → values に v が含まれていなければ true
// 比較は case-insensitive (UA から拾った値が大小混在になりがちなので)
func (s StringSet) Match(v string) bool {
	if s.Mode == "" || len(s.Values) == 0 {
		return true
	}
	v = strings.ToLower(v)
	hit := false
	for _, x := range s.Values {
		if strings.ToLower(x) == v {
			hit = true
			break
		}
	}
	switch s.Mode {
	case "include":
		return hit
	case "exclude":
		return !hit
	default:
		return true
	}
}

// Targeting: 1 つの LineItem が持つマッチング条件。
// すべての非空フィールドが AND で評価される。
type Targeting struct {
	Countries StringSet `json:"countries,omitempty"`
	Devices   StringSet `json:"devices,omitempty"`
	OS        StringSet `json:"os,omitempty"`
	Browsers  StringSet `json:"browsers,omitempty"`
	DayOfWeek StringSet `json:"day_of_week,omitempty"` // "mon","tue",...,"sun"
}

// Context: 1 リクエストから抽出した属性値。
type Context struct {
	Country   string `json:"country,omitempty"`
	Device    string `json:"device,omitempty"`
	OS        string `json:"os,omitempty"`
	Browser   string `json:"browser,omitempty"`
	DayOfWeek string `json:"day_of_week,omitempty"`
}

// dayOfWeek: 現在時刻 (UTC) から月..日 を返す
func dayOfWeekUTC(now time.Time) string {
	return []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}[now.UTC().Weekday()]
}

// Matches: targeting がすべて通れば true。
func (t Targeting) Matches(c Context) bool {
	return t.Countries.Match(c.Country) &&
		t.Devices.Match(c.Device) &&
		t.OS.Match(c.OS) &&
		t.Browsers.Match(c.Browser) &&
		t.DayOfWeek.Match(c.DayOfWeek)
}
```

ポイント：
- **`StringSet` という小さな抽象** を入れただけで、include / exclude を Targeting の全フィールドで使い回せるようになる
- `Match` は case-insensitive にしておく — UA から拾うと "Chrome" / "chrome" / "CHROME" が混在しがち
- `Matches` は **全フィールド AND** の単純な合成

### C. `ua.go` — User-Agent パーサ (標準ライブラリのみ)

ライブラリを入れずに **substring match の組み合わせ**で device / os / browser を判定する。本物の `uap-go` ほど正確ではないが、教材としては十分。

```go
package main

import "strings"

// ParseUA は User-Agent から (device, os, browser) を推定する。
// 不明な場合は空文字を返す。
func ParseUA(ua string) (device, osName, browser string) {
	ua = strings.ToLower(ua)

	// === OS === (順序に注意: ipad は ipad → ipados、その他より先)
	switch {
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"):
		osName = "iOS"
	case strings.Contains(ua, "android"):
		osName = "Android"
	case strings.Contains(ua, "mac os x"), strings.Contains(ua, "macintosh"):
		osName = "macOS"
	case strings.Contains(ua, "windows"):
		osName = "Windows"
	case strings.Contains(ua, "linux"):
		osName = "Linux"
	}

	// === device ===
	switch {
	case strings.Contains(ua, "ipad"), strings.Contains(ua, "tablet"):
		device = "tablet"
	case strings.Contains(ua, "mobile"),
		strings.Contains(ua, "iphone"),
		strings.Contains(ua, "android"):
		// 「mobile」キーワードを含む Android は tablet ではない (公式運用)
		device = "mobile"
	default:
		device = "desktop"
	}

	// === browser === (順序が大事: Edge > Chrome、Safari は Chrome 除外後)
	switch {
	case strings.Contains(ua, "edg/"), strings.Contains(ua, "edge/"):
		browser = "Edge"
	case strings.Contains(ua, "opr/"), strings.Contains(ua, "opera"):
		browser = "Opera"
	case strings.Contains(ua, "firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "safari/"):
		browser = "Safari"
	}

	return
}
```

ポイント：
- 「**判定順序が結果を左右する**」のが UA パースの怖いところ。Edge は UA に "Chrome" を含むので Edge を先にチェック
- iPad は **macOS Safari と区別がつかない時代** (iPadOS 13+) があるが、ここでは無視
- まず "lower 化して substring match" で 99% 賄えるという感覚を掴むのが目的

### D. `selector.go` — Step 02 と同じ

Step 02 から `Selector`, `RandomSelector`, `WeightedSelector`, `HighestBidSelector`, `SelectorRegistry` をそのままコピー。
ただし `Pick(ads []Ad)` を `Pick(items []LineItem)` に置き換える必要があるので、シグネチャをまとめて変更：

```go
package main

import (
	mrand "math/rand"
	"sync"
)

type Selector interface {
	Name() string
	Pick(items []LineItem) (LineItem, bool)
}

type RandomSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex
}

func NewRandomSelector(rng *mrand.Rand) *RandomSelector { return &RandomSelector{rng: rng} }
func (RandomSelector) Name() string                     { return "random" } // 注: pointer receiver にしないとここでエラー
```

修正版：

```go
package main

import (
	mrand "math/rand"
	"sync"
)

type Selector interface {
	Name() string
	Pick(items []LineItem) (LineItem, bool)
}

// --- random ---
type RandomSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex
}

func NewRandomSelector(rng *mrand.Rand) *RandomSelector { return &RandomSelector{rng: rng} }
func (s *RandomSelector) Name() string                  { return "random" }
func (s *RandomSelector) Pick(items []LineItem) (LineItem, bool) {
	if len(items) == 0 {
		return LineItem{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return items[s.rng.Intn(len(items))], true
}

// --- weighted ---
type WeightedSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex
}

func NewWeightedSelector(rng *mrand.Rand) *WeightedSelector { return &WeightedSelector{rng: rng} }
func (s *WeightedSelector) Name() string                    { return "weighted" }
func (s *WeightedSelector) Pick(items []LineItem) (LineItem, bool) {
	if len(items) == 0 {
		return LineItem{}, false
	}
	total := 0
	for _, li := range items {
		if li.BidCPM > 0 {
			total += li.BidCPM
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if total == 0 {
		return items[s.rng.Intn(len(items))], true
	}
	r := s.rng.Intn(total)
	cum := 0
	for _, li := range items {
		if li.BidCPM <= 0 {
			continue
		}
		cum += li.BidCPM
		if r < cum {
			return li, true
		}
	}
	return items[len(items)-1], true
}

// --- highest ---
type HighestBidSelector struct{}

func (HighestBidSelector) Name() string { return "highest" }
func (HighestBidSelector) Pick(items []LineItem) (LineItem, bool) {
	if len(items) == 0 {
		return LineItem{}, false
	}
	best := items[0]
	for _, li := range items[1:] {
		if li.BidCPM > best.BidCPM {
			best = li
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
```

### E. `server.go` — Context 構築 + 配信パイプライン

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

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

// buildContext: query param が指定されていれば最優先、無ければ UA / 時刻から推定。
func buildContext(r *http.Request, now time.Time) Context {
	q := r.URL.Query()
	device, osName, browser := ParseUA(r.UserAgent())

	c := Context{
		Country:   defaultStr(q.Get("country"), "JP"),
		Device:    defaultStr(q.Get("device"), device),
		OS:        defaultStr(q.Get("os"), osName),
		Browser:   defaultStr(q.Get("browser"), browser),
		DayOfWeek: defaultStr(q.Get("day"), dayOfWeekUTC(now)),
	}
	return c
}

func defaultStr(v, fb string) string {
	if v == "" {
		return fb
	}
	return v
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slot := SlotID(defaultStr(q.Get("slot"), string(SlotMainRectangle)))
	selector := s.selectors.Resolve(q.Get("strategy"))
	rctx := buildContext(r, time.Now())

	// 1) slot で絞る
	allInSlot := s.inventory.BySlot(slot)
	// 2) targeting マッチで絞る
	candidates := filterByTargeting(allInSlot, rctx)
	// 3) 残りから 1 つ選ぶ
	winner, ok := selector.Pick(candidates)

	dec := Decision{
		RequestID:  GetRequestID(r.Context()),
		Slot:       slot,
		Context:    rctx,
		Strategy:   selector.Name(),
		Total:      len(allInSlot),
		Candidates: len(candidates),
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

	type AdResponse struct {
		LineItemID string   `json:"line_item_id"`
		BidCPM     int      `json:"bid_cpm"`
		Slot       SlotID   `json:"slot"`
		Context    Context  `json:"context"`
		Creative   Creative `json:"creative"`
	}
	writeJSON(w, http.StatusOK, AdResponse{
		LineItemID: winner.ID,
		BidCPM:     winner.BidCPM,
		Slot:       slot,
		Context:    rctx,
		Creative:   winner.Creative,
	})
}

func filterByTargeting(items []LineItem, c Context) []LineItem {
	out := make([]LineItem, 0, len(items))
	for _, li := range items {
		if li.Targeting.Matches(c) {
			out = append(out, li)
		}
	}
	return out
}

// === decision log ===

type Decision struct {
	RequestID    string  `json:"request_id"`
	Slot         SlotID  `json:"slot"`
	Context      Context `json:"context"`
	Strategy     string  `json:"strategy"`
	Total        int     `json:"total"`      // slot 内の全 LineItem 数
	Candidates   int     `json:"candidates"` // targeting 通過後の数
	WinnerID     string  `json:"winner_id,omitempty"`
	WinnerBidCPM int     `json:"winner_bid_cpm,omitempty"`
	NoFill       bool    `json:"no_fill,omitempty"`
}

func (s *Server) logDecision(ctx context.Context, d Decision) {
	s.logger.InfoContext(ctx, "decision",
		slog.String("req_id", d.RequestID),
		slog.String("slot", string(d.Slot)),
		slog.Any("ctx", d.Context),
		slog.String("strategy", d.Strategy),
		slog.Int("total", d.Total),
		slog.Int("candidates", d.Candidates),
		slog.String("winner_id", d.WinnerID),
		slog.Int("winner_bid_cpm", d.WinnerBidCPM),
		slog.Bool("no_fill", d.NoFill),
	)
}

// === HTML ===

const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <title>Step 03 — Targeting</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 800px; margin: 40px auto; padding: 0 16px; }
    .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
    .controls > * { margin-right: 8px; }
    .meta { font-size: 11px; color: #999; }
    pre { background: #f4f4f4; padding: 8px; font-size: 11px; }
  </style>
</head>
<body>
  <h1>Step 03 — Targeting</h1>

  <div class="controls">
    country <select id="country"><option value="">(auto)</option><option>JP</option><option>US</option><option>KR</option></select>
    device  <select id="device"><option value="">(auto)</option><option>mobile</option><option>desktop</option><option>tablet</option></select>
    os      <select id="os"><option value="">(auto)</option><option>iOS</option><option>Android</option><option>macOS</option><option>Windows</option><option>Linux</option></select>
    browser <select id="browser"><option value="">(auto)</option><option>Chrome</option><option>Safari</option><option>Firefox</option><option>Edge</option></select>
    day     <select id="day"><option value="">(auto)</option><option>mon</option><option>tue</option><option>wed</option><option>thu</option><option>fri</option><option>sat</option><option>sun</option></select>
    <button onclick="loadAll()">request</button>
  </div>

  <div class="ad-slot" data-slot="top-banner"><small>top-banner</small></div>
  <div class="ad-slot" data-slot="main-rectangle"><small>main-rectangle</small></div>

  <pre id="meta">resolved context will show here</pre>

  <script>
    function qstr() {
      const ids = ['country','device','os','browser','day'];
      return ids.map(id => {
        const v = document.getElementById(id).value;
        return v ? id + '=' + encodeURIComponent(v) : '';
      }).filter(Boolean).join('&');
    }
    function loadOne(slot) {
      const el = document.querySelector('[data-slot="' + slot + '"]');
      const q = qstr();
      fetch('/ad?slot=' + slot + (q ? '&' + q : ''))
        .then(r => r.status === 204 ? null : r.json())
        .then(data => {
          if (!data) { el.innerHTML = '<small>no fill (' + slot + ')</small>'; return; }
          const ad = data.creative;
          el.innerHTML =
            '<a href="' + ad.click_url + '"><img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '"></a>' +
            '<div class="meta">' + data.line_item_id + ' / bid=' + data.bid_cpm + '</div>';
          document.getElementById('meta').textContent = JSON.stringify(data.context, null, 2);
        });
    }
    function loadAll() {
      document.querySelectorAll('.ad-slot').forEach(el => loadOne(el.dataset.slot));
    }
    loadAll();
  </script>
</body>
</html>
`
```

### F. `middleware.go` — Step 01-02 と同じ

Step 01 の middleware.go (Chain / RequestID / AccessLog / Recover / statusRecorder / GetRequestID / ctxKey) をそのままコピー。

### G. `main.go` — Step 02 とほぼ同じ

config 読み込み + slog + Server 組み立て + graceful shutdown。
Step 02 と異なる点：

- `s := newServer(logger, defaultInventory(), selectors)` の `defaultInventory()` が `Inventory` (slice of LineItem) を返すように変わった

Step 02 の main.go をベースに `defaultInventory()` の型変更を反映するだけ。

---

## 動作確認

```bash
go run ./steps/step03-targeting/

# country=JP / device=mobile / os=iOS で叩く
curl -s 'http://localhost:8080/ad?country=JP&device=mobile&os=iOS' | jq

# Firefox 除外ターゲティングの確認 (li-globex-jp-weekend)
curl -s 'http://localhost:8080/ad?country=JP&device=desktop&os=macOS&browser=Firefox&day=sat' | jq -r '.line_item_id'
# → Firefox なので li-globex-jp-weekend は除外、別の LineItem が返る

curl -s 'http://localhost:8080/ad?country=JP&device=desktop&os=macOS&browser=Chrome&day=sat' | jq -r '.line_item_id'
# → li-globex-jp-weekend が候補に入る

# 自動推定 (UA 渡し)
curl -s -A 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0)' 'http://localhost:8080/ad' | jq '.context'
# → device=mobile, os=iOS, browser=Safari

# どれにもマッチしない条件
curl -i 'http://localhost:8080/ad?country=KR&device=tablet' 2>&1 | head -1   # 200 (house ad にフォールバック)

# 在庫を覗く
curl -s http://localhost:8080/admin/inventory | jq '.[].targeting'
```

decision log を tail：

```bash
go run ./steps/step03-targeting/ 2>&1 | jq -c 'select(.msg=="decision")'
```

---

## 実験してみよう

- 自分で `LineItem` を追加: `Countries: Include([]string{"JP"})`, `OS: Exclude([]string{"iOS"})` の組み合わせで挙動を確認
- `Targeting` に `Keywords StringSet` を追加し、`Context.Keywords []string` で URL クエリの記事キーワードと突合させる
- `StringSet.Match` を **`_test.go`** で table-driven test する
  ```go
  cases := []struct{
    name string
    set  StringSet
    in   string
    want bool
  }{
    {"empty matches all", StringSet{}, "JP", true},
    {"include hit",       Include([]string{"JP"}), "JP", true},
    {"include miss",      Include([]string{"JP"}), "US", false},
    {"exclude hit",       Exclude([]string{"JP"}), "JP", false},
    {"exclude miss",      Exclude([]string{"JP"}), "US", true},
    {"case insensitive",  Include([]string{"jp"}), "JP", true},
  }
  ```
- UA パーサに `iPadOS 13+` の対応を追加 (Mac UA と区別するための `MaxTouchPoints` などのヒント — クライアント JS から送る運用に)
- `day_of_week` を JST 基準にするには？ → `time.LoadLocation("Asia/Tokyo")` で計算

---

## 設計上のメモ

- **`StringSet` を一段挟むことで、すべての targeting フィールドが include/exclude を持てる** — これがないと、フィールドごとに 2 つフィールド (CountriesInclude / CountriesExclude) を作る羽目になる
- ターゲティングは LineItem が持つ。Creative には付けない (同じ素材を異なる targeting で配信できる柔軟性)
- 商用システムは **ターゲティング評価エンジン** を持ち、数百万 LineItem を ms 単位で評価。bitmap index や inverted index で実装される
- UA パースは年々厳しくなっていて、Google は **UA Client Hints** (`Sec-CH-UA-*` ヘッダ) を推進中

---

## 次へ

→ [Step 04 — トラッキング](../step04-tracking/)
