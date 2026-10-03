package main

import (
	"testing"
	"time"
)

func TestFlags_Defaults(t *testing.T) { // §4, M8
	o, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.ResyncInterval != 30*time.Second || o.LeaseDuration != 3600 || o.SOAPTimeout != 5*time.Second || o.FinalizerTimeout != 10*time.Minute {
		t.Fatalf("defaults %+v", o)
	}
	if !o.LeaderElect || o.MetricsAddr != ":8080" || o.ProbeAddr != ":8081" || o.IGDURL != nil {
		t.Fatalf("defaults %+v", o)
	}
	if o.DescriptionPrefix != "unc/" {
		t.Fatalf("description prefix %q", o.DescriptionPrefix)
	}
	if o.RateLimit != 5 || o.RateBurst != 10 {
		t.Fatalf("rate %v/%d", o.RateLimit, o.RateBurst)
	}
}

func TestFlags_Overrides(t *testing.T) {
	o, err := parseFlags([]string{
		"--igd-url=http://192.168.1.1:2189/rootDesc.xml", "--resync-interval=1m", "--lease-duration=0",
		"--soap-timeout=2s", "--finalizer-timeout=1m", "--leader-elect=false", "--rate-limit=1", "--rate-burst=2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.IGDURL.String() != "http://192.168.1.1:2189/rootDesc.xml" || o.ResyncInterval != time.Minute || o.LeaseDuration != 0 ||
		o.SOAPTimeout != 2*time.Second || o.FinalizerTimeout != time.Minute || o.LeaderElect || o.RateLimit != 1 || o.RateBurst != 2 {
		t.Fatalf("got %+v", o)
	}
}

func TestFlags_Invalid(t *testing.T) {
	for _, args := range [][]string{
		{"--igd-url=::bad"},
		{"--igd-url=ftp://x/"},
		{"--resync-interval=0s"},
		{"--soap-timeout=0s"},
		{"--lease-duration=-1"},
		{"--no-such-flag"},
		{"--description-prefix="},
		{"--description-prefix=has space/"},
		{"--lease-duration=60"},  // ≤ 2 × 30s resync: expires between passes
		{"--resync-interval=1h"}, // default 3600s lease < 2 × 1h

	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v): want error", args)
		}
	}
}

func TestFlags_DescriptionPrefixAndPermanentLease(t *testing.T) {
	o, err := parseFlags([]string{"--description-prefix=k8s/", "--resync-interval=1h", "--lease-duration=0"})
	if err != nil {
		t.Fatal(err)
	}
	if o.DescriptionPrefix != "k8s/" || o.LeaseDuration != 0 {
		t.Fatalf("got %+v", o)
	}
}
