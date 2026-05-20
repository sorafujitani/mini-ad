# Step 07 — RTB と第二価格オークション

> ローカル在庫に加えて、**外部 DSP に bid request を投げて入札を集める**。

---

## このステップで学ぶこと

- **OpenRTB 風の bid request / bid response** の最小スキーマ
- **タイムアウト付き並列 HTTP リクエスト** (Go の `context` + `goroutine` + `chan`)
- **第二価格オークション** の計算ロジック
- floor price とローカル在庫との混合オークション
- nurl (win notice) のマクロ置換

関連座学: [docs/06-auction.md](../../docs/06-auction.md)

---

## 何を作るか

2 つの HTTP サーバを並走させる：

```
[Browser]
   │
   │ GET /ad?...
   ▼
[mini-ad  :8080]   ← SSP 役 (このリポジトリの主役)
   │
   ├─── HTTP POST /bid ──► [mock-dsp #1 :9001]
   ├─── HTTP POST /bid ──► [mock-dsp #2 :9002]
   └─── HTTP POST /bid ──► [mock-dsp #3 :9003]
   │
   │ 100ms タイムアウト、収集分から second-price で勝者決定
   │
   ▼
[Browser]  ← winning markup
```

ディレクトリ構成：

```
steps/step07-rtb/
├── README.md       (これ)
├── mini-ad/
│   └── main.go     ← SSP 役（README に従って新規作成）
└── mock-dsp/
    └── main.go     ← DSP 役（README に従って新規作成、複数ポートで起動）
```

Step 6 の DB が動いていること（ローカル在庫 + DB 在庫のハイブリッド配信を想定）。

---

## 実装 (mock-dsp)

シンプルなので先にこちらから。`steps/step07-rtb/mock-dsp/main.go` を作成。

### mock-dsp / A. パッケージ宣言と OpenRTB 型

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"net/http"
)

// --- OpenRTB minimal subset ---

type BidRequest struct {
	ID  string         `json:"id"`
	Imp []ImpressionRq `json:"imp"`
	Site Site         `json:"site"`
	Device Device     `json:"device"`
	User User         `json:"user"`
	TMax int          `json:"tmax"`
	AT   int          `json:"at"` // 2 = second price
}

type ImpressionRq struct {
	ID         string `json:"id"`
	Banner     Banner `json:"banner"`
	BidFloor   float64 `json:"bidfloor"`
	BidFloorCur string `json:"bidfloorcur"`
}

type Banner struct{ W, H int }
type Site struct{ Domain, Page string }
type Device struct{ UA, IP string; Geo Geo }
type Geo struct{ Country string }
type User struct{ ID string }

type BidResponse struct {
	ID      string    `json:"id"`
	Seatbid []Seatbid `json:"seatbid,omitempty"`
}

type Seatbid struct{ Bid []Bid `json:"bid"` }

type Bid struct {
	ID    string  `json:"id"`
	ImpID string  `json:"impid"`
	Price float64 `json:"price"`
	ADM   string  `json:"adm"`
	NURL  string  `json:"nurl"`
	LURL  string  `json:"lurl"`
	W, H  int
}
```

### mock-dsp / B. ハンドラ — /bid / /win / /loss

```go
type DSP struct {
	name string
	port int
}

