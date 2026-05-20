# Step 05 — フリークエンシーキャップ

> 「同じ広告を 1 日 100 回見せて嫌われる」のを防ぐ。**Step 04 のコードを継承**し、Redis でユーザー単位の imp 上限を実装する。

---

## このステップで学ぶこと

- **ユーザー識別**: cookie で `uid` を発行 / 復元するパターン
- **Redis を使った高速カウンタ**
  - `INCR` の atomicity
  - `EXPIRE` で「自然消滅する」設計
  - キー設計: `freq:{line_item}:{uid}:{yyyymmdd}`
- 配信判定における **「targeting → frequency → 入札」** の順序
- Step 01〜04 で組んだ **slog / middleware / graceful shutdown** を維持したままインフラを足す

関連座学: [docs/03-delivery-flow.md](../../docs/03-delivery-flow.md) §(4) Frequency Cap

---

## 前提

Step 04 までの構成（複数 `.go` ファイル、slog、EventWriter、Dedup）を **そのまま引き継ぐ**。

```bash
cp -r steps/step04-tracking steps/step05-frequency-cap
# 以下の差分を当てる
```

完成例の参照: [`solutions/step05-frequency-cap/`](../../solutions/step05-frequency-cap/)（ビルド確認用。先に読むより、詰まったときの diff 向き）

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

`REDIS_URL` は devShell で `redis://127.0.0.1:6379` がセット済み（アドレスだけ使う場合は `127.0.0.1:6379` でも可）。

---

## ファイル構成（Step 04 + 追加）

```
steps/step05-frequency-cap/
├── main.go        ← Redis ping / FreqStore を Server に渡す
├── middleware.go  ← (変更なし)
├── domain.go      ← LineItem に FreqCapPerDay を追加
├── targeting.go   ← (変更なし)
├── ua.go          ← (変更なし)
├── selector.go    ← (変更なし)
├── ids.go         ← (変更なし)
├── events.go      ← (変更なし)
├── freq.go        ← ★ 新規: FreqStore + uid cookie
└── server.go      ← pick 前に freq チェック、/imp で INCR
```

---

## 何を作るか

| 追加・変更 | 役割 |
|------------|------|
| cookie `mini_ad_uid` | リクエスト元ユーザーを識別 |
| `LineItem.FreqCapPerDay` | 1 日あたりの imp 上限（0 = 無制限） |
| `FreqStore` (Redis) | 当日 imp 回数の取得 / 加算 |
| `handleAd` | targeting 後に freq で候補を除外してから Selector |
| `handleImpression` | imp 計上時に `INCR`（**表示確定後**にカウント） |
| `handlePage` | 初回訪問で uid cookie を発行 |

---

## 実装（差分のみ）

### A. `domain.go` — `FreqCapPerDay` を追加

```go
type LineItem struct {
	ID            string    `json:"id"`
	Slot          SlotID    `json:"slot"`
	Targeting     Targeting `json:"targeting"`
	BidCPM        int       `json:"bid_cpm"`
	FreqCapPerDay int       `json:"freq_cap_per_day"` // 0 = 制限なし
	Creative      Creative  `json:"creative"`
}
```

`defaultInventory()` の例（Acme JP は 1 日 3 回まで）：

```go
{
	ID: "li-acme-jp-mobile", Slot: SlotMainRectangle,
	Targeting: Targeting{Countries: Include([]string{"JP"}), Devices: Include([]string{"mobile"})},
	BidCPM: 300, FreqCapPerDay: 3,
	Creative: Creative{ID: "cr-1", Title: "Acme JP", ImageURL: "https://placehold.co/300x250/orange/white?text=Acme", ClickURL: "https://example.com/acme", Width: 300, Height: 250},
},
{
	ID: "li-globex-jp-weekend", Slot: SlotMainRectangle,
	Targeting: Targeting{Countries: Include([]string{"JP"})},
	BidCPM: 150, FreqCapPerDay: 0, // 無制限
	Creative: Creative{ID: "cr-globex", Title: "Globex", ImageURL: "https://placehold.co/300x250/brown/white?text=Globex", ClickURL: "https://example.com/globex", Width: 300, Height: 250},
},
```

### B. `freq.go` — Redis + uid cookie（新規ファイル）

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const uidCookieName = "mini_ad_uid"

type FreqStore struct {
	rdb *redis.Client
}

func newFreqStoreFromEnv() (*FreqStore, error) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		if u := os.Getenv("REDIS_URL"); strings.HasPrefix(u, "redis://") {
			addr = strings.TrimPrefix(u, "redis://")
		} else {
			addr = "127.0.0.1:6379"
		}
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return &FreqStore{rdb: rdb}, nil
}

func (s *FreqStore) key(uid, lineItem string) string {
	return fmt.Sprintf("freq:%s:%s:%s", lineItem, uid, time.Now().UTC().Format("20060102"))
}

