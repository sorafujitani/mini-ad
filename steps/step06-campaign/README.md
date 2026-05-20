# Step 06 — キャンペーン管理 (PostgreSQL)

> 在庫をメモリから DB に出す。Advertiser / Campaign / LineItem / Creative の階層を作る。

---

## このステップで学ぶこと

- アドサーバの **データモデル中核** = Advertiser / Campaign / LineItem / Creative の 4 階層
- 配信期間・status による **DB レベルでのフィルタリング**
- PostgreSQL の `JSONB` をターゲティング保存に使うパターン
- `pgx` で Go から PostgreSQL を叩く基本
- events を DB の行として保存（Step 04 の NDJSON ログから昇格）

関連座学: [docs/02-terminology.md](../../docs/02-terminology.md) §階層用語

---

## 前提

Step 05 までの概念（トラッキング・uid・frequency cap）を理解していること。在庫とイベントは **メモリから PostgreSQL に移す** ステップです。Redis は Step 6 では必須ではありません（Step 5 の freq を DB 化する拡張は任意課題）。

## 準備

### 1) PostgreSQL を起動

別ターミナルで（初回のみ init が必要）：

```bash
nix run .#postgres-init    # initdb (初回のみ)
nix run .#postgres &       # サーバ起動
nix run .#db-create        # miniad DB + schema 投入 + サンプルデータ
```

`infra/schema.sql` の中身がそのまま投入される。サンプルデータ (Advertiser 2 件 / Campaign 2 件 / LineItem 3 件 / Creative 4 件) も入る。

確認：

```bash
psql -c '\dt'
psql -c 'SELECT id, name, status FROM campaigns;'
psql -c "SELECT id, name, bid_cpm_cents, targeting FROM line_items;"
```

### 2) pgx を追加

```bash
nix develop
go get github.com/jackc/pgx/v5/pgxpool
go get github.com/jackc/pgx/v5
go mod tidy
```

---

## データモデル (再掲)

`infra/schema.sql` を必ず一度開いて読むこと。骨子だけ：

```
advertisers (id, name)
    └── campaigns (id, advertiser_id, name, status, starts_at, ends_at, ...)
            └── line_items (id, campaign_id, name, status, bid_cpm_cents, targeting JSONB, frequency_cap_per_day)
                    └── creatives (id, line_item_id, name, image_url, click_url, width, height, status)
events (id, event_type, creative_id, line_item_id, campaign_id, user_id, occurred_at)
```

ターゲティングは `JSONB` で保存：

```json
{ "countries": ["JP"], "devices": ["mobile"] }
```

---

## 実装

### A. パッケージ宣言とインポート

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)
```

### B. ドメイン型

DB の行から組み立てる「メモリ上の在庫」を表現する型。

```go
type Candidate struct {
	LineItemID      int64
	CampaignID      int64
	BidCPMCents     int64
	FreqCapPerDay   int
	CreativeID      int64
	ImageURL        string
	ClickURL        string
	Width, Height   int
}
```

`Targeting` は JSONB のままでも扱えるが、配信ロジックで使うのは「マッチするかどうか」だけなので、ここでは **SQL 側でマッチ済み** を前提に Go では保持しない。

### C. リクエスト context

```go
type Context struct {
	Country string
	Device  string
}
```

### D. Store (pgx pool ラッパー)

```go
type Store struct {
	pool *pgxpool.Pool
}

func newStore(ctx context.Context) (*Store, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://mini@127.0.0.1:5432/miniad?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &Store{pool: pool}, nil
}
```

### E. 候補抽出クエリ

`infra/schema.sql` の前提を使って、JSONB に対する OR/AND を SQL で表現。

```go
const candidatesQuery = `
SELECT
    li.id, li.campaign_id, li.bid_cpm_cents, li.frequency_cap_per_day,
    cr.id, cr.image_url, cr.click_url, cr.width, cr.height
FROM line_items li
JOIN campaigns c  ON c.id = li.campaign_id
JOIN creatives cr ON cr.line_item_id = li.id
WHERE c.status = 'active'
  AND li.status = 'active'
  AND cr.status = 'active'
  AND c.starts_at <= now() AND c.ends_at >= now()
  AND ( NOT li.targeting ? 'countries' OR li.targeting->'countries' @> $1::jsonb )
  AND ( NOT li.targeting ? 'devices'   OR li.targeting->'devices'   @> $2::jsonb )
`

