# 02. 用語集

アドテクの議論で頻出する用語を、最小限の精度で並べます。

## イベント系

| 用語 | 意味 |
|------|------|
| **Ad Request** | ブラウザ → アドサーバ への「広告ください」リクエスト |
| **Impression (imp)** | 広告が実際に表示された 1 回。imp pixel で計測 |
| **Click** | ユーザーが広告をクリックした 1 回 |
| **Conversion (CV)** | クリック後にユーザーが目的の行動 (購入・登録など) を完了 |
| **Viewability** | imp のうち実際に画面に表示された割合 (IAB は 50% / 1 秒以上を基準) |

## 課金モデル

| 略称 | 課金単位 |
|------|----------|
| **CPM** (Cost per Mille) | 1000 imp あたり。Mille = 1000 (ラテン語) |
| **CPC** (Cost per Click) | 1 click あたり |
| **CPA** (Cost per Acquisition) | 1 conversion あたり |
| **CPV** (Cost per View) | 1 動画視聴あたり |
| **vCPM** (viewable CPM) | viewable な 1000 imp あたり |

## 効率指標

| 指標 | 定義 | 単位 |
|------|------|------|
| **CTR** | clicks / impressions | % |
| **CVR** | conversions / clicks | % |
| **eCPM** | 「実効 CPM」。配信単位を CPM 換算して比較するための値 | 通貨 / 1000 imp |
| **ROAS** | revenue / ad spend | 倍率 |
| **ROI** | profit / ad spend | % |

**eCPM** は特に重要：CPC 入札であろうと CPA 入札であろうと、最終的に「**この imp を出すといくら稼げるか**」を共通単位 (CPM) に揃えて比較できる。

```
eCPM = (期待収益 / 表示回数) × 1000
     CPC モデル: eCPM ≈ CPC × CTR × 1000
     CPA モデル: eCPM ≈ CPA × CTR × CVR × 1000
```

## 階層用語 (キャンペーン構造)

| 用語 | 階層 | 持つ情報 |
|------|------|---------|
| **Advertiser** | 1 | 広告主アカウント、請求単位 |
| **Order / Insertion Order (IO)** | 2 | 契約単位、総予算 |
| **Campaign** | 2-3 | 配信期間、テーマ |
| **LineItem (Ad Group)** | 3-4 | ターゲティング、入札額 |
| **Creative** | 4-5 | 実際の素材 (画像・動画・HTML) |

実際のシステムでは細部が違いますが、**ターゲティングは LineItem に持たせる** のが業界共通です。

## 配信制御

| 用語 | 意味 |
|------|------|
| **Frequency Cap** | 同一ユーザーへの表示回数上限 (例: 1 日 5 回まで) |
| **Recency Cap** | 直近に出した広告は一定時間出さない |
| **Pacing** | 予算を均等に消化する制御。ASAP / Even が代表的 |
| **Day Parting** | 時間帯による配信制限 (深夜は出さない、など) |
| **Targeting** | 配信対象の絞り込み (geo / device / context / audience) |
| **Retargeting** | 過去にサイト訪問したユーザーに再配信 |

## 取引・オークション系

| 用語 | 意味 |
|------|------|
| **Direct Deal** | Advertiser ↔ Publisher の直接契約配信 |
| **RTB** (Real-Time Bidding) | 1 imp ごとにオークション、~100ms で決着 |
| **PMP** (Private Marketplace) | 招待制 RTB、Direct + RTB の中間 |
| **Header Bidding** | クライアント側で複数 SSP に並列入札させる手法 |
| **First-price auction** | 入札額そのものを支払う |
| **Second-price auction** | 2 位の入札額 + 1 セントを支払う (理論上は誠実入札が最適戦略) |
| **Floor Price** | 最低落札価格。これ未満なら売らない |
| **Win Notice / nurl** | DSP が「自分が勝った」と通知される URL |
| **Loss Notice / lurl** | 「負けた」と通知される URL |

## ユーザー識別

| 用語 | 意味 |
|------|------|
| **Cookie** | ブラウザに保存される ID。3rd party cookie は段階廃止中 |
| **IDFA / GAID** | iOS / Android の広告 ID |
| **User ID Syncing** | 各社の cookie ID を相互変換する仕組み |
| **Universal ID** | 業界統一 ID (例: UID2.0, ID5) |
| **Privacy Sandbox** | Chrome が cookie 廃止後に提供する代替 API 群 |

## 品質・安全

| 用語 | 意味 |
|------|------|
| **IVT / Invalid Traffic** | ボットや fraud によって生じる不正トラフィック |
| **Brand Safety** | 不適切なコンテンツ横で広告が表示されないようにする |
| **Ad Verification** | 第三者による imp 計測・viewability 検証 |
| **DSA / DSAR** | GDPR / CCPA における本人開示請求 |
