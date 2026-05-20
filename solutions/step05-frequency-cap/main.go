// Step 05 — フリークエンシーキャップ (Redis)
//
// 参照実装: steps/step05-frequency-cap/README.md に対応する完成形コード。
// Step 04 のトラッキング (EventWriter / Dedup / async pipeline) を継承しつつ、
// uid cookie と FreqStore (Redis) を追加して 1 日あたりの imp 上限を実装する。
//
// 起動順:
//  1. Redis に Ping (失敗時は exit 1)
//  2. EventWriter 起動 (logs/events.jsonl)
//  3. HTTP server 起動
//
// graceful shutdown は 3 段階:
//  1. HTTP server を停止 (新規 imp が来なくなる)
//  2. EventWriter を flush
//  3. Redis client を close
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// =====================================================================
// Config
// =====================================================================

type Config struct {
	Addr            string
	LogLevel        slog.Level
	DefaultStrategy string
	RandomSeed      int64
	EventLogPath    string
	EventBufSize    int
	EventRingCap    int
	DedupTTL        time.Duration
}

func loadConfig() Config {
	cfg := Config{
		Addr:            ":8080",
		LogLevel:        slog.LevelInfo,
		DefaultStrategy: "weighted",
		EventLogPath:    "logs/events.jsonl",
		EventBufSize:    1024,
		EventRingCap:    256,
		DedupTTL:        10 * time.Minute,
	}
	if v := os.Getenv("ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("EVENT_LOG"); v != "" {
		cfg.EventLogPath = v
	}
	if v := os.Getenv("DEFAULT_STRATEGY"); v != "" {
		cfg.DefaultStrategy = v
	}
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	}
	if v := os.Getenv("RANDOM_SEED"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.RandomSeed)
	}
	return cfg
}

// =====================================================================
// main
// =====================================================================

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	seed := cfg.RandomSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	rng := mrand.New(mrand.NewSource(seed))

	// --- Redis (FreqStore) を最初に確認する ---
	freq, err := newFreqStoreFromEnv(context.Background())
	if err != nil {
		logger.Error("redis unavailable",
			slog.Any("err", err),
			slog.String("hint", "別ターミナルで `nix run .#redis` または `redis-server` を起動し、REDIS_ADDR/REDIS_URL を確認してください"),
		)
		os.Exit(1)
	}
	logger.Info("redis connected", slog.String("addr", resolveRedisAddr()))

	// --- EventWriter ---
	events, err := NewEventWriter(logger, cfg.EventLogPath, cfg.EventBufSize, cfg.EventRingCap)
	if err != nil {
		logger.Error("event writer init", slog.Any("err", err))
		_ = freq.Close()
		os.Exit(1)
	}

	dedup := NewDedup(cfg.DedupTTL)

	s := newServer(logger,
		defaultInventory(),
		NewSelectorRegistry(cfg.DefaultStrategy, rng),
		events,
		dedup,
		freq,
	)

	handler := Chain(RequestID, AccessLog(logger), Recover(logger))(s.routes())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting",
			slog.String("addr", cfg.Addr),
			slog.String("event_log", cfg.EventLogPath),
			slog.String("default_strategy", cfg.DefaultStrategy),
		)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown requested by signal")
	case err := <-serverErr:
		if err != nil {
			logger.Error("server crashed", slog.Any("err", err))
			_ = freq.Close()
			os.Exit(1)
		}
	}

	// 1) HTTP server stop
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", slog.Any("err", err))
		_ = srv.Close()
	}

	// 2) EventWriter flush
	flushCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := events.Close(flushCtx); err != nil {
		logger.Error("event writer close", slog.Any("err", err))
	}

	// 3) Redis client close
	if err := freq.Close(); err != nil {
		logger.Warn("redis close", slog.Any("err", err))
	}

	logger.Info("server stopped cleanly")
}
