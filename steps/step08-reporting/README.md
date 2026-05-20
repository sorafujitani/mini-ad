# Step 08 — ペーシングとレポーティング

> 「予算を均等に使う」「使った分を見える化する」。最終ステップ。

---

## このステップで学ぶこと

- **予算チェック** を配信判定に組み込む (`daily_budget` / `total_budget`)
- **Even Pacing** — 1 日を通じて予算を均等に消化する
- **Probabilistic Pacing** — 確率的判定で滑らかにスロットリング
- **集計レポート** — 配信実績を campaign / line_item 単位で見る
- 簡易ダッシュボード (HTML テーブル) の出し方

関連座学: [docs/07-pacing-budget.md](../../docs/07-pacing-budget.md)

---

## 前提

Step 06 (PostgreSQL) のデータベースとスキーマを使います。`events` テーブルに少しデータがあるとレポートが見やすい。

事前に Step 06 を回してデータを溜めるか、テスト用に手で INSERT してもよい：

```bash
psql <<'SQL'
INSERT INTO events (event_type, line_item_id, campaign_id, creative_id, user_id, occurred_at)
SELECT (ARRAY['impression','impression','impression','click'])[1 + (n % 4)],
       (ARRAY[1,1,2,3])[1 + (n % 4)],
       (ARRAY[1,1,1,2])[1 + (n % 4)],
       (ARRAY[1,2,3,4])[1 + (n % 4)],
       'uid-' || (n % 50),
       now() - (interval '1 minute' * (300 - n))
FROM generate_series(1, 300) AS n;
SQL
```

---

## 何を作るか

新しいサーバ。`steps/step08-reporting/main.go` を作成。

| Method & Path | 役割 |
|---------------|------|
| `GET /ad?country=...` | 予算チェック + Even Pacing 適用の配信 |
| `GET /imp` | impression 計上 |
| `GET /click` | click 計上 |
| `GET /report` | HTML ダッシュボード |
| `GET /api/report?date=YYYY-MM-DD` | JSON 集計レポート |

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
	"html/template"
	"log"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)
```

### B. Store と接続 (Step 06 から踏襲)

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
	return &Store{pool: pool}, pool.Ping(ctx)
}
```

### C. 候補抽出 SQL に予算チェックを足す

「**今日 (UTC date)** の impression × bid_cpm の和」を当日消化額とし、`daily_budget_cents` 未満のみ残す。

```go
const candidatesQuery = `
WITH today_spent AS (
    SELECT c.id AS campaign_id,
           COALESCE(SUM(li.bid_cpm_cents) FILTER (WHERE e.event_type = 'impression'), 0)::BIGINT / 1000 AS spent_cents
    FROM campaigns c
    LEFT JOIN events e
      ON e.campaign_id = c.id
     AND e.occurred_at >= date_trunc('day', now() AT TIME ZONE 'UTC')
    LEFT JOIN line_items li ON li.id = e.line_item_id
    GROUP BY c.id
)
SELECT
    li.id, li.campaign_id, li.bid_cpm_cents, li.frequency_cap_per_day,
    cr.id, cr.image_url, cr.click_url, cr.width, cr.height,
    c.daily_budget_cents, COALESCE(ts.spent_cents, 0) AS spent_today
FROM line_items li
JOIN campaigns c   ON c.id = li.campaign_id
JOIN creatives cr  ON cr.line_item_id = li.id
LEFT JOIN today_spent ts ON ts.campaign_id = c.id
WHERE c.status = 'active'
  AND li.status = 'active'
  AND cr.status = 'active'
  AND c.starts_at <= now() AND c.ends_at >= now()
  AND ( NOT li.targeting ? 'countries' OR li.targeting->'countries' @> $1::jsonb )
  AND ( NOT li.targeting ? 'devices'   OR li.targeting->'devices'   @> $2::jsonb )
  AND ( c.daily_budget_cents = 0 OR COALESCE(ts.spent_cents, 0) < c.daily_budget_cents )
