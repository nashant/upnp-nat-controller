package mapping

import (
	"strings"
	"testing"
)

func TestDescriptions_For_Short(t *testing.T) {
	d := Descriptions{Prefix: "unc/"}
	if got := d.For(Owner{Namespace: "traefik", Name: "public-traefik"}); got != "unc/traefik/public-traefik" {
		t.Fatalf("got %q", got)
	}
}

func TestDescriptions_For_LongIsTruncatedWithHash(t *testing.T) { // pf labels hold 63 bytes
	d := Descriptions{Prefix: "unc/"}
	o := Owner{Namespace: strings.Repeat("n", 40), Name: strings.Repeat("s", 40)}
	got := d.For(o)
	if len(got) > MaxDescriptionLen {
		t.Fatalf("len %d > %d: %q", len(got), MaxDescriptionLen, got)
	}
	if !strings.HasPrefix(got, "unc/") || !strings.Contains(got, "~") {
		t.Fatalf("want prefix and hash marker, got %q", got)
	}
	if again := d.For(o); again != got {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}
	other := d.For(Owner{Namespace: o.Namespace, Name: strings.Repeat("s", 39) + "t"})
	if other == got {
		t.Fatalf("owners differing only past the cut collide: %q", got)
	}
}

func TestDescriptions_For_ExactlyAtLimitNotHashed(t *testing.T) {
	d := Descriptions{Prefix: "unc/"}
	// 4 (prefix) + 29 + 1 + 29 = 63
	o := Owner{Namespace: strings.Repeat("a", 29), Name: strings.Repeat("b", 29)}
	if got := d.For(o); len(got) != 63 || strings.Contains(got, "~") {
		t.Fatalf("got %q (len %d)", got, len(got))
	}
}

func TestDescriptions_Owns(t *testing.T) {
	d := Descriptions{Prefix: "unc/", Adopt: []string{"upnp-nat-controller/"}}
	o := Owner{Namespace: "traefik", Name: "public-traefik"}
	cases := map[string]Ownership{
		"unc/traefik/public-traefik":                 Current,
		"upnp-nat-controller/traefik/public-traefik": Adoptable, // previous default prefix
		"traefik/public-traefik":                     Adoptable, // Python controller
		"unc/traefik/other":                          NotOwned,
		"upnp-nat-controller/traefik/other":          NotOwned,
		"Xbox":                                       NotOwned,
		"":                                           NotOwned,
	}
	for desc, want := range cases {
		if got := d.Owns(o, desc); got != want {
			t.Errorf("Owns(%q) = %v, want %v", desc, got, want)
		}
	}
	long := Owner{Namespace: strings.Repeat("n", 40), Name: strings.Repeat("s", 40)}
	if got := d.Owns(long, d.For(long)); got != Current {
		t.Fatalf("hashed description not recognised as current: %v", got)
	}
	if got := d.Owns(long, (Descriptions{Prefix: "upnp-nat-controller/"}).For(long)); got != Adoptable {
		t.Fatalf("hashed old-prefix description not adoptable: %v", got)
	}
}

func TestDescriptions_Parse(t *testing.T) {
	d := Descriptions{Prefix: "unc/", Adopt: []string{"upnp-nat-controller/"}}
	for desc, want := range map[string][2]string{
		"unc/media/plex":                 {"media", "plex"},
		"upnp-nat-controller/media/plex": {"media", "plex"},
	} {
		ns, name, ok := d.Parse(desc)
		if !ok || ns != want[0] || name != want[1] {
			t.Errorf("Parse(%q) = %q %q %v", desc, ns, name, ok)
		}
	}
	for _, bad := range []string{"", "Xbox", "media/plex", "unc/", "unc/media", "unc//plex", "unc/media/", "unc/a/b/c"} {
		if _, _, ok := d.Parse(bad); ok {
			t.Errorf("Parse(%q) ok, want !ok", bad)
		}
	}
	long := Owner{Namespace: strings.Repeat("n", 40), Name: strings.Repeat("s", 40)}
	if _, _, ok := d.Parse(d.For(long)); !ok {
		t.Fatal("hashed description should still parse as ours")
	}
}

func TestDescriptions_Validate(t *testing.T) {
	for _, p := range []string{"unc/", "k8s-", "x"} {
		if err := (Descriptions{Prefix: p}).Validate(); err != nil {
			t.Errorf("prefix %q: %v", p, err)
		}
	}
	for _, p := range []string{"", "has space/", strings.Repeat("p", 21), "tab\t/", "ü/"} {
		if err := (Descriptions{Prefix: p}).Validate(); err == nil {
			t.Errorf("prefix %q: want error", p)
		}
	}
}
