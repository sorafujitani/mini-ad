# 01. アドテクエコシステム — 登場人物の地図

アドテクは登場人物が多くて、初見では「なぜこんなにレイヤーがあるのか」が掴みにくい領域です。
**「お金とリクエストの流れ」** を 1 本の軸にして整理します。

---

## メインキャスト

| 略称 | フルネーム | 役割 | このリポジトリで触れる Step |
|------|-----------|------|---------------------------|
| **Publisher** | Publisher (媒体社) | 広告枠を持つサイト・アプリ運営者 | Step 1〜 |
| **Advertiser** | Advertiser (広告主) | 広告を出したい企業 | Step 6〜 |
| **Ad Server (Publisher-side)** | First-party Ad Server | 媒体社が広告配信を管理するサーバ | **本リポジトリの主役** |
| **SSP** | Supply-Side Platform | 媒体社の枠を複数の DSP に売る取引所インターフェース | (Step 7 で擬似的に登場) |
| **DSP** | Demand-Side Platform | 広告主が複数の SSP に対して入札する側 | Step 7 |
| **Ad Exchange** | Ad Exchange | SSP と DSP を仲介する取引市場 | (概念のみ) |
| **DMP** | Data Management Platform | 第三者データを使って広告主・媒体社にオーディエンスを提供 | (扱わない) |
| **CDP** | Customer Data Platform | 自社データを統合してターゲティングに使う | (扱わない) |

---

## お金の流れ

```
       広告費 ($)
Advertiser  ─────►  DSP  ─────►  Ad Exchange / SSP  ─────►  Publisher
                     ▲                                            ▲
                     │ (DSP 手数料)              (SSP 手数料)     │
                     └────────────────────────────────────────────┘
```

広告主が払う 100 円が、媒体社の手元に届くまでに **DSP / Exchange / SSP の手数料が引かれる** のがこの業界の構造。手数料の不透明さ ("ad tech tax") は古典的な問題です。

---

## リクエスト（imp 1 回分）の流れ

```
[Browser] -- ad request --> [Publisher Ad Server] 
                                  │
                                  │ (直販在庫があれば即返す)
                                  │ (なければ次へ)
                                  ▼
                              [SSP / Exchange]
                                  │
                                  │ bid request
                                  ▼
                      ┌──── [DSP A] ────┐
                      ├──── [DSP B] ────┤   ← 100ms 程度で全 DSP が入札
                      └──── [DSP C] ────┘
                                  │
                          highest bid wins
                                  │
                                  ▼
                              [SSP / Exchange]
                                  │
                                  ▼
                          [Publisher Ad Server]
                                  │
                                  ▼
                              [Browser]  ← 広告タグ返却
```

「ad request」と「bid request」は別物だと意識すると整理しやすいです：

- **Ad Request**: ブラウザ → アドサーバ。1 回。
- **Bid Request**: アドサーバ (SSP) → 複数 DSP。並列に複数。

---

## First-party / Third-party Ad Server

| | First-party | Third-party |
|---|---|---|
| 誰が運営 | Publisher | Advertiser / 第三者 |
| 主目的 | 枠の販売管理 | クリエイティブ管理・計測 |
| 例 | Google Ad Manager | Sizmek, Flashtalking |
| ドメインの cookie | ファースト | サード |

**サードパーティ Cookie の段階的廃止** (Chrome の Privacy Sandbox 等) によって、Third-party Ad Server のビジネスは構造変化を迫られている、というのが今のホットトピック。

---

## このリポジトリのスコープ

```
┌─────────────────────────────────────────────────┐
│                Publisher's site                  │
│  ┌──────────────┐                                │
│  │ ad slot div  │                                │
│  └──────┬───────┘                                │
└─────────┼────────────────────────────────────────┘
          │ ad request
          ▼
┌─────────────────────────────────────────────────┐
│ mini-ad  (Publisher-side Ad Server)              │ ← Step 1-8 で作る
│   - 在庫 (Campaign/LineItem/Creative)            │
│   - ターゲティング                                │
│   - frequency cap                                │
│   - tracking                                     │
│   - 簡易 RTB (bid request を発行する側)           │
└─────────────────┬───────────────────────────────┘
                  │ bid request (Step 7)
                  ▼
┌─────────────────────────────────────────────────┐
│ mock-dsp  (Demand-side のモック)                  │ ← Step 7 で並走させる
└─────────────────────────────────────────────────┘
```
