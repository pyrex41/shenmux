// Package transport defines the V2 session transport selection boundary.
// Implementations may use the authenticated relay or an optional direct path;
// callers never depend on a particular network topology.
package transport

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type Kind string

const (
	KindRelay  Kind = "relay"
	KindDirect Kind = "direct"
	// KindTailscale is an explicit direct tailnet path. It is ordered with
	// direct endpoints but retained as a separate kind for policy/telemetry.
	KindTailscale Kind = "tailscale"
)

type Endpoint struct {
	Kind Kind
	URL  string
}

type Session interface {
	Send(context.Context, []byte) error
	Recv(context.Context) ([]byte, error)
	Close() error
}

type Dialer interface {
	Dial(context.Context, Endpoint) (Session, error)
}

// EndpointResolver discovers transport candidates at connection time. Direct
// transports use this boundary so peer addresses can change without being
// persisted in session configuration.
type EndpointResolver interface {
	Resolve(context.Context) ([]Endpoint, error)
}

type StaticEndpoints []Endpoint

func (e StaticEndpoints) Resolve(context.Context) ([]Endpoint, error) {
	return append([]Endpoint(nil), e...), nil
}

type Health struct {
	mu      sync.Mutex
	entries map[string]healthEntry
}

type healthEntry struct {
	Failures int
	Until    time.Time
}

func NewHealth() *Health { return &Health{entries: make(map[string]healthEntry)} }

func (h *Health) RecordSuccess(endpoint Endpoint) {
	h.mu.Lock()
	delete(h.entries, key(endpoint))
	h.mu.Unlock()
}

func (h *Health) RecordFailure(endpoint Endpoint, now time.Time) {
	h.mu.Lock()
	entry := h.entries[key(endpoint)]
	entry.Failures++
	delay := time.Second << min(entry.Failures-1, 6)
	if delay > time.Minute {
		delay = time.Minute
	}
	entry.Until = now.Add(delay)
	h.entries[key(endpoint)] = entry
	h.mu.Unlock()
}

func (h *Health) Available(endpoint Endpoint, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.entries[key(endpoint)]
	return !ok || !now.Before(entry.Until)
}

func key(e Endpoint) string { return string(e.Kind) + "\x00" + e.URL }
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Selector prefers a healthy direct endpoint, then falls back to relay. It
// never returns a direct endpoint that has recently failed unless all options
// are unhealthy, ensuring relay remains the safe availability path.
type Selector struct{ Health *Health }

func (s Selector) Order(endpoints []Endpoint, now time.Time) []Endpoint {
	result := append([]Endpoint(nil), endpoints...)
	health := s.Health
	if health == nil {
		health = NewHealth()
	}
	sort.SliceStable(result, func(i, j int) bool {
		iAvail, jAvail := health.Available(result[i], now), health.Available(result[j], now)
		if iAvail != jAvail {
			return iAvail
		}
		if result[i].Kind != result[j].Kind {
			return result[i].Kind == KindDirect || result[i].Kind == KindTailscale
		}
		return false
	})
	return result
}

func Connect(ctx context.Context, dialer Dialer, selector Selector, endpoints []Endpoint) (Session, Endpoint, error) {
	if dialer == nil {
		return nil, Endpoint{}, errors.New("transport dialer is nil")
	}
	if len(endpoints) == 0 {
		return nil, Endpoint{}, errors.New("no transport endpoints")
	}
	if selector.Health == nil {
		selector.Health = NewHealth()
	}
	var last error
	for _, endpoint := range selector.Order(endpoints, time.Now()) {
		session, err := dialer.Dial(ctx, endpoint)
		if err == nil {
			selector.Health.RecordSuccess(endpoint)
			return session, endpoint, nil
		}
		selector.Health.RecordFailure(endpoint, time.Now())
		last = err
	}
	return nil, Endpoint{}, last
}

// ConnectResolved discovers optional direct endpoints, appends the relay
// fallback, and applies the normal health-based selection. Discovery failure
// does not make the relay unavailable.
func ConnectResolved(ctx context.Context, dialer Dialer, selector Selector, direct EndpointResolver, fallback ...Endpoint) (Session, Endpoint, error) {
	endpoints := make([]Endpoint, 0, len(fallback)+1)
	var resolveErr error
	if direct != nil {
		resolved, err := direct.Resolve(ctx)
		if err != nil {
			resolveErr = err
		} else {
			endpoints = append(endpoints, resolved...)
		}
	}
	endpoints = append(endpoints, fallback...)
	if len(endpoints) == 0 {
		if resolveErr != nil {
			return nil, Endpoint{}, resolveErr
		}
		return nil, Endpoint{}, errors.New("no transport endpoints")
	}
	return Connect(ctx, dialer, selector, endpoints)
}