func (s *Store) Candidates(ctx context.Context, c Context) ([]Candidate, error) {
	countryJSON, _ := json.Marshal([]string{c.Country})
	deviceJSON, _ := json.Marshal([]string{c.Device})

	rows, err := s.pool.Query(ctx, candidatesQuery, string(countryJSON), string(deviceJSON))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(
			&c.LineItemID, &c.CampaignID, &c.BidCPMCents, &c.FreqCapPerDay,
			&c.CreativeID, &c.ImageURL, &c.ClickURL, &c.Width, &c.Height,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
```

ポイント：
- JSONB の `?` 演算子は「キーが存在するか」、`@>` は「包含」
- `NOT li.targeting ? 'countries' OR ...` で「**キーが無ければ素通り、あればマッチを要求**」を表現
- pgx に `[]string` を直接渡すと PostgreSQL の `text[]` になるので、JSONB が欲しい時は `json.Marshal → string` で渡す

### F. event の書き込み

```go
func (s *Store) WriteEvent(ctx context.Context, eventType string, lineItemID, campaignID, creativeID int64, userID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (event_type, line_item_id, campaign_id, creative_id, user_id)
		VALUES ($1, $2, $3, $4, $5)
	`, eventType, lineItemID, campaignID, creativeID, userID)
	return err
}
```

### G. 重み付け選択 (Step 02 から踏襲)

```go
func weightedPick(cands []Candidate) (Candidate, bool) {
	if len(cands) == 0 {
		return Candidate{}, false
	}
	var total int64
	for _, c := range cands {
		if c.BidCPMCents > 0 {
			total += c.BidCPMCents
		}
	}
	if total == 0 {
		return cands[mrand.Intn(len(cands))], true
	}
	r := mrand.Int63n(total)
	var cum int64
	for _, c := range cands {
		if c.BidCPMCents <= 0 {
			continue
		}
		cum += c.BidCPMCents
		if r < cum {
			return c, true
		}
	}
	return cands[len(cands)-1], true
}
```

### H. uid cookie / imp_id / 1x1 GIF (Step 04/05 と同じ)

```go
const uidCookieName = "mini_ad_uid"

func ensureUID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(uidCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	uid := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: uidCookieName, Value: uid, Path: "/", MaxAge: 60 * 60 * 24 * 30, HttpOnly: true,
	})
	return uid
}

func newImpID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00,
	0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02,
	0x44, 0x01, 0x00, 0x3b,
}
```

### I. impSession (in-memory) で imp_id → 行 ID を引けるようにする

`/imp` や `/click` が来た時に「どの line_item / campaign / creative の imp だったか」を引くため、`/ad` を返した時の組み合わせを覚える。

```go
type impInfo struct {
	LineItemID, CampaignID, CreativeID int64
	UserID                              string
	ExpiresAt                           time.Time
}

type ImpStore struct {
	mu sync.Mutex
	m  map[string]impInfo
}

func newImpStore() *ImpStore {
	s := &ImpStore{m: make(map[string]impInfo)}
	go s.gcLoop()
	return s
}

func (s *ImpStore) Put(impID string, info impInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[impID] = info
}

func (s *ImpStore) Get(impID string) (impInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[impID]
	return v, ok
}

func (s *ImpStore) gcLoop() {
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.m {
			if now.After(v.ExpiresAt) {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
}
```

`import "sync"` を A. のインポート群に追加すること。

実運用では Redis に置くか、imp_url のクエリに必要情報を埋め込んで stateless にする。学習用は in-memory で十分。

### J. Server とハンドラ

```go
type Server struct {
	store *Store
	imps  *ImpStore
}

type AdResponse struct {
	LineItemID  int64  `json:"line_item_id"`
	CampaignID  int64  `json:"campaign_id"`
	CreativeID  int64  `json:"creative_id"`
	ImpID       string `json:"imp_id"`
	UID         string `json:"uid"`
	BidCPMCents int64  `json:"bid_cpm_cents"`
	Image       string `json:"image_url"`
	ClickURL    string `json:"click_url"`
	ImpURL      string `json:"imp_url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := ensureUID(w, r)
	reqCtx := Context{
		Country: defaultStr(r.URL.Query().Get("country"), "JP"),
		Device:  defaultStr(r.URL.Query().Get("device"), "mobile"),
	}

	cands, err := s.store.Candidates(ctx, reqCtx)
	if err != nil {
		http.Error(w, "candidates: "+err.Error(), http.StatusInternalServerError)
		return
	}

	winner, ok := weightedPick(cands)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	impID := newImpID()
	s.imps.Put(impID, impInfo{
		LineItemID: winner.LineItemID,
		CampaignID: winner.CampaignID,
		CreativeID: winner.CreativeID,
		UserID:     uid,
		ExpiresAt:  time.Now().Add(10 * time.Minute),
	})

	resp := AdResponse{
		LineItemID:  winner.LineItemID,
		CampaignID:  winner.CampaignID,
		CreativeID:  winner.CreativeID,
		ImpID:       impID,
		UID:         uid,
		BidCPMCents: winner.BidCPMCents,
		Image:       winner.ImageURL,
		ClickURL:    fmt.Sprintf("/click?imp_id=%s&dest=%s", impID, url.QueryEscape(winner.ClickURL)),
		ImpURL:      fmt.Sprintf("/imp?imp_id=%s", impID),
		Width:       winner.Width,
		Height:      winner.Height,
	}

	log.Printf("ad: uid=%s imp_id=%s line_item=%d campaign=%d", uid, impID, winner.LineItemID, winner.CampaignID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleImpression(w http.ResponseWriter, r *http.Request) {
	impID := r.URL.Query().Get("imp_id")
	info, ok := s.imps.Get(impID)
	if !ok {
		log.Printf("imp: unknown imp_id=%s", impID)
	} else {
		if err := s.store.WriteEvent(r.Context(), "impression",
			info.LineItemID, info.CampaignID, info.CreativeID, info.UserID); err != nil {
			log.Printf("write impression: %v", err)
		}
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(transparentGIF)
}

func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	impID := r.URL.Query().Get("imp_id")
	dest := r.URL.Query().Get("dest")
	if !strings.HasPrefix(dest, "https://") && !strings.HasPrefix(dest, "http://") {
		http.Error(w, "invalid dest", http.StatusBadRequest)
		return
	}
	info, ok := s.imps.Get(impID)
	if ok {
		if err := s.store.WriteEvent(r.Context(), "click",
			info.LineItemID, info.CampaignID, info.CreativeID, info.UserID); err != nil {
			log.Printf("write click: %v", err)
		}
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func defaultStr(v, fb string) string {
	if v == "" {
		return fb
	}
	return v
}
```

### K. HTML テンプレート

```go
const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8"><title>Step 06 — Campaign DB</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
  .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
</style></head><body>
  <h1>Step 06 — Campaign DB</h1>
  <p>在庫は PostgreSQL から取得。</p>
  <div class="ad-slot" id="ad-slot"><small>ad slot</small></div>
  <script>
    fetch('/ad?country=JP&device=mobile', { credentials: 'same-origin' })
      .then(r => r.status === 204 ? null : r.json())
      .then(ad => {
        const slot = document.getElementById('ad-slot');
        if (!ad) { slot.innerHTML = '<small>no fill</small>'; return; }
        slot.innerHTML =
          '<small>campaign=' + ad.campaign_id + ' line_item=' + ad.line_item_id + ' creative=' + ad.creative_id + '</small><br>' +
          '<a href="' + ad.click_url + '">' +
          '<img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '"></a>' +
          '<img src="' + ad.imp_url + '" width="1" height="1" style="display:none">';
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

### L. main

```go
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := newStore(ctx)
	if err != nil {
		log.Fatalf("postgres: %v\n  → 別ターミナルで `nix run .#postgres-init && nix run .#postgres && nix run .#db-create` を実行してください", err)
	}
	s := &Server{store: store, imps: newImpStore()}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handlePage)
	mux.HandleFunc("/ad", s.handleAd)
	mux.HandleFunc("/imp", s.handleImpression)
	mux.HandleFunc("/click", s.handleClick)

	addr := ":8080"
	log.Printf("[step06] listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

---

## 動作確認

```bash
go run ./steps/step06-campaign/

# 配信
curl -s 'http://localhost:8080/ad?country=JP&device=mobile' | jq

# imp を計上
curl -s "http://localhost:8080$(curl -s 'http://localhost:8080/ad?country=JP&device=mobile' | jq -r .imp_url)" -o /dev/null

# events テーブルに書き込まれていく
psql -c "SELECT event_type, line_item_id, count(*) FROM events GROUP BY 1, 2 ORDER BY 1, 2;"

# 期間外にして配信されないことを確認
psql -c "UPDATE campaigns SET ends_at = now() - interval '1 day' WHERE id = 1;"
curl -i 'http://localhost:8080/ad?country=JP&device=mobile' 2>&1 | head -1

# 元に戻す
psql -c "UPDATE campaigns SET ends_at = now() + interval '30 days' WHERE id = 1;"
```

---

## 実験してみよう

- 自分で `INSERT INTO advertisers / campaigns / line_items / creatives` を叩いてみて、配信されることを確認
- `JSONB` に `keywords` フィールドを足し、URL クエリ `?kw=coffee` でフィルタするように拡張
- 同じ LineItem に複数 Creative を入れて、A/B テストっぽくランダムローテにする
- `pgxpool` の MaxConns を変えて負荷時の挙動を観察 (`pgx_max_conns_per_user` の警告を見る)

---

## 設計上のメモ

- 商用システムは **「DB は管理画面用、配信用は別ストア (in-memory snapshot)」** がよくある。配信時に毎回 SQL を叩くと遅すぎるため
- 学習用は毎リクエスト SQL でも OK。本番では `n` 秒ごとに DB → in-memory にロードする
- imp/click ハンドラの `INSERT` も hotpath で重いので、本番は `batch insert` か Kafka 経由で別プロセス集約が一般的

---

## 次へ

→ [Step 07 — RTB とオークション](../step07-rtb/)
