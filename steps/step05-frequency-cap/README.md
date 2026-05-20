# Step 05 — フリークエンシーキャップ

> 「同じ広告を 1 日 100 回見せて嫌われる」のを防ぐ。

ここから **インフラ依存が登場**。Redis を使います。

---

## このステップで学ぶこと

- **ユーザー識別**: cookie で `uid` を発行 / 復元するパターン
- **Redis を使った高速カウンタ**
  - `INCR` の atomicity
  - `EXPIRE` で「自然消滅する」設計
  - キー設計: `freq:{line_item}:{uid}:{yyyymmdd}`
- 配信判定における **「targeting → frequency → 入札」** の順序

関連座学: [docs/03-delivery-flow.md](../../docs/03-delivery-flow.md) §(4) Frequency Cap

---

## 準備

### 1) Redis を起動

別ターミナルで：

```bash
nix run .#redis
```

データは `./.dev/redis/` 配下。止めるときは `Ctrl-C`。完全リセットは `nix run .#reset`。

### 2) go-redis を追加

devShell に入った上で：

```bash
nix develop
go get github.com/redis/go-redis/v9
go mod tidy
```

---

## 何を作るか

| 追加要素 | 役割 |
|----------|------|
| cookie `mini_ad_uid` の発行 | リクエスト元のユーザーを識別 |
| `LineItem.FreqCapPerDay` | 1 日あたりの imp 上限 (0 = 無制限) |
| 配信判定での Redis 参照 | 上限超過した LineItem を候補から除外 |
| impression 計上時の `INCR` + `EXPIRE` | カウンタ加算 |

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

	"github.com/redis/go-redis/v9"
)
```

### B. 在庫に `FreqCapPerDay` を追加

```go
type Creative struct {
	ID     string
	Title  string
	Image  string
	Click  string
	Width  int
	Height int
}

type Targeting struct {
	Countries []string
}

type LineItem struct {
	ID            string
	Targeting     Targeting
	BidCPM        int
	FreqCapPerDay int // 0 = 制限なし
	Creative      Creative
}

var inventory = []LineItem{
	{
		ID:            "li-acme-jp",
		Targeting:     Targeting{Countries: []string{"JP"}},
		BidCPM:        300,
		FreqCapPerDay: 3, // 同じ uid に 1 日 3 回まで
		Creative:      Creative{ID: "cr-acme", Title: "Acme", Image: "https://placehold.co/300x250/orange/white?text=Acme", Click: "https://example.com/acme", Width: 300, Height: 250},
	},
	{
		ID:            "li-globex-any",
		Targeting:     Targeting{},
		BidCPM:        150,
		FreqCapPerDay: 0, // 無制限
		Creative:      Creative{ID: "cr-globex", Title: "Globex", Image: "https://placehold.co/300x250/brown/white?text=Globex", Click: "https://example.com/globex", Width: 300, Height: 250},
	},
}
```

### C. Redis frequency store

```go
type FreqStore struct {
	rdb *redis.Client
}

func newFreqStore(addr string) *FreqStore {
	return &FreqStore{
		rdb: redis.NewClient(&redis.Options{Addr: addr}),
	}
}

func (s *FreqStore) key(uid, lineItem string) string {
	return fmt.Sprintf("freq:%s:%s:%s", lineItem, uid, time.Now().UTC().Format("20060102"))
}