`

type Candidate struct {
	LineItemID, CampaignID, CreativeID int64
	BidCPMCents                        int64
	FreqCapPerDay                      int
	ImageURL, ClickURL                 string
	Width, Height                      int
	DailyBudgetCents                   int64
	SpentTodayCents                    int64
}

func (s *Store) Candidates(ctx context.Context, country, device string) ([]Candidate, error) {
	cj, _ := json.Marshal([]string{country})
	dj, _ := json.Marshal([]string{device})
	rows, err := s.pool.Query(ctx, candidatesQuery, string(cj), string(dj))
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
			&c.DailyBudgetCents, &c.SpentTodayCents,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
```

### D. Even Pacing

「期待消化 = `daily_budget × (経過秒 / 86400)`」と「実消化 = `spent_today`」を比べて確率的にスキップ。

```go
// pacingPasses: pacing が許可するなら true を返す。
//   daily_budget_cents = 0  → 制限なし、常に true
//   ratio = expected / max(actual, 1)
//   ratio >= 1 → 全力 (true)
//   ratio <  1 → rand() < ratio で確率配信
func pacingPasses(dailyBudget, spentToday int64) bool {
	if dailyBudget <= 0 {
		return true
	}
	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	elapsed := now.Sub(startOfDay).Seconds()
	if elapsed <= 0 {
		return true
	}
	expected := float64(dailyBudget) * elapsed / 86400.0
	if expected <= 0 {
		return true
	}
	actual := float64(spentToday)
	if actual <= 0 {
		return true
	}
	ratio := expected / actual
	if ratio >= 1.0 {
		return true
	}
	return mrand.Float64() < ratio
}
```

### E. 集計クエリ

```go
type ReportRow struct {
	CampaignID   int64  `json:"campaign_id"`
	CampaignName string `json:"campaign_name"`
	LineItemID   int64  `json:"line_item_id"`
	LineItemName string `json:"line_item_name"`
	Impressions  int64  `json:"impressions"`
	Clicks       int64  `json:"clicks"`
	CTRPct       float64 `json:"ctr_pct"`
	SpendCents   int64  `json:"spend_cents"`
}

const reportQuery = `
SELECT
  c.id, c.name, li.id, li.name,
  COUNT(*) FILTER (WHERE e.event_type = 'impression')                  AS imps,
  COUNT(*) FILTER (WHERE e.event_type = 'click')                       AS clicks,
  COALESCE(
    ROUND(100.0 * COUNT(*) FILTER (WHERE e.event_type = 'click')
                / NULLIF(COUNT(*) FILTER (WHERE e.event_type = 'impression'), 0), 2),
    0
  )                                                                    AS ctr_pct,
  COALESCE(
    SUM(li.bid_cpm_cents) FILTER (WHERE e.event_type = 'impression') / 1000,
    0
  )                                                                    AS spend_cents
