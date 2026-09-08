package cell

import (
	"strings"
	"testing"
)

// TestEveryFactoryNamespaceNestsUnderOneRoot is the property the nesting exists
// for. A Factory concern added later must not land outside the root by
// accident: a stray top-level name is how two systems end up sharing a word
// while meaning different things, and this is the cheapest place to catch it.
//
// varvig's core reserves the same root and asserts the same property from its
// side, so the two agree on the spelling by construction rather than by
// someone remembering.
func TestEveryFactoryNamespaceNestsUnderOneRoot(t *testing.T) {
	owned := map[string]string{
		"CapabilitiesPrefix": CapabilitiesPrefix,
		"AttemptPrefix":      AttemptPrefix,
		"ClaimPrefix":        ClaimPrefix,
		"EnvelopePrefix":     EnvelopePrefix,
		"LeasePrefix":        LeasePrefix,
		"ReservationPrefix":  ReservationPrefix,
	}
	for name, p := range owned {
		if !strings.HasPrefix(p, Prefix) {
			t.Errorf("%s = %q, which does not nest under %q", name, p, Prefix)
		}
		if !strings.HasSuffix(p, "/") {
			t.Errorf("%s = %q, which is not a namespace prefix", name, p)
		}
	}
}

// TestPinsAreNotFactorys keeps the one deliberate exception honest. Pins are
// varvig's federation primitive — its GC root walk and pin handlers act on that
// name — so Factory requests retention there without owning the namespace, and
// moving it under the Factory root would break the core rather than tidy it.
func TestPinsAreNotFactorys(t *testing.T) {
	if strings.HasPrefix(PinPrefix, Prefix) {
		t.Errorf("PinPrefix = %q is nested under the Factory root; pins are varvig's", PinPrefix)
	}
	if PinPrefix != "refs/pins/" {
		t.Errorf("PinPrefix = %q, want varvig's own spelling", PinPrefix)
	}
}

// TestNoteNamespacesAreUnprefixed guards the other half of the naming: note
// namespaces are not refs and do not take the refs/ root, but they are still
// Factory's and still need one owner.
func TestNoteNamespacesAreUnprefixed(t *testing.T) {
	for _, ns := range []string{NoteEvidence, NoteEnvironment, NoteArtifact, NoteAgreement, NoteEffect} {
		if strings.HasPrefix(ns, "refs/") {
			t.Errorf("note namespace %q looks like a ref name", ns)
		}
		if !strings.HasPrefix(ns, "factory/") {
			t.Errorf("note namespace %q is not under Factory's own root", ns)
		}
	}
}
