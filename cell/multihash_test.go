package cell

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestToMultihashMatchesVarvigsEncoding pins the wire form against a value
// verified by running the real `varvig tickets attach-artifact`, which accepted
// this exact string and printed it back from `varvig tickets artifacts`.
//
// The constant is the point of the test. A refactor that changed the uvarint
// framing would still round-trip through this file's own decoder and look
// correct; only a value the real binary has accepted catches that.
func TestToMultihashMatchesVarvigsEncoding(t *testing.T) {
	const (
		labelled = "sha256:46d4fece1941224acfda42351d2b496a5c902e9f9e1dd6fc81e5254907c40665"
		want     = "122046d4fece1941224acfda42351d2b496a5c902e9f9e1dd6fc81e5254907c40665"
	)
	got, err := ToMultihash(labelled)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ToMultihash = %s, want %s", got, want)
	}
	// The digest bytes are carried through untouched — this is a re-encoding, not
	// a rehash. That is what lets Factory hand varvig a hash it computed itself.
	if !strings.HasSuffix(got, strings.TrimPrefix(labelled, "sha256:")) {
		t.Fatalf("the digest was altered: %s", got)
	}
	// 0x12 = sha2-256, 0x20 = 32 bytes.
	if !strings.HasPrefix(got, "1220") {
		t.Fatalf("prefix = %s, want the sha2-256/32-byte header 1220", got[:4])
	}
}

func TestMultihashRoundTrip(t *testing.T) {
	sum := sha256.Sum256([]byte("pretend-image-bytes"))
	labelled := HashAlgorithm + ":" + hex.EncodeToString(sum[:])

	mh, err := ToMultihash(labelled)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromMultihash(mh)
	if err != nil {
		t.Fatal(err)
	}
	if back != labelled {
		t.Fatalf("round trip: %s -> %s -> %s", labelled, mh, back)
	}
}

func TestFromMultihashReadsVarvigsBlake3(t *testing.T) {
	// An object id, or a content hash a peer wrote with varvig's default. Factory
	// must be able to read it: refusing would make Factory blind to artifacts it
	// did not produce, which is the opposite of what a federation needs.
	const objectID = "1e20127191478cea5c2cdc6ca46a180bb5d3c63afeca88b617aad9d2468443fae561"
	got, err := FromMultihash(objectID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "blake3:") {
		t.Fatalf("FromMultihash = %s, want a blake3: label", got)
	}
	// And it is labelled honestly rather than relabelled as Factory's own
	// algorithm — a blake3 digest called "sha256" would compare as equal to
	// nothing and look like a Factory hash while being none.
	if strings.HasPrefix(got, HashAlgorithm+":") {
		t.Fatalf("a blake3 digest was mislabelled as %s", HashAlgorithm)
	}
}

func TestToMultihashRejectsMalformedInput(t *testing.T) {
	sum := sha256.Sum256(nil)
	full := hex.EncodeToString(sum[:])
	cases := []struct{ name, in string }{
		{"no label", full},
		{"unknown algorithm", "md5:" + full},
		{"not hex", "sha256:zzzz"},
		{"short digest", "sha256:abcd"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ToMultihash(tc.in); err == nil {
				t.Fatalf("ToMultihash(%q) was accepted", tc.in)
			}
		})
	}
}

func TestFromMultihashRejectsMalformedInput(t *testing.T) {
	cases := []struct{ name, in string }{
		{"not hex", "zzzz"},
		{"empty", ""},
		// 0x99 is not a code this build knows. An unnameable hash is one it
		// cannot compare, so it must not be silently accepted.
		{"unknown code", "9920" + strings.Repeat("ab", 32)},
		{"length disagrees with payload", "1220abcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FromMultihash(tc.in); err == nil {
				t.Fatalf("FromMultihash(%q) was accepted", tc.in)
			}
		})
	}
}

func TestIsMultihashDistinguishesTheTwoForms(t *testing.T) {
	sum := sha256.Sum256(nil)
	labelled := HashAlgorithm + ":" + hex.EncodeToString(sum[:])
	mh, err := ToMultihash(labelled)
	if err != nil {
		t.Fatal(err)
	}
	if !IsMultihash(mh) {
		t.Fatal("a multihash was not recognised as one")
	}
	if IsMultihash(labelled) {
		t.Fatal("a Factory-labelled hash was mistaken for a multihash")
	}
}
