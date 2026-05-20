# 05. トラッキング — impression と click をどう数えるか

「広告を返した = 表示された」ではありません。`/ad` レスポンスを返してもブラウザが描画しないこともある。
正しく数えるためにアドサーバは **トラッキング用のリクエスト** を別途受ける必要があります。

---

## 二大トラッキング方式

### (1) Impression Pixel

返した HTML の中に **1x1 の透明画像** を埋めておき、ブラウザが画像をロードしたタイミングで「表示された」と数える。

```html
<a href="https://ads.example.com/click?imp_id=abc">
  <img src="https://cdn.example.com/creative.jpg">
</a>
<!-- impression pixel -->
<img src="https://ads.example.com/impression?imp_id=abc" width="1" height="1" style="display:none">
```

アドサーバ側のハンドラ:

```go
func handleImpression(w http.ResponseWriter, r *http.Request) {
    // ログを書く
    impID := r.URL.Query().Get("imp_id")
    log.Printf("impression: %s", impID)

    // 1x1 GIF を返す
    w.Header().Set("Content-Type", "image/gif")
    w.Write(transparent1x1GIF)
}
```

### (2) Click Redirect

クリックは **アドサーバを経由してから LP に飛ばす**。これによりクリックを記録できる。

```
[Browser]  click
    │
    ▼
https://ads.example.com/click?imp_id=abc&dest=https%3A//advertiser.com/lp
    │   ← アドサーバが受け取りログ
    ▼
HTTP 302 Location: https://advertiser.com/lp
    │
    ▼
[LP]
```

---

## なぜ pixel なのか

「`<img>` のロード」は古典的な計測手段だが、近年は `<script>` 経由・`Beacon API` (`navigator.sendBeacon`) も多い。
特徴比較:

| 方式 | メリット | デメリット |
|------|----------|-----------|
| `<img>` pixel | あらゆる環境で動く、サードパーティ Cookie 送信可 | レスポンスを待たないため失敗が分からない |
| `<script>` | Cookie・JS 制御可 | ブロックされやすい (AdBlocker) |
| `sendBeacon` | ページ離脱時にも確実に送れる | 古いブラウザ非対応 |

---

## カウントの罠

「impression を素直に数える」だけだと数が膨らみがちです。

| 問題 | 対策 |
|------|------|
| 同じ imp の pixel が二重に発火 | imp_id にユニーク ID を付け、サーバ側で重複排除 |
| Bot が片っ端から pixel を叩く | IVT フィルタ (User-Agent 検出、IP レピュテーション、行動分析) |
| 配信したが画面に出ていない | Viewability 計測 (IntersectionObserver で「50% / 1 秒以上見えたか」を確認) |
| Click 数 > Imp 数 | クリック先 URL を直接 ML から踏まれている → Click は imp の subset のはず |

業界では **MRC (Media Rating Council)** がガイドラインを定めており、Adsserver は「MRC 認定計測」を取ることで信頼性を担保します。

---

## VAST (動画広告)

ディスプレイは img / click で完結しますが、動画広告は **VAST (Video Ad Serving Template)** という XML 仕様で定義されます。

```xml
<VAST version="4.0">
  <Ad>
    <InLine>
      <Impression><![CDATA[https://ads.example.com/imp?...]]></Impression>
      <TrackingEvents>
        <Tracking event="firstQuartile">...</Tracking>
        <Tracking event="midpoint">...</Tracking>
        <Tracking event="complete">...</Tracking>
      </TrackingEvents>
      <MediaFiles>
        <MediaFile type="video/mp4">https://cdn.example.com/ad.mp4</MediaFile>
      </MediaFiles>
    </InLine>
  </Ad>
</VAST>
```

動画再生時に **どの程度見られたか** (4 分の 1, 半分, 完了) を別々の URL で計測するため、imp/click 以上に粒度が細かい。

本リポジトリでは触れませんが、動画広告に進む場合は IAB の VAST 仕様を読むのが定石です。

---

## このリポジトリの実装方針

| Step | 取得イベント | 保存先 |
|------|--------------|--------|
| Step 4 | impression, click | プロセスログ (stdout / file) |
| Step 6 以降 | impression, click | PostgreSQL `events` テーブル |
| Step 8 | impression, click | events から集計してレポート |
