# Step 01 — Hello, Ad! / HTTP サーバの土台

> 最小のアドサーバを、後の step でも使い回せる **proper な Go HTTP サーバ構造** で組む。

---

## このステップで学ぶこと

- アドサーバが返すのは **広告そのものではなく「広告クリエイティブの情報 (JSON)」** であること
- ブラウザ → アドサーバの ad request は単なる HTTP GET
- 後続 step でずっと使う **Go HTTP サーバの土台**
  - `log/slog` による構造化ログ
  - middleware パターン (request ID / access log / panic recovery)
  - `http.Server` + `signal.NotifyContext` による **graceful shutdown**
  - 環境変数経由の設定
  - **複数広告枠 (slot)** を扱う在庫構造

関連座学: [docs/00-overview.md](../../docs/00-overview.md), [docs/03-delivery-flow.md](../../docs/03-delivery-flow.md)

---

## 何を作るか

`steps/step01-hello-ad/main.go` を新規作成。

| Method & Path | レスポンス | 役割 |
|---------------|-----------|------|
| `GET /` | `text/html` | Publisher の記事ページ。中に 2 つの広告枠 + 広告タグ |
| `GET /ad?slot=main-rectangle` | `application/json` (200) | 広告クリエイティブ |
| `GET /ad?slot=unknown` | (204) | 在庫なし時の no-fill |
| `GET /healthz` | `application/json` (200) | サーバ生存確認 |

ファイル構成（1 ファイルにまとめる）：

```
steps/step01-hello-ad/main.go
  ├─ config     (env から設定読み込み)
  ├─ domain     (Ad / SlotID / Inventory)
  ├─ middleware (RequestID / AccessLog / Recover)
  ├─ server     (Server struct + handlers)
  ├─ HTML テンプレート
  └─ main       (起動 + graceful shutdown)
```

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
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)
```

`log/slog` は Go 1.21+ 標準の構造化ログ。`signal.NotifyContext` は graceful shutdown のキモ。

### B. 設定 (env 経由)

```go
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
```

ポイント：
- env が未設定でも動く安全な default を持つ
- LOG_LEVEL=debug で動かすと slog が DEBUG 以上を出力するようになる

### C. ドメイン型 — Ad / SlotID / Inventory

```go
// Ad は配信される広告 1 つ。
type Ad struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	ClickURL string `json:"click_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// SlotID は広告枠の論理 ID。サイズと用途を区別する。
type SlotID string

const (
	SlotMainRectangle  SlotID = "main-rectangle"  // 300 x 250
	SlotTopBanner      SlotID = "top-banner"      // 728 x 90
	SlotSideSkyscraper SlotID = "side-skyscraper" // 160 x 600
)

// Inventory: 各 slot に出せる広告のリスト。
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
		// side-skyscraper は今回は在庫なし → no-fill の動作確認に使う
	}
}
```

ポイント：
- 「広告枠 (slot)」は **物理位置 (top / sidebar)** ではなく **論理サイズ + 用途** として識別
- `Inventory` を `map[SlotID][]Ad` にしたことで、「枠ごとに別の在庫を引く」配信フローが自然に書ける
- ある slot に在庫がない場合の **no-fill** が default で表現できる (`side-skyscraper`)

### D. middleware — Request ID / Access Log / Recover

middleware は「リクエストを処理する前後に共通処理を差し込む」パターン。Go 標準 `net/http` では `http.Handler` を装飾する形で書く。

```go
// Middleware は http.Handler を装飾する関数型。
type Middleware func(http.Handler) http.Handler

// Chain は middleware を「外側 → 内側」の順で適用する。
// Chain(A, B, C)(h) は A(B(C(h))) と等価で、
// 受信時は A → B → C → h、レスポンス時は逆順に通る。
func Chain(mws ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}
```

#### Request ID — リクエストごとの ID 発行

