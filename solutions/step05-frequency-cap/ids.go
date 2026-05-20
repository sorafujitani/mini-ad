// ids.go — imp_id 生成
//
// crypto/rand を使う = 推測困難。impression を後から偽装しにくい。
package main

import (
	"crypto/rand"
	"encoding/hex"
)

func newImpID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
