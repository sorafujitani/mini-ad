# 参照実装 (solutions)

各 step の **動作する完成コード** です。学習の進め方は次のどちらでも構いません。

| 進め方 | 向いている人 |
|--------|-------------|
| `steps/stepNN-xxx/README.md` を読みながら自分で `main.go` を書く | 手を動かして理解したい |
| 詰まったら `solutions/stepNN-xxx/` を見る / `diff` する | まず全体像を掴みたい |

## 動かし方

```bash
nix develop
go run ./solutions/step01-hello-ad/
```

| 参照実装 | 必要なインフラ |
|----------|----------------|
| step01-hello-ad | なし |
| step02-ad-selection | なし |
| step05-frequency-cap | Redis (`nix run .#redis`) |

Step 6 以降の参照実装は順次追加予定。PostgreSQL はルート [README.md](../README.md) のセットアップ参照。

## ビルド確認

```bash
./scripts/verify.sh
```

すべての参照実装がコンパイルできることを CI やローカルで確認できます。
