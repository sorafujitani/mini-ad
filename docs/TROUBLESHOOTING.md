# トラブルシューティング

ハンズオン中によくあるエラーと対処です。

## Go / ビルド

| 症状 | 原因 | 対処 |
|------|------|------|
| `go: cannot find main module` | リポジトリルート以外で `go run` している | `cd` で `mini-ad/` ルートに移動してから実行 |
| `package ... is not in std` (redis / pgx) | 依存未追加 | `go get` → `go mod tidy`（各 step の README「準備」参照） |
| Step 1 だけ動かない | `steps/step01-hello-ad/main.go` 未作成 | README に従って作成するか [`solutions/step01-hello-ad/`](../solutions/step01-hello-ad/) を参照 |

```bash
./scripts/verify.sh   # 参照実装がビルドできるか確認
```

## Redis (Step 5+)

| 症状 | 対処 |
|------|------|
| `redis ping failed` / connection refused | 別ターミナルで `nix run .#redis` |
| cap が効かない | `/ad` だけ叩いていないか確認。**imp pixel (`/imp`)** でカウントする設計 |
| curl で uid が変わる | `curl -c cookies.txt -b cookies.txt` で cookie を保持 |

## PostgreSQL (Step 6+)

| 症状 | 対処 |
|------|------|
| `PGDATA not initialized` | `nix run .#postgres-init` |
| `connection refused` | `nix run .#postgres` でサーバ起動 |
| テーブルが空 | `nix run .#db-create` で schema + サンプルデータ投入 |
| データをリセットしたい | `nix run .#reset` のあと init → postgres → db-create をやり直す |

`psql` は devShell 内で `PGHOST` / `PGUSER` / `PGDATABASE` が自動設定されます。

## 動作確認のヒント

- 構造化ログは stderr に JSON で出ます: `go run ... 2>&1 | jq -c`
- `LOG_LEVEL=debug` で DEBUG 以上を表示（ハンドラが `DebugContext` を使っている箇所のみ増える）
- Step 2 以降: `RANDOM_SEED=42` で乱数を固定し、分布テストを再現可能にする
