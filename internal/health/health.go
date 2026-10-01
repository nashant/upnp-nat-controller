// Package health implements the liveness check: the process is live while
// its control loops keep completing passes, whatever the router does.
package health

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// missedPasses is how many intervals may pass without a beat (NFR-OPS-1).
const missedPasses = 5

// Tracker records loop passes. It is safe for concurrent use.
type Tracker struct {
	clock    clock.PassiveClock
	mu       sync.Mutex
	deadline time.Time
	armed    bool
}

// New returns a tracker that is healthy until the first Beat.
func New(clk clock.PassiveClock) *Tracker { return &Tracker{clock: clk} }

// Beat records a completed pass of a loop that runs every interval. The
// check fails once 5 × the longest interval seen passes with no beat.
func (t *Tracker) Beat(interval time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.clock.Now().Add(missedPasses * interval); d.After(t.deadline) {
		t.deadline = d
	}
	t.armed = true
}

// Check is a healthz.Checker.
func (t *Tracker) Check(_ *http.Request) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.armed {
		return nil
	}
	if now := t.clock.Now(); now.After(t.deadline) {
		return fmt.Errorf("control loop overdue by %s", now.Sub(t.deadline).Round(time.Second))
	}
	return nil
}
