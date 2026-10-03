// Package health implements the liveness check: the process is live while
// each of its control loops keeps completing passes, whatever the router does.
package health

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// missedPasses is how many intervals a loop may go without a beat (NFR-OPS-1).
const missedPasses = 5

// Tracker records passes per named loop. It is safe for concurrent use.
type Tracker struct {
	clock     clock.PassiveClock
	mu        sync.Mutex
	deadlines map[string]time.Time
}

// New returns a tracker; a loop is only checked after its first Beat.
func New(clk clock.PassiveClock) *Tracker {
	return &Tracker{clock: clk, deadlines: map[string]time.Time{}}
}

// Beat records a completed pass of loop, which runs every interval. The
// check fails once 5 × the longest interval seen for loop passes with no beat.
func (t *Tracker) Beat(loop string, interval time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.clock.Now().Add(missedPasses * interval); d.After(t.deadlines[loop]) {
		t.deadlines[loop] = d
	}
}

// Check is a healthz.Checker. It fails if any loop is overdue.
func (t *Tracker) Check(_ *http.Request) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock.Now()
	var late []string
	for loop, d := range t.deadlines {
		if now.After(d) {
			late = append(late, fmt.Sprintf("%s overdue by %s", loop, now.Sub(d).Round(time.Second)))
		}
	}
	if len(late) > 0 {
		sort.Strings(late)
		return fmt.Errorf("control loops stalled: %v", late)
	}
	return nil
}
