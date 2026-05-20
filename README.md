# mini-ad

広告配信に使われる **アドサーバ (Ad Server)** の基本を、Go で最小構成を組み立てながら学ぶハンズオン教材。

「ブラウザに広告が出るまで」「どうやって配信先を選ぶか」「インプレッションやクリックをどう数えるか」「予算や配信頻度をどう制御するか」「RTB はなぜ必要か」を、座学と動くコードの両輪で理解することを目的とします。

---

## このリポジトリの構成

```
mini-ad/
├── docs/        座学コーナー（用語・登場人物・配信フローなど）
├── steps/       ハンズオン (Step 1 → Step 8)
├── infra/       DB スキーマなど共通インフラ資材
├── flake.nix    Nix flake (Go / Redis / PostgreSQL を一式提供)
└── README.md    このファイル
```

各 step は **独立した main パッケージ** として動かせます。
前の step のコードに新しい概念を 1 つだけ足していく構成です。

---

## 学習ロードマップ

| Step | テーマ | 学ぶこと | 主な技術 |
|------|--------|---------|----------|
| [01](steps/step01-hello-ad/) | 最小のアドサーバ | 広告タグ / ad request / creative の概念 | Go net/http のみ |
| [02](steps/step02-ad-selection/) | 広告選択ロジック | 在庫 (inventory) と選定アルゴリズム (random / weighted) | 〃 |
| [03](steps/step03-targeting/) | ターゲティング | ジオ・デバイス・コンテンツによる絞り込み | 〃 |
| [04](steps/step04-tracking/) | トラッキング | impression pixel / click redirect / ログ設計 | 〃 |
| [05](steps/step05-frequency-cap/) | フリークエンシーキャップ | ユーザー識別 (cookie) と Redis でのカウント | Go + **Redis** |
| [06](steps/step06-campaign/) | キャンペーン管理 | Campaign / LineItem / Creative の階層と永続化 | Go + **PostgreSQL** |
| [07](steps/step07-rtb/) | RTB と入札 | OpenRTB 風 bid request / 第二価格オークション | Go + PostgreSQL |
| [08](steps/step08-reporting/) | ペーシングとレポーティング | 予算消化・スムージング・集計 | Go + PostgreSQL |

座学を先に読みたい人は [`docs/00-overview.md`](docs/00-overview.md) から。

---

## セットアップ

### 必要なもの

- [Nix](https://nixos.org/download.html)（flake が有効化されていること: `experimental-features = nix-command flakes`）

それだけ。Go / Redis / PostgreSQL は Nix が用意します。

### 開発シェルに入る

```bash
nix develop
```

これで以下が PATH に入ります：

- `go` (1.22)
- `gopls`
- `redis-server` / `redis-cli`
- `postgres` / `psql` / `initdb` / `createdb`
- `sqlite3`
- `jq` / `curl` / `httpie`

環境変数も自動でセットされます（`PGHOST`, `PGUSER`, `PGDATABASE`, `REDIS_URL` など）。

### Redis を起動（Step 5 以降で必要）

別ターミナルで：

```bash
nix run .#redis
```

データは `./.dev/redis/` に保存されます。

### PostgreSQL を起動（Step 6 以降で必要）

初回のみ：

```bash
nix run .#postgres-init      # initdb (PGDATA=./.dev/postgres)
nix run .#postgres &         # サーバー起動（バックグラウンド）
nix run .#db-create          # miniad DB 作成 + schema.sql 投入
```

2 回目以降は `nix run .#postgres` のみで OK。

データは `./.dev/postgres/` に保存され、`.gitignore` 済みです。

### 各 step を動かす

```bash
go mod tidy                            # 依存ダウンロード（初回 / Step 5 以降）
go run ./steps/step01-hello-ad/        # http://localhost:8080
```

---

## ハンズオンの進め方

各 step の `README.md` には **実装するコード本体が段階分けで掲載されています**。`main.go` ファイル自体は意図的に置いていません — 各 step の README を読みながら、自分の `steps/stepNN-xxx/main.go` に書き起こしてください（写経 / 理解しながら手で打つ / コピペ、お好みで）。

1. **その step の `README.md` を読む**
   - 「このステップで学ぶこと」「何を作るか」「実装」を上から順に
   - 関連する座学 (`docs/`) も同時に読むと理解が深まる
2. **`steps/stepNN-xxx/main.go` を新規作成**
   - README の `### A. ... ### B. ...` の Go コードブロックを上から順に書いていく
   - 各ブロックの解説で「**なぜそう書くか**」を確認しながら進める
3. **動かす・URL を叩く・ログを観察**
   - README の「動作確認」コマンドで挙動を検証
   - 「実験してみよう」で挙動を変えてみる
4. **次の step へ**
   - 前の step を `cp -r` で複製して、新しい概念だけ追記する進め方を推奨

---

## ライセンス / 注意

学習用途のシンプルな実装です。本物のアドサーバが備えるべき要件（DSAR・ID 連携・viewability・brand safety・配信遅延 < 100ms 制約 など）は意図的に削っています。理解の足場として読み、必要に応じて公式仕様（[IAB OpenRTB](https://iabtechlab.com/standards/openrtb/) など）も参照してください。