func (s *FreqStore) Count(ctx context.Context, uid, lineItem string) (int, error) {
	v, err := s.rdb.Get(ctx, s.key(uid, lineItem)).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (s *FreqStore) Inc(ctx context.Context, uid, lineItem string) error {
	k := s.key(uid, lineItem)
	pipe := s.rdb.TxPipeline()
	pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// ensureUID: 既存 cookie があれば返す。無ければ発行して Set-Cookie する。
func ensureUID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(uidCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	uid := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: uidCookieName, Value: uid, Path: "/",
		MaxAge: 60 * 60 * 24 * 30, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return uid
}
```

ポイント：

- キーに **日付 (yyyymmdd)** を含める → 翌日は別キーになり、自然リセット相当
- `redis.Nil` → `count=0` に正規化
- `INCR` + `EXPIRE` は **パイプライン**で 1 RTT にまとめる

### C. `server.go` — 配信パイプラインに freq を挿入

`Server` に `freq *FreqStore` を追加。

```go
type Server struct {
	logger    *slog.Logger
	inventory Inventory
	selectors *SelectorRegistry
	events    *EventWriter
	dedup     *Dedup
	freq      *FreqStore
}
```

候補抽出（**targeting → frequency → selector**）：

```go
func (s *Server) eligibleLineItems(ctx context.Context, uid string, slot SlotID, reqCtx Context) ([]LineItem, error) {
	var out []LineItem
	for _, li := range s.inventory.BySlot(slot) {
		if !li.Targeting.Matches(reqCtx) {
			continue
		}
		if li.FreqCapPerDay > 0 {
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
```

`handleAd` の先頭で uid を確保し、`eligibleLineItems` の結果を `selector.Pick` に渡す。

`handlePage` でも `ensureUID(w, r)` を呼ぶ（初回から cookie が付く）。

`handleImpression` で imp 計上（Step 04 の dedup / event 送信の**後**または**成功時**）：

```go
lineItem := r.URL.Query().Get("line_item")
uid := ensureUID(w, r)
if lineItem != "" && uid != "" {
	if err := s.freq.Inc(r.Context(), uid, lineItem); err != nil {
		s.logger.WarnContext(r.Context(), "freq inc failed", slog.Any("err", err))
	}
}
```

ポイント：

- **`/ad` では INCR しない** — まだ表示されていない。imp pixel が返ってきた時点でカウント
- `FreqCapPerDay == 0` の LineItem は Redis を見ない（コスト削減）
- Redis 障害時: 学習用は 500 を返す。本番では「cap を素通り」「配信停止」などポリシーを決める

### D. publisher HTML — `credentials: 'same-origin'`

cookie を送るため、Step 04 の `fetch('/ad?...')` を次のように変更：

```javascript
fetch('/ad?slot=' + slot + (q ? '&' + q : ''), { credentials: 'same-origin' })
```

imp pixel も同様に `credentials: 'same-origin'` を付ける。

### E. `main.go` — 起動時 Redis チェック

```go
freq, err := newFreqStoreFromEnv()
if err != nil {
	logger.Error("redis unavailable", slog.Any("err", err),
		slog.String("hint", "別ターミナルで nix run .#redis を起動してください"))
	os.Exit(1)
}
s := newServer(logger, defaultInventory(), selectors, events, dedup, freq)
```

graceful shutdown は Step 04 と同じ（HTTP → EventWriter flush の 2 段階）。

---

## 動作確認

```bash
go run ./steps/step05-frequency-cap/
```

別ターミナルで（**cookie を保持**）：

```bash
JAR=/tmp/mini-ad-cookies.txt
rm -f "$JAR"

for i in $(seq 1 5); do
  resp=$(curl -s -c "$JAR" -b "$JAR" 'http://localhost:8080/ad?slot=main-rectangle&country=JP&device=mobile')
  echo "$resp" | jq -c '{line_item_id, imp_id}'
  imp_url=$(echo "$resp" | jq -r .imp_url)
  curl -s -c "$JAR" -b "$JAR" "http://localhost:8080${imp_url}" -o /dev/null
done

redis-cli keys 'freq:*'
```

期待：

- `FreqCapPerDay: 3` の LineItem は、**imp を 3 回計上した後**は候補から外れる
- cap のない LineItem にフォールバックする（no-fill にならない設計にしておくと分かりやすい）

ブラウザでは http://localhost:8080 を開き、リロードを繰り返して挙動を確認。

---

## 実験してみよう

- 2 つのブラウザで同時アクセス → 別 `uid` で別カウンタ
- `EXPIRE` を `60 * time.Second` に変え、1 分後に cap がリセットされるのを観察
- `redis-cli MONITOR` で `INCR` / `EXPIRE` のタイミングを確認
- frequency cap をキャンペーン単位（`freq:campaign:{id}:...`）に変更してみる

---

## 設計上のメモ

- 実プロダクトでは frequency cap は **毎 imp 前後で参照**されるため、Redis の shard / レプリケーションが前提
- 「**いつカウントするか**」= ad request か imp pixel か viewable かで、cap の意味が変わる（この step では **imp pixel**）
- cookie 以外の ID（Privacy Sandbox、UID2 等）への対応が業界トレンド

---

## 次へ

→ [Step 06 — キャンペーン管理 (PostgreSQL)](../step06-campaign/)
