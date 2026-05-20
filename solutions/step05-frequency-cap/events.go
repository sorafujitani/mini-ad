// events.go — Event 型 / Dedup / EventWriter (async + ring buffer)
//
// Step 04 と同じ。本 step では変更なし。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type EventType string

const (
	EventAdRequest  EventType = "ad_request"
	EventImpression EventType = "impression"
	EventViewable   EventType = "viewable"
	EventClick      EventType = "click"
)

type Event struct {
	Type       EventType `json:"type"`
	ImpID      string    `json:"imp_id"`
	LineItemID string    `json:"line_item_id,omitempty"`
	Slot       SlotID    `json:"slot,omitempty"`
	BidCPM     int       `json:"bid_cpm,omitempty"`
	RequestID  string    `json:"request_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	IP         string    `json:"ip,omitempty"`
	UA         string    `json:"ua,omitempty"`
	UID        string    `json:"uid,omitempty"`
	Dest       string    `json:"dest,omitempty"`
}

// =====================================================================
// Dedup — TTL 付き in-memory set
// =====================================================================

type Dedup struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

func NewDedup(ttl time.Duration) *Dedup {
	d := &Dedup{m: make(map[string]time.Time), ttl: ttl}
	go d.gcLoop()
	return d
}

// Mark: 初出なら true (= 今回処理して OK)、既に見たことあるなら false。
func (d *Dedup) Mark(eventType EventType, impID string) bool {
	if impID == "" {
		return true
	}
	k := string(eventType) + ":" + impID
	d.mu.Lock()
	defer d.mu.Unlock()
	if exp, ok := d.m[k]; ok && time.Now().Before(exp) {
		return false
	}
	d.m[k] = time.Now().Add(d.ttl)
	return true
}

func (d *Dedup) gcLoop() {
	t := time.NewTicker(d.ttl / 2)
	defer t.Stop()
	for range t.C {
		d.mu.Lock()
		now := time.Now()
		for k, exp := range d.m {
			if now.After(exp) {
				delete(d.m, k)
			}
		}
		d.mu.Unlock()
	}
}

// =====================================================================
// EventWriter — async ファイル append + in-memory ring buffer
// =====================================================================

type EventWriter struct {
	logger *slog.Logger
	ch     chan Event
	done   chan struct{}

	mu   sync.Mutex
	ring []Event
	head int
	cap  int

	f *os.File
}

func NewEventWriter(logger *slog.Logger, path string, bufSize, ringCap int) (*EventWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &EventWriter{
		logger: logger,
		ch:     make(chan Event, bufSize),
		done:   make(chan struct{}),
		ring:   make([]Event, ringCap),
		cap:    ringCap,
		f:      f,
	}
	go w.run()
	return w, nil
}

// Submit: ホットパスから呼ぶ。chan が満杯なら捨てる。
func (w *EventWriter) Submit(e Event) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	select {
	case w.ch <- e:
	default:
		w.logger.Warn("event channel full, dropping event",
			slog.String("type", string(e.Type)),
			slog.String("imp_id", e.ImpID),
		)
	}
}

func (w *EventWriter) Close(ctx context.Context) error {
	close(w.ch)
	select {
	case <-w.done:
		return w.f.Close()
	case <-ctx.Done():
		return errors.New("event writer flush timed out")
	}
}

func (w *EventWriter) Recent(n int) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n > w.cap {
		n = w.cap
	}
	out := make([]Event, 0, n)
	for i := 0; i < w.cap && len(out) < n; i++ {
		idx := (w.head - 1 - i + w.cap) % w.cap
		if w.ring[idx].OccurredAt.IsZero() {
			break
		}
		out = append(out, w.ring[idx])
	}
	return out
}

func (w *EventWriter) run() {
	defer close(w.done)
	enc := json.NewEncoder(w.f)
	for e := range w.ch {
		if err := enc.Encode(e); err != nil {
			w.logger.Error("event write failed", slog.Any("err", err))
		}
		w.appendRing(e)
	}
}

func (w *EventWriter) appendRing(e Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ring[w.head] = e
	w.head = (w.head + 1) % w.cap
}
