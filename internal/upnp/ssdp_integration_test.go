//go:build integration

package upnp_test

import (
	"testing"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

// TestSSDPSearcher_Smoke needs a real IGD on the LAN: go test -tags integration ./internal/upnp/
func TestSSDPSearcher_Smoke(t *testing.T) {
	locs, err := upnp.SSDPSearcher{}.Search(ctx)
	if err != nil || len(locs) == 0 {
		t.Fatalf("no IGD found: %v", err)
	}
	st, err := upnp.New(upnp.Config{}).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("found %+v", st)
}