func (d *DSP) handleBid(w http.ResponseWriter, r *http.Request) {
	var req BidRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[%s] bid request id=%s tmax=%d imp=%d", d.name, req.ID, req.TMax, len(req.Imp))

	// 50% で no-bid
	if mrand.Intn(2) == 0 {
		log.Printf("[%s]   → no bid", d.name)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(req.Imp) == 0 {
		http.Error(w, "no imp", http.StatusBadRequest)
		return
	}

	imp := req.Imp[0]
	floor := imp.BidFloor
	if floor < 0.5 {
		floor = 0.5
	}
	// floor の 1.0〜3.0 倍でランダム価格
	price := floor * (1.0 + mrand.Float64()*2.0)
	bidID := newID()

	resp := BidResponse{
		ID: req.ID,
		Seatbid: []Seatbid{{Bid: []Bid{{
			ID:    bidID,
			ImpID: imp.ID,
			Price: price,
			W:     imp.Banner.W,
			H:     imp.Banner.H,
			ADM: fmt.Sprintf(
				`<a href="https://example.com/%s/lp"><img src="https://placehold.co/%dx%d/teal/white?text=%s" width="%d" height="%d"></a>`,
				d.name, imp.Banner.W, imp.Banner.H, d.name, imp.Banner.W, imp.Banner.H,
			),
			NURL: fmt.Sprintf("http://127.0.0.1:%d/win?bid=%s&price=${AUCTION_PRICE}", d.port, bidID),
			LURL: fmt.Sprintf("http://127.0.0.1:%d/loss?bid=%s", d.port, bidID),
		}}}},
	}

	log.Printf("[%s]   → bid id=%s price=%.4f", d.name, bidID, price)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (d *DSP) handleWin(w http.ResponseWriter, r *http.Request) {
	log.Printf("[%s] WIN bid=%s price=%s", d.name, r.URL.Query().Get("bid"), r.URL.Query().Get("price"))
	w.WriteHeader(http.StatusNoContent)
}

func (d *DSP) handleLoss(w http.ResponseWriter, r *http.Request) {
	log.Printf("[%s] LOSS bid=%s", d.name, r.URL.Query().Get("bid"))
	w.WriteHeader(http.StatusNoContent)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

### mock-dsp / C. main

```go
func main() {
	port := flag.Int("port", 9001, "listen port")
	name := flag.String("name", "DSP-A", "dsp name (log用)")
	flag.Parse()

	d := &DSP{name: *name, port: *port}

	mux := http.NewServeMux()
	mux.HandleFunc("/bid", d.handleBid)
	mux.HandleFunc("/win", d.handleWin)
	mux.HandleFunc("/loss", d.handleLoss)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("[%s] listening on %s", *name, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

---

## 実装 (mini-ad)

`steps/step07-rtb/mini-ad/main.go` を作成。Step 06 の在庫機構をそのままに、RTB レイヤーを足す。

簡略化のため Step 06 にあった PostgreSQL は省き、**ローカル在庫 (memory)** に戻す（RTB ロジックの理解を優先）。
RTB と DB の併用は実験課題に回します。

### mini-ad / A. パッケージ宣言

```go
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)
```

`net/url` も `math/rand` も mini-ad では未使用なので入れない。

### mini-ad / B. OpenRTB 型 (mock-dsp と同じ)

mock-dsp と一致させる。Go では別パッケージなので型をコピーする (本来は `pkg/openrtb/` に共有化するが、教材として step ごとの自己完結を優先)。

```go
type BidRequest struct {
	ID     string         `json:"id"`
	Imp    []ImpressionRq `json:"imp"`
	Site   Site           `json:"site"`
	Device Device         `json:"device"`
	User   User           `json:"user"`
	TMax   int            `json:"tmax"`
	AT     int            `json:"at"`
}

type ImpressionRq struct {
	ID          string  `json:"id"`
	Banner      Banner  `json:"banner"`
	BidFloor    float64 `json:"bidfloor"`
	BidFloorCur string  `json:"bidfloorcur"`
}
type Banner struct{ W, H int }
type Site struct{ Domain, Page string }
type Device struct {
	UA  string `json:"ua"`
	IP  string `json:"ip"`
	Geo Geo    `json:"geo"`
}
type Geo struct {
	Country string `json:"country"`
}
type User struct {
	ID string `json:"id"`
}

type BidResponse struct {
	ID      string    `json:"id"`
	Seatbid []Seatbid `json:"seatbid"`
}
type Seatbid struct {
	Bid []Bid `json:"bid"`
}
type Bid struct {
	ID    string  `json:"id"`
	ImpID string  `json:"impid"`
	Price float64 `json:"price"`
	ADM   string  `json:"adm"`
	NURL  string  `json:"nurl"`
	LURL  string  `json:"lurl"`
	W, H  int
}
```

### mini-ad / C. ローカル在庫 (RTB のフロア決定用に最低限)

```go
type LocalLineItem struct {
	ID       string
	Country  string
	BidUSD   float64 // 1 imp あたり USD (CPM ではなく単価で簡略化)
	ImageURL string
	ClickURL string
	W, H     int
}

var localInventory = []LocalLineItem{
	{ID: "local-house", Country: "JP", BidUSD: 1.50, ImageURL: "https://placehold.co/300x250/gray/white?text=HouseAd", ClickURL: "https://example.com/house", W: 300, H: 250},
}
```

「local-house」は **常に応募する house ad** のイメージ。RTB の入札が誰も来なかった場合のフォールバック。

### mini-ad / D. オークションロジック

```go
type AuctionEntry struct {
	Source   string  // "local" or "dsp:NAME"
	Price    float64 // USD per imp
	ADM      string  // markup
	NURL     string  // win notice URL (DSP のみ)
	W, H     int
}

// secondPriceAuction: 価格降順に並べて、1 位を勝者にし
// 支払額 = max(2位の価格, floor) + 0.01
func secondPriceAuction(entries []AuctionEntry, floor float64) (winner AuctionEntry, clearPrice float64, ok bool) {
	if len(entries) == 0 {
		return AuctionEntry{}, 0, false
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Price > entries[j].Price
	})
	winner = entries[0]
	if winner.Price < floor {
		return AuctionEntry{}, 0, false
	}
	second := floor
	if len(entries) >= 2 && entries[1].Price > second {
		second = entries[1].Price
	}
	clearPrice = second + 0.01
	if clearPrice > winner.Price {
		clearPrice = winner.Price
	}
	return winner, clearPrice, true
}
```

### mini-ad / E. DSP に並列で bid request を送る

```go
type DSPClient struct {
	urls    []string // 例: ["http://127.0.0.1:9001", ...]
	timeout time.Duration
	client  *http.Client
}

func newDSPClient(urls []string, timeout time.Duration) *DSPClient {
	return &DSPClient{
		urls:    urls,
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

// CallAll: 全 DSP に bid request を並列 POST し、tmax 内に返ってきた bid だけ集める。
func (c *DSPClient) CallAll(ctx context.Context, req BidRequest) []Bid {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, _ := json.Marshal(req)

	type result struct {
		from string
		bid  *Bid
	}
	resultsCh := make(chan result, len(c.urls))
	var wg sync.WaitGroup

	for _, u := range c.urls {
		wg.Add(1)
		go func(dspURL string) {
			defer wg.Done()
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, dspURL+"/bid", bytes.NewReader(body))
			if err != nil {
				resultsCh <- result{from: dspURL}
				return
			}
			httpReq.Header.Set("Content-Type", "application/json")
			resp, err := c.client.Do(httpReq)
			if err != nil {
				log.Printf("dsp %s error: %v", dspURL, err)
				resultsCh <- result{from: dspURL}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				resultsCh <- result{from: dspURL}
				return
			}
			var br BidResponse
			if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
				resultsCh <- result{from: dspURL}
				return
			}
			if len(br.Seatbid) > 0 && len(br.Seatbid[0].Bid) > 0 {
				b := br.Seatbid[0].Bid[0]
				resultsCh <- result{from: dspURL, bid: &b}
			} else {
				resultsCh <- result{from: dspURL}
			}
		}(u)
	}

	// 全 goroutine の完了 or タイムアウトを待つ
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var bids []Bid
	for r := range resultsCh {
		if r.bid != nil {
			bids = append(bids, *r.bid)
		}
	}
	return bids
}
```

ポイント：
- `context.WithTimeout` で `tmax` 超過時に goroutine が早期撤退
- `http.Client.Timeout` は予備の二重防護
- バッファ付き chan + WaitGroup + close で「全完了 or タイムアウト切り」を綺麗にハンドル

### mini-ad / F. ${AUCTION_PRICE} のマクロ置換と nurl 発火

```go
func fireNURL(client *http.Client, nurl string, clearPrice float64) {
	if nurl == "" {
		return
	}
	final := strings.ReplaceAll(nurl, "${AUCTION_PRICE}", fmt.Sprintf("%.4f", clearPrice))
	go func() {
		req, err := http.NewRequest(http.MethodGet, final, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("nurl error: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
}
```

`go func()` で非同期発火 — ブラウザに返すレスポンスを待たせない。

### mini-ad / G. Server とハンドラ

```go
type Server struct {
	dsp *DSPClient
}

type AdResponse struct {
	ImpID      string  `json:"imp_id"`
	Source     string  `json:"source"`
	Price      float64 `json:"price"`
	ClearPrice float64 `json:"clear_price"`
	ADM        string  `json:"adm"`
	W, H       int
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	country := defaultStr(r.URL.Query().Get("country"), "JP")
	uid := ensureUID(w, r)
	impID := newImpID()

	// 1) ローカル在庫からエントリーを作る (フロアになる候補)
	var entries []AuctionEntry
	var localFloor float64
	for _, li := range localInventory {
		if li.Country != "" && li.Country != country {
			continue
		}
		entries = append(entries, AuctionEntry{
			Source: "local:" + li.ID,
			Price:  li.BidUSD,
			ADM:    fmt.Sprintf(`<a href="%s"><img src="%s" width="%d" height="%d"></a>`, li.ClickURL, li.ImageURL, li.W, li.H),
			W:      li.W, H: li.H,
		})
		if li.BidUSD > localFloor {
			localFloor = li.BidUSD
		}
	}

	// 2) DSP に並列入札を投げる
	bidReq := BidRequest{
		ID: impID,
		Imp: []ImpressionRq{{
			ID:          "1",
			Banner:      Banner{W: 300, H: 250},
			BidFloor:    localFloor, // ローカル最高値をフロアに
			BidFloorCur: "USD",
		}},
		Site:   Site{Domain: "publisher.example.com", Page: "https://publisher.example.com/article/1"},
		Device: Device{UA: r.UserAgent(), IP: clientIP(r), Geo: Geo{Country: country}},
		User:   User{ID: uid},
		TMax:   100,
		AT:     2,
	}
	bids := s.dsp.CallAll(ctx, bidReq)
	for _, b := range bids {
		entries = append(entries, AuctionEntry{
			Source: "dsp",
			Price:  b.Price,
			ADM:    b.ADM,
			NURL:   b.NURL,
			W:      b.W, H: b.H,
		})
	}

	// 3) 第二価格オークション
	winner, clearPrice, ok := secondPriceAuction(entries, localFloor)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 4) DSP が勝ったら win notice を発火
	if strings.HasPrefix(winner.Source, "dsp") {
		fireNURL(http.DefaultClient, winner.NURL, clearPrice)
	}

	resp := AdResponse{
		ImpID:      impID,
		Source:     winner.Source,
		Price:      winner.Price,
		ClearPrice: clearPrice,
		ADM:        winner.ADM,
		W:          winner.W,
		H:          winner.H,
	}
	log.Printf("ad: imp=%s winner=%s price=%.4f clear=%.4f (bids=%d local=%d)",
		impID, winner.Source, winner.Price, clearPrice, len(bids), len(entries)-len(bids))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func clientIP(r *http.Request) string {
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}
func defaultStr(v, fb string) string {
	if v == "" {
		return fb
	}
	return v
}

const uidCookieName = "mini_ad_uid"

func ensureUID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(uidCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	uid := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: uidCookieName, Value: uid, Path: "/", MaxAge: 60 * 60 * 24 * 30, HttpOnly: true})
	return uid
}

func newImpID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

### mini-ad / H. HTML テンプレート

```go
const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8"><title>Step 07 — RTB</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
  .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
  pre { background: #f4f4f4; padding: 8px; }
</style></head><body>
  <h1>Step 07 — RTB</h1>
  <p>local + DSP の入札を集めて second-price でオークション。</p>
  <div class="ad-slot" id="ad-slot"><small>ad slot</small></div>
  <pre id="meta"></pre>
  <script>
    fetch('/ad', { credentials: 'same-origin' })
      .then(r => r.status === 204 ? null : r.json())
      .then(ad => {
        const slot = document.getElementById('ad-slot');
        if (!ad) { slot.innerHTML = '<small>no fill</small>'; return; }
        slot.innerHTML = ad.adm;
        document.getElementById('meta').textContent = JSON.stringify(ad, null, 2);
      });
  </script>
</body></html>
`

func handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ensureUID(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, publisherPageHTML)
}
```

### mini-ad / I. main

```go
func main() {
	dspURLs := strings.Split(os.Getenv("DSP_URLS"), ",")
	// 空要素を除く
	var clean []string
	for _, u := range dspURLs {
		u = strings.TrimSpace(u)
		if u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		log.Println("DSP_URLS not set; defaulting to 9001/9002/9003")
		clean = []string{"http://127.0.0.1:9001", "http://127.0.0.1:9002", "http://127.0.0.1:9003"}
	}

	s := &Server{dsp: newDSPClient(clean, 100*time.Millisecond)}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handlePage)
	mux.HandleFunc("/ad", s.handleAd)

	addr := ":8080"
	log.Printf("[step07 mini-ad] listening on http://localhost%s (dsp=%v)", addr, clean)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

---

## 動作確認

ターミナル 1〜3 (DSP):

```bash
go run ./steps/step07-rtb/mock-dsp/ -port=9001 -name=DSP-A &
go run ./steps/step07-rtb/mock-dsp/ -port=9002 -name=DSP-B &
go run ./steps/step07-rtb/mock-dsp/ -port=9003 -name=DSP-C &
```

ターミナル 4 (mini-ad):

```bash
go run ./steps/step07-rtb/mini-ad/
```

ターミナル 5：

```bash
# 配信
curl -s 'http://localhost:8080/ad?country=JP' | jq

# 何回か叩いて winner の出方を観察
for i in $(seq 1 10); do
  curl -s 'http://localhost:8080/ad?country=JP' | jq -c '{source, price, clear_price}'
done
```

期待：

- 3 DSP のうち入札した中から最高額が勝つ（local が勝つことも、no-bid が多いとあり得る）
- 勝者の `clear_price` が「2 位 + 0.01」になっている
- DSP ターミナル側で `WIN` ログが出る (勝った時のみ)

---

## 実験してみよう

- DSP 側の `mrand.Intn(2) == 0` 確率を変えて入札頻度を変える
- mini-ad の `TMax` を `50ms` / `200ms` に変えて応答率の変化を見る
- DSP の 1 つにわざと `time.Sleep(150ms)` を入れて、タイムアウトされる挙動を確認
- **First-price** に切り替えて配信収益がどう変わるかをシミュレート
- ローカル在庫を **Step 06 の PostgreSQL** に戻して、フロアを動的にする
- DSP の入札に同じユーザーへの "頻度学習" ロジックを足す (同じ uid に対しては 2 回目以降価格を下げる、など)

---

## 設計上のメモ

- 本物の OpenRTB は **schema が巨大** (200 フィールド超)。最初は最小サブセットから始めるのが正解
- 商用 DSP は `tmax 100ms` の中で **ML 推論 + 入札判断** を完了させるため、専用ハードウェアやキャッシュを駆使
- Header Bidding はこれをブラウザ側でやる手法 (cf. [docs/06-auction.md](../../docs/06-auction.md) §Header Bidding)
- 「Second-price は誠実入札最適」だが、SSP がフロアを動的調整すると性質が崩れる ← 業界が First-price 主流に移った遠因

---

## 次へ

→ [Step 08 — ペーシングとレポーティング](../step08-reporting/)
