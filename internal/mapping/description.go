package mapping

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// MaxDescriptionLen is the longest description a router reliably keeps.
// miniupnpd on pf stores it in a rule label of PF_RULE_LABEL_SIZE (64)
// bytes including the NUL, so anything longer comes back truncated.
const MaxDescriptionLen = 63

const (
	// DefaultPrefix marks mappings made by this controller.
	DefaultPrefix = "unc/"
	// FormerPrefix is the prefix used before DefaultPrefix; mappings
	// carrying it are adopted.
	FormerPrefix = "upnp-nat-controller/"

	maxPrefixLen = 20
	hashLen      = 8 // hex characters
)

// Descriptions builds and recognises the port mapping descriptions that
// mark a mapping as belonging to a Service: Prefix + "<namespace>/<name>",
// shortened with a hash when that would exceed MaxDescriptionLen.
type Descriptions struct {
	Prefix string
	// Adopt lists older prefixes whose mappings are taken over.
	Adopt []string
}

// DefaultDescriptions is the controller's default naming.
var DefaultDescriptions = Descriptions{Prefix: DefaultPrefix, Adopt: []string{FormerPrefix}}

// Validate checks the prefix is short, non-empty printable ASCII.
func (d Descriptions) Validate() error {
	if d.Prefix == "" {
		return errors.New("description prefix must not be empty")
	}
	if len(d.Prefix) > maxPrefixLen {
		return fmt.Errorf("description prefix %q is longer than %d bytes", d.Prefix, maxPrefixLen)
	}
	for _, c := range d.Prefix {
		if c <= ' ' || c > '~' {
			return fmt.Errorf("description prefix %q must be printable ASCII without spaces", d.Prefix)
		}
	}
	return nil
}

// For returns the description for o's mappings.
func (d Descriptions) For(o Owner) string {
	return describe(d.Prefix, o)
}

func describe(prefix string, o Owner) string {
	body := o.Namespace + "/" + o.Name
	if len(prefix)+len(body) <= MaxDescriptionLen {
		return prefix + body
	}
	sum := sha256.Sum256([]byte(body))
	keep := MaxDescriptionLen - len(prefix) - 1 - hashLen
	return prefix + body[:keep] + "~" + hex.EncodeToString(sum[:])[:hashLen]
}

// Ownership says how a router mapping relates to a Service.
type Ownership int

// Ownership values.
const (
	NotOwned  Ownership = iota
	Current             // described with the current naming
	Adoptable           // ours under an older naming; rewrite it
)

// Owns classifies desc for owner o.
func (d Descriptions) Owns(o Owner, desc string) Ownership {
	if desc == d.For(o) {
		return Current
	}
	if desc == o.Namespace+"/"+o.Name { // Python controller
		return Adoptable
	}
	for _, p := range d.Adopt {
		if p != d.Prefix && desc == describe(p, o) {
			return Adoptable
		}
	}
	return NotOwned
}

// Parse returns the owner named by a description made with the current or
// an adopted prefix. For a hash-shortened description the names are the
// truncated ones.
func (d Descriptions) Parse(desc string) (namespace, name string, ok bool) {
	for _, p := range append([]string{d.Prefix}, d.Adopt...) {
		body, found := strings.CutPrefix(desc, p)
		if !found {
			continue
		}
		if i := len(body) - hashLen - 1; len(desc) == MaxDescriptionLen && i > 0 && body[i] == '~' && isHex(body[i+1:]) {
			namespace, name, _ = strings.Cut(body[:i], "/")
			return namespace, name, namespace != ""
		}
		namespace, name, found = strings.Cut(body, "/")
		if found && namespace != "" && name != "" && !strings.Contains(name, "/") {
			return namespace, name, true
		}
	}
	return "", "", false
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) == hashLen
}
