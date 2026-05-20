# 03. 広告配信フロー — リクエスト 1 本を追いかける

「ad request が来たときアドサーバは何をするか」を、`mini-ad` の Step 8 完成形を想定して時系列で追います。

---

## ステージ別の処理

```
[Browser]                              [Ad Server (mini-ad)]
   |                                          |
   |  GET /ad?slot=top&...                    |
   |  Cookie: uid=abc123                       |
   |  User-Agent: ...                          |
   |----------------------------------------> |
   |                                          |
   |              (1) Ad Request 受信
   |              (2) リクエスト解析
   |                  - User-Agent → device
   |                  - IP → country
   |                  - slot, page URL
   |                  - uid (cookie)
   |              (3) Candidate 抽出 (DB)
   |                  - 配信期間内
   |                  - 予算残あり
   |                  - status=active
   |                  - targeting 一致
   |              (4) Frequency 過去 N 回チェック (Redis)
   |                  - cap オーバーは除外
   |              (5) Pacing チェック
   |                  - 当日の予算ペース内か
   |              (6) Auction / 順位付け
   |                  - eCPM 高い順
   |              (7) Creative を 1 つ選ぶ
   |              (8) Tracking URL を埋め込んだ広告タグを生成
   |              (9) Frequency カウンタを +1 (Redis)
   |              (10) (非同期で) impression log を書く
   |                                          |
   |  200 OK                                   |
   |  { creative: {...}, imp_url, click_url } |
   | <--------------------------------------- |
   |                                          |
   |  画像を <img> で表示                       |
   |  imp_url を非同期で GET                    |
   | ---------------------------------------> |  ← impression 計測
   |                                          |
   |  クリックすると                            |
   |  click_url を経由して LP へリダイレクト    |
   | ---------------------------------------> |  ← click 計測
   |                                          |
```

---

## 各ステージの仕事

### (1)(2) 受信と解析

- HTTP リクエスト自体は単なる GET。
- `User-Agent` や送信元 IP は **ヒント** であって信用しすぎない（簡単に偽装できる）。
- `slot` パラメータで「どの枠か」を伝えるのが標準。
- セッション継続のため **uid cookie** を発行 (なければ初回で発行する)。

### (3) Candidate 抽出

DB から「今このリクエストに出せるかもしれない LineItem」を絞り込む。
SQL でいうと:

```sql
SELECT li.*, c.*
FROM line_items li
JOIN campaigns c ON c.id = li.campaign_id
WHERE c.status = 'active'
  AND li.status = 'active'
  AND c.starts_at <= now() AND c.ends_at >= now()
  AND (li.targeting->'countries' ? $country OR NOT li.targeting ? 'countries')
  AND ...
```

ここで「**マッチング**」と呼ばれる処理が走る。本番では数百万件の LineItem を ms 単位で絞り込むので、転置インデックスやインメモリストアを使うことが多い。

### (4) Frequency Cap チェック

Redis のような高速 KV ストアに `freq:{uid}:{campaign_id}:{yyyymmdd}` のようなキーで imp 回数を保存。上限超過しているものを除外する。

詳しくは [Step 5](../steps/step05-frequency-cap/) で。

### (5) Pacing

「キャンペーン予算 1000$、24 時間で 1000$ を均等に使いたい」のような制約。
単純な実装としては `(現在時刻までに使うべき額) - (実際に使った額)` のずれを見て、ずれが大きすぎたら一時的に止める。

詳しくは [07. ペーシングと予算](07-pacing-budget.md)。

### (6) Auction / 順位付け

候補が複数残った場合、**eCPM の高い順** に並べる（収益最大化）。
RTB の場合は外部 DSP に bid request を送って入札を集める。詳しくは [06. オークションと RTB](06-auction.md)。

### (7)(8) Creative 選択とタグ生成

選ばれた LineItem に紐付く Creative が複数あれば、A/B テストや均等配信ロジックで 1 つ選ぶ。
レスポンスには **トラッキング URL** を埋め込む。

### (9)(10) 副作用

- Frequency カウンタの +1 は同期で（次の判定に影響するため）
- impression ログ書き込みは非同期で OK（応答を待たせない）

---

## なぜ「リクエスト時にすべてやる」のか

「広告枠ごとに事前に広告を割り当てておいて、リクエスト時には表示するだけ」じゃダメなの？

ダメ。**配信判断は imp ごとに最新情報で決めたい** から：

- 1 秒前まで余っていた予算が他リクエストで使い切られているかも
- 同じユーザーが直前に同じ広告を見たかも (frequency)
- 外部 DSP の入札意欲は数百 ms 単位で変わる

このため、アドサーバは「**ほぼ stateless にスケールしつつ、共有状態 (予算・frequency) は Redis 等で高速参照**」という典型構成になります。