FROM events e
JOIN line_items li ON li.id = e.line_item_id
JOIN campaigns c   ON c.id = li.campaign_id
WHERE e.occurred_at::date = $1::date
GROUP BY c.id, c.name, li.id, li.name
ORDER BY c.id, li.id
`

func (s *Store) Report(ctx context.Context, day time.Time) ([]ReportRow, error) {
	rows, err := s.pool.Query(ctx, reportQuery, day.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportRow
	for rows.Next() {
		var r ReportRow
		if err := rows.Scan(&r.CampaignID, &r.CampaignName, &r.LineItemID, &r.LineItemName,
			&r.Impressions, &r.Clicks, &r.CTRPct, &r.SpendCents); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

### F. event 書き込み + imp_id セッション (Step 06 を踏襲)

```go
type impInfo struct {
	LineItemID, CampaignID, CreativeID int64
	UserID                             string
	ExpiresAt                          time.Time
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
func (s *ImpStore) Put(id string, info impInfo) { s.mu.Lock(); defer s.mu.Unlock(); s.m[id] = info }
func (s *ImpStore) Get(id string) (impInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
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

func (s *Store) WriteEvent(ctx context.Context, eventType string, lineItemID, campaignID, creativeID int64, userID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (event_type, line_item_id, campaign_id, creative_id, user_id)
		VALUES ($1, $2, $3, $4, $5)
	`, eventType, lineItemID, campaignID, creativeID, userID)
	return err
}
```

### G. 共通ヘルパ (uid / imp_id / GIF)

```go
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

var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00,
	0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02,
	0x44, 0x01, 0x00, 0x3b,
}

func defaultStr(v, fb string) string {
	if v == "" {
		return fb
	}
	return v
}
```

### H. 配信 / 計測 ハンドラ

```go
type Server struct {
	store *Store
	imps  *ImpStore
	tpl   *template.Template
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
	country := defaultStr(r.URL.Query().Get("country"), "JP")
	device := defaultStr(r.URL.Query().Get("device"), "mobile")

	cands, err := s.store.Candidates(ctx, country, device)
	if err != nil {
		http.Error(w, "candidates: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// pacing で確率的に間引く
	filtered := cands[:0]
	for _, c := range cands {
		if pacingPasses(c.DailyBudgetCents, c.SpentTodayCents) {
			filtered = append(filtered, c)
		} else {
			log.Printf("pacing skip campaign=%d (budget=%d spent=%d)", c.CampaignID, c.DailyBudgetCents, c.SpentTodayCents)
		}
	}

	winner, ok := weightedPick(filtered)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	impID := newImpID()
	s.imps.Put(impID, impInfo{
		LineItemID: winner.LineItemID, CampaignID: winner.CampaignID,
		CreativeID: winner.CreativeID, UserID: uid,
		ExpiresAt: time.Now().Add(10 * time.Minute),
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleImpression(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("imp_id")
	if info, ok := s.imps.Get(id); ok {
		_ = s.store.WriteEvent(r.Context(), "impression", info.LineItemID, info.CampaignID, info.CreativeID, info.UserID)
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(transparentGIF)
}

func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("imp_id")
	dest := r.URL.Query().Get("dest")
	if !strings.HasPrefix(dest, "https://") && !strings.HasPrefix(dest, "http://") {
		http.Error(w, "invalid dest", http.StatusBadRequest)
		return
	}
	if info, ok := s.imps.Get(id); ok {
		_ = s.store.WriteEvent(r.Context(), "click", info.LineItemID, info.CampaignID, info.CreativeID, info.UserID)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

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

### I. レポートハンドラ

```go
func (s *Server) handleReportAPI(w http.ResponseWriter, r *http.Request) {
	day := time.Now().UTC()
	if d := r.URL.Query().Get("date"); d != "" {
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			http.Error(w, "bad date", http.StatusBadRequest)
			return
		}
		day = t
	}
	rows, err := s.store.Report(r.Context(), day)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

const reportTemplate = `<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8"><title>mini-ad / report</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 1000px; margin: 24px auto; padding: 0 16px; }
  table { border-collapse: collapse; width: 100%; }
  th, td { border: 1px solid #ddd; padding: 6px 10px; text-align: right; }
  th:nth-child(-n+4), td:nth-child(-n+4) { text-align: left; }
  thead { background: #f4f4f4; }
</style></head><body>
  <h1>mini-ad — Report ({{.Date}})</h1>
  <table>
    <thead><tr>
      <th>Campaign</th><th>LineItem</th><th>Imps</th><th>Clicks</th><th>CTR %</th><th>Spend (¢)</th>
    </tr></thead>
    <tbody>
    {{range .Rows}}
      <tr>
        <td>{{.CampaignName}} (#{{.CampaignID}})</td>
        <td>{{.LineItemName}} (#{{.LineItemID}})</td>
        <td>{{.Impressions}}</td>
        <td>{{.Clicks}}</td>
        <td>{{.CTRPct}}</td>
        <td>{{.SpendCents}}</td>
      </tr>
    {{else}}
      <tr><td colspan="6">no data</td></tr>
    {{end}}
    </tbody>
  </table>
  <p>↓ JSON: <a href="/api/report?date={{.Date}}">/api/report?date={{.Date}}</a></p>
</body></html>
`

func (s *Server) handleReportHTML(w http.ResponseWriter, r *http.Request) {
	day := time.Now().UTC()
	if d := r.URL.Query().Get("date"); d != "" {
		if t, err := time.Parse("2006-01-02", d); err == nil {
			day = t
		}
	}
	rows, err := s.store.Report(r.Context(), day)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.Execute(w, struct {
		Date string
		Rows []ReportRow
	}{Date: day.Format("2006-01-02"), Rows: rows})
}
```

### J. main

```go
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := newStore(ctx)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}

	tpl, err := template.New("report").Parse(reportTemplate)
	if err != nil {
		log.Fatal(err)
	}

	s := &Server{store: store, imps: newImpStore(), tpl: tpl}

	mux := http.NewServeMux()
	mux.HandleFunc("/ad", s.handleAd)
	mux.HandleFunc("/imp", s.handleImpression)
	mux.HandleFunc("/click", s.handleClick)
	mux.HandleFunc("/report", s.handleReportHTML)
	mux.HandleFunc("/api/report", s.handleReportAPI)

	addr := ":8080"
	log.Printf("[step08] listening on http://localhost%s  report: http://localhost%s/report", addr, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

---

## 動作確認

```bash
go run ./steps/step08-reporting/

# 配信を回す
for i in $(seq 1 50); do
  resp=$(curl -s 'http://localhost:8080/ad?country=JP&device=mobile')
  echo "$resp" | jq -r .imp_url \
    | xargs -I{} curl -s "http://localhost:8080{}" -o /dev/null
done

# レポート (HTML)
open http://localhost:8080/report          # macOS

# レポート (JSON)
curl -s 'http://localhost:8080/api/report' | jq
```

予算をわざと小さくして pacing がかかる挙動を見る：

```bash
psql -c "UPDATE campaigns SET daily_budget_cents = 500 WHERE id = 1;"

# 100 回叩く間に Acme は途中で pacing 抑制されるはず
for i in $(seq 1 100); do
  curl -s 'http://localhost:8080/ad?country=JP&device=mobile' \
    | jq -r '.campaign_id // "no_fill"'
done | sort | uniq -c
```

---

## 実験してみよう

- `daily_budget_cents` を変えて pacing の挙動を観察。pacing 確率が時間とともにどう変わるか
- `pacingPasses` の代わりに「**ハード上限**」(完全停止) を実装して比較
- ASAP / Even の切替フラグをキャンペーンに追加し、ASAP は pacing スキップしないようにする
- レポートに **eCPM** カラムを足す (`spend / imps × 1000`)
- 日別だけでなく **時間別** (`date_trunc('hour', occurred_at)`) のレポートを作る
- 予算残量を Redis にキャッシュして配信判定 → DB クエリを減らす

---

## 設計上のメモ

- 「pacing は確率で抑制」が基本だが、商用 DSP では **「いくらで入札するか」 (bid shading)** で間接的にコントロールするのが現代的
- 集計は配信プロセスとは別の "reporting pipeline" で行うのが普通 (ClickHouse / BigQuery / Druid 等の OLAP DB へ流す)
- 「予算超過」は法的にも避けたい (Advertiser との契約) ので、本番は **multiple layer のチェック** がある

---

## おめでとう

ここまでで以下の機能を持つアドサーバが完成しました：

- 在庫管理 (PostgreSQL)
- ターゲティング (geo / device)
- フリークエンシーキャップ (Redis)
- インプレッション・クリックトラッキング
- 簡易 RTB (第二価格オークション)
- ペーシング & 集計レポート

ここから先の深掘り方向：

- **動画広告 (VAST/VPAID)**
- **viewability 計測** (`IntersectionObserver` ベース)
- **Privacy Sandbox 対応** (Topics API / Protected Audience API)
- **bid shading** や **入札最適化** (機械学習)
- **配信エンジンの高速化** (in-memory snapshot + lock-free 構造)

公式仕様の入口：

- [IAB Tech Lab — OpenRTB 2.x](https://iabtechlab.com/standards/openrtb/)
- [IAB Tech Lab — VAST 4.x](https://iabtechlab.com/standards/vast/)
- [Privacy Sandbox](https://privacysandbox.com/)
