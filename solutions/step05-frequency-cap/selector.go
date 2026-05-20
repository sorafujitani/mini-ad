// selector.go — Selector / 3 戦略 / Registry
//
// Step 02-03 と同じ。default = weighted。
package main

import (
	mrand "math/rand"
	"sync"
)

type Selector interface {
	Name() string
	Pick(items []LineItem) (LineItem, bool)
}

// --- random ---

type RandomSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex
}

func NewRandomSelector(rng *mrand.Rand) *RandomSelector { return &RandomSelector{rng: rng} }
func (s *RandomSelector) Name() string                  { return "random" }
func (s *RandomSelector) Pick(items []LineItem) (LineItem, bool) {
	if len(items) == 0 {
		return LineItem{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return items[s.rng.Intn(len(items))], true
}

// --- weighted ---

type WeightedSelector struct {
	rng *mrand.Rand
	mu  sync.Mutex
}

func NewWeightedSelector(rng *mrand.Rand) *WeightedSelector { return &WeightedSelector{rng: rng} }
func (s *WeightedSelector) Name() string                    { return "weighted" }
func (s *WeightedSelector) Pick(items []LineItem) (LineItem, bool) {
	if len(items) == 0 {
		return LineItem{}, false
	}
	total := 0
	for _, li := range items {
		if li.BidCPM > 0 {
			total += li.BidCPM
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if total == 0 {
		return items[s.rng.Intn(len(items))], true
	}
	r := s.rng.Intn(total)
	cum := 0
	for _, li := range items {
		if li.BidCPM <= 0 {
			continue
		}
		cum += li.BidCPM
		if r < cum {
			return li, true
		}
	}
	return items[len(items)-1], true
}

// --- highest ---

type HighestBidSelector struct{}

func (HighestBidSelector) Name() string { return "highest" }
func (HighestBidSelector) Pick(items []LineItem) (LineItem, bool) {
	if len(items) == 0 {
		return LineItem{}, false
	}
	best := items[0]
	for _, li := range items[1:] {
		if li.BidCPM > best.BidCPM {
			best = li
		}
	}
	return best, true
}

// --- registry ---

type SelectorRegistry struct {
	defaultName string
	selectors   map[string]Selector
}

func NewSelectorRegistry(defaultName string, rng *mrand.Rand) *SelectorRegistry {
	return &SelectorRegistry{
		defaultName: defaultName,
		selectors: map[string]Selector{
			"random":   NewRandomSelector(rng),
			"weighted": NewWeightedSelector(rng),
			"highest":  HighestBidSelector{},
		},
	}
}

func (r *SelectorRegistry) Resolve(name string) Selector {
	if name == "" {
		name = r.defaultName
	}
	if s, ok := r.selectors[name]; ok {
		return s
	}
	return r.selectors[r.defaultName]
}
