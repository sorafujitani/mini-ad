# 06. オークションと RTB

複数の広告候補が残ったとき、どう 1 つを選ぶか。
最大化したい目的が **「収益」** なので、原則は **eCPM が高い順** に並べる。これがアドサーバ内の「ローカルオークション」。
さらに外部 DSP に問い合わせて入札を集めるのが **RTB (Real-Time Bidding)**。

---

## まず: ローカルオークション

直販キャンペーンしかないアドサーバでも、複数 LineItem が同じ枠を狙うことがあるため社内でオークションが必要。

```
candidates: [
  { line_item_id: 1, bid_cpm: 300 },
  { line_item_id: 2, bid_cpm: 200 },
  { line_item_id: 3, bid_cpm: 150 },
]

→ line_item_id = 1 が勝ち (= 1 imp を 0.3 セントで売る)
```

CPM 入札なら単純比較で済むが、CPC / CPA が混在する場合は eCPM 換算が必要:

```
eCPM(LineItem)
  = expected_revenue_per_impression × 1000

CPC モデル: eCPM = CPC × 予測 CTR × 1000
CPA モデル: eCPM = CPA × 予測 CTR × 予測 CVR × 1000
```

「予測 CTR」「予測 CVR」を機械学習で出すのが、商用 DSP の中核技術です (CTR prediction)。

---

## RTB の全体像

```
[Publisher Ad Server / SSP]
        │
        │ 1. bid request (OpenRTB JSON)
        │    "この imp 誰か入札する？ 100ms 以内に返してね"
        │
        ├──────────────► DSP A  → bid: 250
        ├──────────────► DSP B  → bid: 310   ★ winner
        └──────────────► DSP C  → no bid
        │
        │ 2. winner を決定 (auction)
        │
        ▼
[Publisher Ad Server]
        │
        │ 3. winning ad markup を返す
        │
        ▼
[Browser]
        │
        │ 4. winner DSP の nurl (win notice) を叩く
        │
        ▼
[DSP B]   ← 「自分が勝った」と知る
```

---

## OpenRTB の最小 bid request

```json
{
  "id": "req-abc123",
  "imp": [{
    "id": "1",
    "banner": { "w": 300, "h": 250 },
    "bidfloor": 1.50,
    "bidfloorcur": "USD"
  }],
  "site": {
    "domain": "publisher.example.com",
    "page":   "https://publisher.example.com/article/42"
  },
  "device": {
    "ua": "Mozilla/5.0 ...",
    "ip": "203.0.113.5",
    "geo": { "country": "JPN" }
  },
  "user":   { "id": "u-xyz" },
  "at":     2
}
```

ポイント:

- `at: 2` = Second-price auction
- `bidfloor` = フロアプライス。これ未満は弾かれる
- `device.geo.country` = 3 文字 ISO コード
- 入札者 (DSP) は **100ms 以内** に応答する暗黙ルール

bid response の最小形:

```json
{
  "id": "req-abc123",
  "seatbid": [{
    "bid": [{
      "id":     "bid-1",
      "impid":  "1",
      "price":  3.10,
      "adm":    "<a href=...><img src=...></a>",
      "nurl":   "https://dsp.example.com/win?bid_id=bid-1&price=${AUCTION_PRICE}",
      "lurl":   "https://dsp.example.com/loss?bid_id=bid-1"
    }]
  }]
}
```

---

## First-price vs Second-price

| | First-price | Second-price |
|---|---|---|
| 落札者が払う額 | 自分の入札額 | 2 位の入札額 + 0.01 |
| 戦略 | 「他がいくらで入れそうか」を読む駆け引きが要る | **誠実入札 (真の評価額)** が dominant strategy |
| 歴史 | 2017 年頃まで Header Bidding は first-price メイン | OpenRTB の本来想定は second-price |
| 現在 | Google Ads Manager は 2019 年に first-price へ移行 | DSP 内部のローカルオークションでは依然多い |

理論的には second-price が "truthful" だが、実装上 SSP がフロアを動的調整すると second-price の利点が薄れる。結果として **「実質 first-price」** が業界で増えました。

---

## 第二価格オークションの計算例

```
入札: [DSP_A=5.00, DSP_B=3.10, DSP_C=2.50]
floor: 1.50

winner = DSP_A
DSP_A の支払額 = max(2位の入札, floor) + 0.01
              = max(3.10, 1.50) + 0.01
              = 3.11
```

DSP_A は 5.00 まで出す価値があると思って入札したのに、実際の支払いは 3.11 で済む = 入札者にとって「正直に評価額を入れるのが最適」になる、というのが理論的な美しさ。

---

## Header Bidding (発展)

```
[Browser]
    │
    │ (ページロード時、ad slot より先に)
    ├──── prebid.js が複数 SSP に並列入札
    │      ├──► SSP A
    │      ├──► SSP B
    │      └──► SSP C
    │
    │ 各 SSP の最高入札額を Browser 側に返す
    │
    │ それを Ad Server (DFP / GAM) に渡して
    │ サーバ側の直販在庫と一緒に最終オークション
    ▼
[Publisher Ad Server]
```

「アドサーバが SSP に問い合わせるより前に、ブラウザ側で複数 SSP に同時入札させる」手法。
2014 年頃から普及。本リポジトリでは扱いません。

---

## このリポジトリでの実装

[Step 7](../steps/step07-rtb/) で:

- `mini-ad` が SSP 役として bid request を発行
- `mock-dsp` を別プロセスで起動し、ランダム入札させる
- Second-price で勝者を決定
- 結果を impression として記録
