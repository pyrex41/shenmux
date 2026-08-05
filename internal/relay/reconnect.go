package relay

import (
	"math/rand"
	"sync"
	"time"
)

type ConnectionState uint8

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateBackoff
)

// BackoffPolicy controls bounded reconnect delay.  Jitter is a fraction in
// [0,1], and is applied symmetrically around the exponential delay.
type BackoffPolicy struct {
	Initial time.Duration
	Maximum time.Duration
	Factor  float64
	Jitter  float64
	Rand    *rand.Rand
}

func (p BackoffPolicy) normalized() BackoffPolicy {
	if p.Initial <= 0 {
		p.Initial = time.Second
	}
	if p.Maximum <= 0 {
		p.Maximum = time.Minute
	}
	if p.Maximum < p.Initial {
		p.Maximum = p.Initial
	}
	if p.Factor < 1 {
		p.Factor = 2
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	}
	if p.Jitter > 1 {
		p.Jitter = 1
	}
	return p
}

func (p BackoffPolicy) Delay(attempt int) time.Duration {
	p = p.normalized()
	if attempt < 1 {
		attempt = 1
	}
	d := float64(p.Initial)
	for i := 1; i < attempt; i++ {
		d *= p.Factor
		if d >= float64(p.Maximum) {
			d = float64(p.Maximum)
			break
		}
	}
	if d > float64(p.Maximum) {
		d = float64(p.Maximum)
	}
	if p.Jitter > 0 {
		r := p.Rand
		if r == nil {
			r = rand.New(rand.NewSource(time.Now().UnixNano()))
		}
		// Symmetric jitter keeps the delay bounded by the policy maximum.
		d *= 1 + (r.Float64()*2-1)*p.Jitter
		if d < 0 {
			d = 0
		}
		// Exponential delay is clamped before jitter; clamp again afterwards so
		// jitter can never violate the policy's advertised maximum.
		if d > float64(p.Maximum) {
			d = float64(p.Maximum)
		}
	}
	return time.Duration(d)
}

// Reconnector is a small, concurrency-safe state machine.  Networking code
// drives it from callbacks, making reconnect behavior deterministic and easy
// to test without a live WebSocket server.
type Reconnector struct {
	mu      sync.Mutex
	policy  BackoffPolicy
	state   ConnectionState
	attempt int
}

func NewReconnector(policy BackoffPolicy) *Reconnector {
	return &Reconnector{policy: policy.normalized(), state: StateDisconnected}
}

func (r *Reconnector) State() ConnectionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Reconnector) Start() {
	r.mu.Lock()
	r.state = StateConnecting
	r.mu.Unlock()
}

func (r *Reconnector) Connected() {
	r.mu.Lock()
	r.state = StateConnected
	r.attempt = 0
	r.mu.Unlock()
}

func (r *Reconnector) Disconnected() {
	r.mu.Lock()
	r.state = StateDisconnected
	r.mu.Unlock()
}

// Failed records a failed attempt, enters backoff, and returns the delay until
// the next Start call.  The first failure uses Initial.
func (r *Reconnector) Failed() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempt++
	r.state = StateBackoff
	return r.policy.Delay(r.attempt)
}

func (r *Reconnector) Attempt() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempt
}
