# 04. ターゲティング — 「誰に出すか」をどう決めるか

「広告を出す/出さない」のフィルタリング条件を **ターゲティング** と呼びます。
LineItem に紐づき、Candidate 抽出 (cf. [03. 配信フロー](03-delivery-flow.md)) の段階で適用されます。

---

## 大きく 4 種類

| 種類 | 何を見る | 例 |
|------|----------|-----|
| **Geographic** | IP・GPS から推定する地域 | 「東京都のみ」「日本以外除外」 |
| **Device / Tech** | User-Agent から推定する端末特性 | 「iOS のみ」「モバイルのみ」「Chrome 120+」 |
| **Contextual** | ページの内容そのもの | 「料理サイトのみ」「特定キーワードを含む記事」 |
| **Audience (Behavioral)** | ユーザー過去履歴・属性データ | 「30 代男性」「家電サイト訪問者」 |

このリポジトリの **Step 3** では Geographic と Device を扱います。Audience targeting は本格的にはユーザーグラフが必要なので深追いしません。

---

## マッチングの基本パターン

```
LineItem.targeting = {
  "countries":     ["JP", "US"],   ← OR
  "devices":       ["mobile"],     ← OR
  "keywords":      ["coffee"]      ← OR
  ...
}

request.context = {
  "country": "JP",
  "device":  "desktop",
  ...
}
```

- フィールド単位で **OR** (`countries` の中はどれか 1 つマッチすれば OK)
- フィールド間で **AND** (`countries` AND `devices` AND `keywords` 全部 OK 必要)
- 指定がないフィールドは **「制約なし」** とみなす

このルールは Google Ad Manager や Xandr (旧 AppNexus) などでも基本同じ。

---

## include / exclude

ターゲティングは「含める」だけでなく「除外する」も必要です:

```json
{
  "countries": { "include": ["JP"] },
  "categories": { "exclude": ["adult", "gambling"] }
}
```

`exclude` が 1 つでもヒットしたら **その時点で対象外** にする (ブランドセーフティ要件)。

---

## ジオの実装で知っておくこと

- IP → 国 のマッピングは **MaxMind GeoIP2** などのデータベースを使う
- アプリだと GPS や端末 OS 由来の国情報も使える
- VPN / プロキシで偽装されるので 100% 正確ではない
- 地域 ZIP code レベルになるとさらに精度が下がる

Step 3 では学習のために、リクエスト query param で `country=JP` を渡す簡易実装にしています。

---

## デバイス判定で知っておくこと

- 基本は `User-Agent` 文字列のパース
- Apple は UA を簡略化する方向 (iOS Safari は UA からバージョン取得困難に)
- Google は **User-Agent Client Hints** (`Sec-CH-UA-*` ヘッダー) を推進
- スマホ / タブレット / デスクトップ の 3 分類が最頻

Step 3 では `strings.Contains(ua, "Mobile")` レベルの簡易判定。実運用では `github.com/ua-parser/uap-go` のようなライブラリを使うのが普通です。

---

## ブール演算で表現するターゲティング DSL (発展)

商用システムだと、ターゲティングは「式」として保存されます:

```
(country IN [JP, US]) AND (device = mobile) AND NOT (category IN [adult])
```

これをそのまま評価できる「予測木 (prediction tree)」「BVM (boolean vector matching)」のような構造があり、数百万 LineItem を ms で評価する技術が研究されています。
このリポジトリでは扱いませんが、興味があれば *"AdTech matching engine"* で検索すると論文が出てきます。
