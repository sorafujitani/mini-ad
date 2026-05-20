// freq.go — FreqStore (Redis) + uid cookie
//
// Step 05 で新規追加。1 日あたり (line_item, uid) で imp 回数をカウントする。
// キー設計: freq:{line_item}:{uid}:{yyyymmdd}
//   - 日付を含めるので翌日は別キー → 自然リセット
//   - INCR + EXPIRE を 1 RTT のパイプラインで実行
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const uidCookieName = "mini_ad_uid"

type FreqStore struct {
	rdb *redis.Client
}

// resolveRedisAddr: REDIS_ADDR > REDIS_URL > 127.0.0.1:6379 の優先順で解決。
// REDIS_URL は redis://host:port[/db] の形を想定し、host:port 部分を抜き出す。
func resolveRedisAddr() string {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		return addr
	}
	if u := os.Getenv("REDIS_URL"); u != "" {
		s := strings.TrimPrefix(u, "redis://")
		s = strings.TrimPrefix(s, "rediss://")
		// path / userinfo を切り捨てて host:port のみを残す
		if i := strings.Index(s, "/"); i >= 0 {
			s = s[:i]
		}
		if i := strings.Index(s, "@"); i >= 0 {
			s = s[i+1:]
		}
		if s != "" {
			return s
		}
	}
	return "127.0.0.1:6379"
}

func newFreqStoreFromEnv(ctx context.Context) (*FreqStore, error) {
	addr := resolveRedisAddr()
	rdb := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return &FreqStore{rdb: rdb}, nil
}

func (s *FreqStore) Close() error {
	if s == nil || s.rdb == nil {
		return nil
	}
	return s.rdb.Close()
}

func (s *FreqStore) key(uid, lineItem string) string {
	return fmt.Sprintf("freq:%s:%s:%s", lineItem, uid, time.Now().UTC().Format("20060102"))
}

// Count: 当日の imp 回数を返す。未記録キーは 0 として正規化。
func (s *FreqStore) Count(ctx context.Context, uid, lineItem string) (int, error) {
	v, err := s.rdb.Get(ctx, s.key(uid, lineItem)).Int()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	return v, nil
}

// Inc: INCR + EXPIRE (24h) を pipeline で 1 RTT に。
func (s *FreqStore) Inc(ctx context.Context, uid, lineItem string) error {
	k := s.key(uid, lineItem)
	pipe := s.rdb.TxPipeline()
	pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// ensureUID: 既存 cookie があれば返す。無ければ発行して Set-Cookie する。
func ensureUID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(uidCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	uid := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     uidCookieName,
		Value:    uid,
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 30,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return uid
}