// Count: 当日の imp 回数を返す。キーが無ければ 0。
func (s *FreqStore) Count(ctx context.Context, uid, lineItem string) (int, error) {
	v, err := s.rdb.Get(ctx, s.key(uid, lineItem)).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

// Inc: imp 計上時に呼ぶ。1 加算して 24h で expire を貼る。
// パイプラインで INCR と EXPIRE を 1 ラウンドトリップにまとめる。
func (s *FreqStore) Inc(ctx context.Context, uid, lineItem string) error {
	k := s.key(uid, lineItem)
	pipe := s.rdb.TxPipeline()
	pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}
```

ポイント：
- **キーに日付 (yyyymmdd) を含める** → 翌日になれば自然に別キー → リセット相当
- `redis.Nil` を `count=0` に変換する小さなラッパー
- `INCR` + `EXPIRE` をパイプラインで 1 つにする（ラウンドトリップ削減）

### D. uid cookie ヘルパ

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
		Name:     uidCookieName,
		Value:    uid,
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 30, // 30 日
		HttpOnly: true,
	})
	return uid
}
```

### E. 配信判定 (targeting + freq cap)

```go
type Context struct{ Country string }

func match(t Targeting, c Context) bool {
	if len(t.Countries) == 0 {
		return true
	}
	for _, x := range t.Countries {
		if x == c.Country {
			return true
		}
	}
	return false
}

// pickAd: targeting + frequency cap でフィルタ → 重み付け選択
func (s *Server) pickAd(ctx context.Context, uid string, reqCtx Context) (LineItem, bool, error) {
	var cands []LineItem
	for _, li := range inventory {
		if !match(li.Targeting, reqCtx) {
			continue
		}
		if li.FreqCapPerDay > 0 {
			cnt, err := s.freq.Count(ctx, uid, li.ID)
			if err != nil {
				return LineItem{}, false, err
			}
			if cnt >= li.FreqCapPerDay {
				log.Printf("freq cap: uid=%s line_item=%s count=%d/cap=%d skip", uid, li.ID, cnt, li.FreqCapPerDay)
				continue
			}
		}
		cands = append(cands, li)
	}
	if len(cands) == 0 {
		return LineItem{}, false, nil
	}
	return weightedPick(cands), true, nil
}

func weightedPick(items []LineItem) LineItem {
	total := 0
	for _, li := range items {
		total += li.BidCPM
	}
	if total == 0 {
		return items[mrand.Intn(len(items))]
	}
	r := mrand.Intn(total)
	cum := 0
	for _, li := range items {
		cum += li.BidCPM
		if r < cum {
			return li
		}
	}
	return items[len(items)-1]
}
```

ポイント：
- `targeting → freq cap → 重み付け` の順序。順序を変えると無意味な Redis 参照が増える
- `FreqCapPerDay == 0` のときは Redis を見ない (高速化 & コスト)

### F. 1x1 GIF (Step 04 と同じ) と imp_id

```go
var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00,
	0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02,
	0x44, 0x01, 0x00, 0x3b,
}

func newImpID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

### G. Server とハンドラ

```go
type Server struct {
	freq *FreqStore
}

type AdResponse struct {
	LineItemID string `json:"line_item_id"`
	ImpID      string `json:"imp_id"`
	UID        string `json:"uid"`
	BidCPM     int    `json:"bid_cpm"`
	Title      string `json:"title"`
	Image      string `json:"image_url"`
	ClickURL   string `json:"click_url"`
	ImpURL     string `json:"imp_url"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

func (s *Server) handleAd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := ensureUID(w, r)
	reqCtx := Context{Country: defaultStr(r.URL.Query().Get("country"), "JP")}

	li, ok, err := s.pickAd(ctx, uid, reqCtx)
	if err != nil {
		http.Error(w, "frequency lookup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	impID := newImpID()

	resp := AdResponse{
		LineItemID: li.ID,
		ImpID:      impID,
		UID:        uid,
		BidCPM:     li.BidCPM,
		Title:      li.Creative.Title,
		Image:      li.Creative.Image,
		ClickURL:   fmt.Sprintf("/click?imp_id=%s&line_item=%s&dest=%s", impID, li.ID, url.QueryEscape(li.Creative.Click)),
		ImpURL:     fmt.Sprintf("/imp?imp_id=%s&line_item=%s", impID, li.ID),
		Width:      li.Creative.Width,
		Height:     li.Creative.Height,
	}

	log.Printf("ad: uid=%s line_item=%s imp_id=%s", uid, li.ID, impID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleImpression(w http.ResponseWriter, r *http.Request) {
	uid := ensureUID(w, r) // 普通は ad と同じセッションだが念のため
	li := r.URL.Query().Get("line_item")

	if err := s.freq.Inc(r.Context(), uid, li); err != nil {
		log.Printf("freq inc error: %v", err)
	}

	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(transparentGIF)
}

func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	dest := r.URL.Query().Get("dest")
	if !strings.HasPrefix(dest, "https://") && !strings.HasPrefix(dest, "http://") {
		http.Error(w, "invalid dest", http.StatusBadRequest)
		return
	}
	log.Printf("click: imp_id=%s line_item=%s dest=%s",
		r.URL.Query().Get("imp_id"), r.URL.Query().Get("line_item"), dest)
	http.Redirect(w, r, dest, http.StatusFound)
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
```

ポイント：
- `INCR` は **impression ハンドラ** で。`/ad` 時点では「実際に表示された」と確定していないため
- `pickAd` の `error` 経路は Redis 障害時の挙動。本番では「Redis ダウン時は cap を素通り (= 配信止めない)」みたいな fallback が必要

### H. HTML テンプレート

```go
const publisherPageHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <title>Step 05 — Frequency Cap</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
    .ad-slot { margin: 24px 0; padding: 8px; border: 1px dashed #ccc; min-height: 80px; }
  </style>
</head>
<body>
  <h1>Step 05 — Frequency Cap</h1>
  <p>リロードを繰り返すと、Acme は 3 回までで cap に当たり、Globex (cap なし) だけが返るようになる。</p>

  <div class="ad-slot" id="ad-slot"><small>ad slot</small></div>

  <script>
    fetch('/ad', { credentials: 'same-origin' })
      .then(r => r.status === 204 ? null : r.json())
      .then(ad => {
        const slot = document.getElementById('ad-slot');
        if (!ad) {
          slot.innerHTML = '<small>no fill</small>';
          return;
        }
        slot.innerHTML =
          '<small>line_item=' + ad.line_item_id + ' uid=' + ad.uid + '</small><br>' +
          '<a href="' + ad.click_url + '">' +
          '<img src="' + ad.image_url + '" width="' + ad.width + '" height="' + ad.height + '"></a>' +
          '<img src="' + ad.imp_url + '" width="1" height="1" style="display:none">';
      });
  </script>
</body>
</html>
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

### I. main

```go
func main() {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	s := &Server{freq: newFreqStore(redisAddr)}

	// 起動時に Redis に届くか軽く確認
	if err := s.freq.rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis ping failed at %s: %v\n  → 別ターミナルで `nix run .#redis` を起動してください", redisAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handlePage)
	mux.HandleFunc("/ad", s.handleAd)
	mux.HandleFunc("/imp", s.handleImpression)
	mux.HandleFunc("/click", s.handleClick)

	addr := ":8080"
	log.Printf("[step05] listening on http://localhost%s (redis=%s)", addr, redisAddr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

---

## 動作確認

```bash
go run ./steps/step05-frequency-cap/
```

別ターミナルで（cookie を `-c` / `-b` で同一ファイルに保存）：

```bash
JAR=/tmp/cookies
rm -f $JAR

# 1) /ad を 5 回叩く + 直後に /imp を叩いて imp を計上
for i in $(seq 1 5); do
  resp=$(curl -s -c $JAR -b $JAR 'http://localhost:8080/ad?country=JP')
  echo "$resp" | jq -c '{line_item_id, imp_id}'
  imp_url=$(echo "$resp" | jq -r .imp_url)
  curl -s -c $JAR -b $JAR "http://localhost:8080${imp_url}" -o /dev/null
done

# 2) Redis 直接確認
redis-cli keys 'freq:*'
redis-cli get $(redis-cli keys 'freq:li-acme-jp:*' | head -1)

# 3) cap (3) に当たった後の挙動: Acme は返らず Globex に切り替わる
for i in $(seq 1 3); do
  curl -s -c $JAR -b $JAR 'http://localhost:8080/ad?country=JP' | jq -r .line_item_id
done
```

期待：

- 最初の 3 回は Acme JP (cap = 3)
- 4 回目以降は Globex (cap なし) に切り替わる

---

## 実験してみよう

- 2 つの異なるブラウザ (Chrome / Firefox) で同時アクセス → 別 uid で別カウンタになる
- `Inc` の `EXPIRE` を `60 * time.Second` に変えて、1 分後に cap がリセットされるのを観察
- `redis-cli MONITOR` で配信時の Redis コマンド列を観察
- frequency cap を「キャンペーン単位」「ユーザー × 全広告」など別の粒度で実装してみる
- impression と click で別 cap (`freq:imp:` / `freq:click:`) を持たせる

---

## 設計上のメモ

- 実プロダクトでは frequency cap は超ヘビーな機能。**毎 imp で参照**するため、Redis の負荷分散・レプリケーション・shard が前提
- 「**ユーザー間で同一 uid が漏れない**」工夫が必要 (例: partition key を `hash(uid)` にする)
- IDFA / GAID 廃止に伴って、cookie 以外のユーザー識別 (UID2.0 / Privacy Sandbox の Topics API 等) への対応が業界トレンド

---

## 次へ

→ [Step 06 — キャンペーン管理 (PostgreSQL)](../step06-campaign/)
