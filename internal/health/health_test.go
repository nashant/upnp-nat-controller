package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func newTracker() (*Tracker, *clocktesting.FakeClock) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	return New(clk), clk
}

func TestHealth_HealthyBeforeFirstBeat(t *testing.T) { // not leader yet: nothing to watch
	tr, clk := newTracker()
	clk.Step(time.Hour)
	if err := tr.Check(nil); err != nil {
		t.Fatal(err)
	}
}

func TestHealth_FailsAfter5xIntervalWithoutPass(t *testing.T) { // NFR-OPS-1, S17, D11
	tr, clk := newTracker()
	tr.Beat("poller", 30*time.Second)
	clk.Step(150 * time.Second)
	if err := tr.Check(nil); err != nil {
		t.Fatalf("at exactly 5x: %v", err)
	}
	clk.Step(time.Second)
	if err := tr.Check(nil); err == nil {
		t.Fatal("healthy after 5x interval without a pass")
	}
	tr.Beat("poller", 30*time.Second)
	if err := tr.Check(nil); err != nil {
		t.Fatalf("after a pass: %v", err)
	}
}

func TestHealth_LongestIntervalWins(t *testing.T) {
	tr, clk := newTracker()
	tr.Beat("poller", 10*time.Minute) // IGD poll with a long pollingInterval
	tr.Beat("poller", 30*time.Second) // reconciler pass must not shorten the deadline
	clk.Step(49 * time.Minute)
	if err := tr.Check(nil); err != nil {
		t.Fatal(err)
	}
}

func TestHealth_RouterDownStillHealthy(t *testing.T) { // NFR-OPS-1
	// A pass that failed to reach the router is still a pass: the poller and
	// reconciler beat whatever the router does.
	tr, clk := newTracker()
	for i := 0; i < 20; i++ {
		tr.Beat("poller", 30*time.Second)
		clk.Step(30 * time.Second)
		if err := tr.Check(nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

func TestHealth_HTTP500WhenWedged(t *testing.T) { // S17
	tr, clk := newTracker()
	tr.Beat("poller", 30*time.Second)
	h := &healthz.Handler{Checks: map[string]healthz.Checker{"resync": tr.Check}}
	get := func() int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		return rr.Code
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("code %d", c)
	}
	clk.Step(5*30*time.Second + time.Second)
	if c := get(); c != http.StatusInternalServerError {
		t.Fatalf("code %d, want 500", c)
	}
}

func TestHealth_PollerDoesNotMaskWedgedReconciler(t *testing.T) { // review I2, S17
	tr, clk := newTracker()
	tr.Beat("reconciler", 30*time.Second)
	for i := 0; i < 10; i++ { // poller keeps running, reconciler is stuck
		clk.Step(30 * time.Second)
		tr.Beat("poller", 30*time.Second)
	}
	if err := tr.Check(nil); err == nil || !strings.Contains(err.Error(), "reconciler") {
		t.Fatalf("got %v, want reconciler overdue", err)
	}
}
