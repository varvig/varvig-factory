package cell

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Multihash codes, from the multiformats registry, matching the subset varvig
// implements.
//
// Only these two exist here because only these two are needed: BLAKE3 is what
// varvig writes object ids with, and SHA2-256 is, in varvig's own words,
// "provided for interoperability" — which is exactly what this file uses it for.
const (
	// CodeSHA2_256 is the code Factory's own content hashes encode under.
	CodeSHA2_256 = 0x12
	// CodeBLAKE3 is varvig's default, seen on every object id.
	CodeBLAKE3 = 0x1e
)

// labelForCode maps a multihash code to the self-describing label Factory writes
// (CELL.md §4.3). A code with no label here is not one this build can render.
var labelForCode = map[uint64]string{
	CodeSHA2_256: "sha256",
	CodeBLAKE3:   "blake3",
}

// codeForLabel is the inverse.
var codeForLabel = map[string]uint64{
	"sha256": CodeSHA2_256,
	"blake3": CodeBLAKE3,
}

// digestSize is the digest length, in bytes, for each code. Both are 32.
var digestSize = map[uint64]int{
	CodeSHA2_256: 32,
	CodeBLAKE3:   32,
}

// ToMultihash converts a Factory labelled hash ("sha256:<64 hex>") into the
// multihash hex form varvig's CLI accepts.
//
// This conversion is why the algorithm choice in §4.3 was never load-bearing.
// Factory hashes artifact bytes with SHA-256 and labels the result in text;
// varvig encodes the algorithm *in the bytes* as `<uvarint code><uvarint
// length><digest>`. Both carry the same 32 digest bytes, so the conversion is a
// re-encoding and nothing else — no rehashing, no second algorithm, and no
// dependency, because SHA2-256 is a registered multihash code.
//
// The practical consequence is the one that matters: an artifact Factory has
// already hashed can be handed to `varvig tickets attach-artifact` as-is, which
// is what makes the artifact-ref a real object and therefore visible to
// `varvig gc --report-external`.
func ToMultihash(labelled string) (string, error) {
	label, digestHex, ok := strings.Cut(labelled, ":")
	if !ok {
		return "", fmt.Errorf("cell: %q is not a labelled hash; want <algorithm>:<hex>", labelled)
	}
	code, known := codeForLabel[label]
	if !known {
		return "", fmt.Errorf("cell: no multihash code for algorithm %q", label)
	}
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		return "", fmt.Errorf("cell: %q has a malformed digest: %w", labelled, err)
	}
	if want := digestSize[code]; len(digest) != want {
		// A short digest would still encode, and varvig would reject it with a
		// length mismatch at the far end. Catching it here names the actual
		// problem instead of surfacing it as a CLI error about someone else's
		// argument.
		return "", fmt.Errorf("cell: %s digest is %d bytes, want %d", label, len(digest), want)
	}

	var buf []byte
	buf = binary.AppendUvarint(buf, code)
	buf = binary.AppendUvarint(buf, uint64(len(digest)))
	buf = append(buf, digest...)
	return hex.EncodeToString(buf), nil
}

// FromMultihash converts a multihash hex — an object id or a content hash as
// varvig prints it — into Factory's labelled form.
//
// It accepts BLAKE3 as well as SHA2-256, because a content hash Factory reads
// back may have been written by a peer that hashed with varvig's default rather
// than Factory's. Refusing to read it would make Factory unable to see artifacts
// it did not produce, which is the opposite of what a federation needs.
func FromMultihash(mh string) (string, error) {
	raw, err := hex.DecodeString(mh)
	if err != nil {
		return "", fmt.Errorf("cell: %q is not hex: %w", mh, err)
	}
	code, n := binary.Uvarint(raw)
	if n <= 0 {
		return "", fmt.Errorf("cell: %q has no readable multihash code", mh)
	}
	length, m := binary.Uvarint(raw[n:])
	if m <= 0 {
		return "", fmt.Errorf("cell: %q has no readable digest length", mh)
	}
	digest := raw[n+m:]
	if uint64(len(digest)) != length {
		return "", fmt.Errorf("cell: %q declares a %d-byte digest but carries %d", mh, length, len(digest))
	}
	label, known := labelForCode[code]
	if !known {
		// An unknown algorithm is not an error to swallow. A hash this build
		// cannot name is one it cannot compare, and pretending otherwise would
		// put an unlabelled value into an environment or an evidence record.
		return "", fmt.Errorf("cell: unknown multihash code 0x%x in %q", code, mh)
	}
	return label + ":" + hex.EncodeToString(digest), nil
}

// IsMultihash reports whether s looks like a multihash hex this build can read.
// It is used to tell a varvig-shaped hash from a Factory-labelled one at a
// boundary where either may arrive.
func IsMultihash(s string) bool {
	_, err := FromMultihash(s)
	return err == nil
}
