module github.com/sorafujitani/mini-ad

go 1.24

// 依存ライブラリは各 step で必要になったときに `go get` で追加してください。
// Step 5 — go get github.com/redis/go-redis/v9
// Step 6 — go get github.com/jackc/pgx/v5

require github.com/redis/go-redis/v9 v9.19.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
)