```go
type ctxKey int

const ctxKeyRequestID ctxKey = iota

var reqIDCounter uint64

func nextRequestID() string {
	n := atomic.AddUint64(&reqIDCounter, 1)
	return fmt.Sprintf("req-%d-%06d", time.Now().UnixNano(), n)
}

// RequestID: 既存の X-Request-ID ヘッダがあればそれを使い、無ければ発行する。
// context に詰めて downstream で取り出せるようにする。
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

// GetRequestID: context から取り出すヘルパ。ハンドラ側で使う。
func GetRequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}
```

ポイント：
- `ctxKey` を **プライベートな独自型** にする — `context.WithValue` のキー衝突を防ぐ Go の定石
- 上流 (例: ロードバランサ) が既に X-Request-ID を付けてくれていれば尊重する

#### Access Log — レスポンスのステータス・サイズ・所要時間を記録

`http.ResponseWriter` は標準でステータスを記録してくれないので、wrapper を作る。

```go
// statusRecorder は ResponseWriter を装飾してステータスとバイト数を観測可能にする。
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
```

#### Recover — panic で落ちないようにする

```go
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
```

ポイント：
- panic はゴルーチン単位で伝播するので、ハンドラ単位で `defer recover()` を仕込まないと **1 つのリクエストで落ちたら全リクエスト道連れ**
- スタックトレースを `debug.Stack()` で取って slog に乗せる

### E. Server と handler

ハンドラを **メソッドにする** ことで、依存 (logger, inventory) を `Server` フィールド経由で渡せる。グローバル変数を避ける idiomatic な書き方。

```go
type Server struct {
	logger    *slog.Logger
	inventory Inventory
}

func newServer(logger *slog.Logger, inv Inventory) *Server {
	return &Server{logger: logger, inventory: inv}
}

// routes: ServeMux を組み立てて返す。
// middleware 適用は main で外から行うので、ここでは "純粋なルーティング" だけを返す。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/ad", s.handleAd)
	return mux
}

// === helpers ===

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// === handlers ===

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
		// 該当 slot に在庫なし: no-fill (204) を返す。
		s.logger.InfoContext(r.Context(), "no fill",
			slog.String("req_id", GetRequestID(r.Context())),
			slog.String("slot", string(slot)),
		)
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	// この step ではまだ選定ロジックがないので先頭を返す。
	// Step 02 で random / weighted / highest を導入する。
	ad := ads[0]
	s.logger.InfoContext(r.Context(), "ad served",
		slog.String("req_id", GetRequestID(r.Context())),
		slog.String("slot", string(slot)),
		slog.String("ad_id", ad.ID),
	)
	writeJSON(w, http.StatusOK, ad)
}
```

ポイント：
- `routes()` で middleware を適用しないのが大事。**middleware は main で外から付ける** (テスト時にも組み替えやすい)
- ハンドラからは `s.logger.InfoContext(r.Context(), ...)` のように **context 経由** で呼ぶ。req_id を slog の attrs に詰めて構造化

### F. publisher ページの HTML テンプレート

複数の `<div class="ad-slot">` を置いて、JS で **全部の slot に対して並列に `/ad` を呼ぶ**。

```go
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
```

### G. main — 起動と graceful shutdown

「Ctrl-C を受けたら `srv.Shutdown(ctx)` を呼んで、in-flight リクエストが終わるまで待つ」のが graceful shutdown。

```go
func main() {
	cfg := loadConfig()

	// 構造化ログ (JSON 出力)。本番想定。
	// stdout に人間向け text で出したいなら slog.NewTextHandler を使う。
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	s := newServer(logger, defaultInventory())

	// middleware は外から付ける (順序が重要)
	handler := Chain(
		RequestID,           // 最外側: 全リクエストに ID 振る
		AccessLog(logger),   // ステータス・所要時間を記録
		Recover(logger),     // panic を吸収
	)(s.routes())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// SIGINT / SIGTERM を待つ context
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// サーバはバックグラウンドで起動
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", slog.String("addr", cfg.Addr))
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// シグナル受信 or サーバエラーを待つ
	select {
	case <-rootCtx.Done():
		logger.Info("shutdown requested by signal")
	case err := <-serverErr:
		if err != nil {
			logger.Error("server crashed", slog.Any("err", err))
			os.Exit(1)
		}
	}

	// graceful shutdown: in-flight を最大 10 秒待つ
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", slog.Any("err", err))
		_ = srv.Close()
		os.Exit(1)
	}
	logger.Info("server stopped cleanly")
}
```

ポイント：
- `ReadHeaderTimeout` は Slowloris 攻撃の最低限の対策。**入れないと CodeQL が叱る**
- `signal.NotifyContext` を使うと「シグナル受信時に context が cancel される」が綺麗に書ける (Go 1.16+)
- `serverErr` チャネルでサーバ自身のクラッシュも検知

---

## 動作確認

ターミナル 1：

```bash
go run ./steps/step01-hello-ad/
```

ターミナル 2：

```bash
# healthz
curl -s http://localhost:8080/healthz | jq

# 各 slot を直接叩く
curl -s 'http://localhost:8080/ad?slot=main-rectangle'   | jq
curl -s 'http://localhost:8080/ad?slot=top-banner'       | jq
curl -i 'http://localhost:8080/ad?slot=side-skyscraper' 2>&1 | head -3   # 204
curl -i 'http://localhost:8080/ad?slot=unknown'         2>&1 | head -3   # 204

# Request ID の echo
curl -s -H 'X-Request-ID: my-trace-1' -D - 'http://localhost:8080/healthz' | grep -i request-id

# panic 確認用 (handleAd に panic を仕込んで再起動した場合)
# → アクセスログには 500、サーバは生き残る
```

ブラウザで http://localhost:8080 を開くと 3 枠が並列に読まれ、`side-skyscraper` だけが no-fill 表示になる。

ターミナル 1 (サーバ) で graceful shutdown を確認：

```bash
# Ctrl-C を打つと
# {"time":"...","level":"INFO","msg":"shutdown requested by signal"}
# {"time":"...","level":"INFO","msg":"server stopped cleanly"}
```

ログを JSON で読みたい時は：

```bash
go run ./steps/step01-hello-ad/ 2>&1 | jq -c
LOG_LEVEL=debug go run ./steps/step01-hello-ad/    # DEBUG レベルも見たい時
```

---

## 実験してみよう

- `handleAd` の中に `panic("oops")` を仕込む → 1 リクエストは 500 になるがサーバは生き残ることを確認
- `SlotID` を `"unknown"` で叩いたら 204 が返ることを確認 → ブラウザのスロットも「no fill」表示
- `ADDR=:9999 go run ...` で別ポートで起動できることを確認
- `LOG_LEVEL=debug` にすると `ad served` の DEBUG 行が見えるようになる… **見えない**。`InfoContext` で出しているので debug にしても増えない。試しに `DebugContext` に書き換えて挙動を確認
- ベンチ: `wrk -t2 -c50 -d10s http://localhost:8080/ad?slot=main-rectangle` で req/s を測ってみる

---

## 設計上のメモ

- 「ハンドラを `Server` のメソッドにする」「routes() で純粋にルーティングだけ返す」「middleware は main で外から付ける」 の 3 点セットは **テストしやすさ** に直結する (`httptest.NewServer(s.routes())` で完結する)
- `slog` は Go 1.21+ で標準入りした構造化ログ。ad 配信のような **大量同質ログ** こそ JSON で出して後段で集計するのが現代的
- graceful shutdown は本番運用必須。デプロイ時にユーザのリクエストを途中で切らないために
- 「アドサーバの責務 = リクエストごとに最適な広告を返す」とは別に、「サーバとしての健全性 = healthz / graceful shutdown / observability」が必要

---

## 次へ

→ [Step 02 — 広告選択ロジック](../step02-ad-selection/)
