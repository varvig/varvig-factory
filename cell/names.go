package cell

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Ref and note namespaces (CELL.md §2). These names are the interoperability
// surface: a cell that writes them somewhere else is not federating, it is
// keeping a private diary.
const (
	// CapabilitiesPrefix roots the per-cell static capabilities ref.
	CapabilitiesPrefix = "refs/factory/cells/"
	// AttemptPrefix roots the immutable attempt refs.
	AttemptPrefix = "refs/attempts/"
	// ClaimPrefix roots the advisory, TTL'd claim refs.
	ClaimPrefix = "refs/claims/"
	// PinPrefix roots retention requests. This is varvig's own pin namespace
	// (FEDERATION.md §4) — Factory adds no primitive of its own, it just names
	// what upstream should retain. The name shape below is varvig's too, not
	// Factory's: a pin written in a shape varvig cannot parse would still occupy
	// the namespace while failing to be recognised as a pin.
	PinPrefix = "refs/pins/"

	// NoteEvidence carries an attempt's evidence records (CELL.md §4.1).
	NoteEvidence = "factory/evidence"
	// NoteEnvironment carries the environment descriptors evidence points at.
	NoteEnvironment = "factory/environment"
	// NoteArtifact carries artifact-ref records for binary outputs (CELL.md §7).
	NoteArtifact = "factory/artifact"
	// NoteAgreement carries promotion-agreement observations (CELL.md §9).
	NoteAgreement = "factory/agreement"
)

// ValidID reports whether s is a well-formed cell id: lowercase alphanumeric
// and dashes, starting alphanumeric, at most 63 bytes (CELL.md §1). The same
// rule validates a scope-free path component, which is what a cell id has to be
// — it is interpolated into ref names, so a slash or a "." in one would let a
// misconfigured cell write outside its own namespace.
func ValidID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// CheckID returns a named error for an invalid cell id, so a misconfiguration
// fails at startup with the reason rather than at the first ref write with a
// confusing ref name.
func CheckID(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("cell: invalid cell id %q: want lowercase alphanumeric and dashes, starting alphanumeric, max 63 bytes", id)
	}
	return nil
}

// validTaskID accepts a varvig ticket id: a hash-shaped token with no path
// separators. Like the cell id it is interpolated into a ref name.
func validTaskID(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == ':' || c == '.':
			// ':' admits a labelled hash ("sha256:…"); '.' is admitted for the
			// same reason but ".." is rejected below.
		default:
			return false
		}
	}
	return !strings.Contains(s, "..")
}

// CapabilitiesRef is where a cell publishes its static capabilities.
func CapabilitiesRef(cellID string) (string, error) {
	if err := CheckID(cellID); err != nil {
		return "", err
	}
	return CapabilitiesPrefix + cellID + "/capabilities", nil
}

// AttemptRef names attempt n (1-based) by cellID at task. Attempt refs are
// immutable: a second attempt is a new n, never a new value at an existing one
// (CELL.md §2). That is what makes duplicate attempts survive a reconnect
// without a merge algorithm.
func AttemptRef(cellID, taskID string, n int) (string, error) {
	if err := CheckID(cellID); err != nil {
		return "", err
	}
	if !validTaskID(taskID) {
		return "", fmt.Errorf("cell: invalid task id %q", taskID)
	}
	if n < 1 {
		return "", fmt.Errorf("cell: attempt number must be >= 1, got %d", n)
	}
	return fmt.Sprintf("%s%s/%s/%d", AttemptPrefix, cellID, taskID, n), nil
}

// AttemptTaskPrefix is the ref prefix holding every attempt this cell has made
// at a task — used to find the next free attempt number and to answer "have I
// already attempted this?" (FACTORY.md §5.1).
func AttemptTaskPrefix(cellID, taskID string) (string, error) {
	if err := CheckID(cellID); err != nil {
		return "", err
	}
	if !validTaskID(taskID) {
		return "", fmt.Errorf("cell: invalid task id %q", taskID)
	}
	return fmt.Sprintf("%s%s/%s/", AttemptPrefix, cellID, taskID), nil
}

// ClaimRef names cellID's claim on task. Claims are per-cell refs, so two cells
// never contend for the same name — the CAS that matters happens on the attempt
// and on the promoted branch (CELL.md §5).
func ClaimRef(cellID, taskID string) (string, error) {
	if err := CheckID(cellID); err != nil {
		return "", err
	}
	if !validTaskID(taskID) {
		return "", fmt.Errorf("cell: invalid task id %q", taskID)
	}
	return ClaimPrefix + cellID + "/" + taskID, nil
}

// PinPeerPrefix enumerates this cell's pins.
//
// The peer id is hex-encoded because that is what varvig's own pin naming does
// (internal/pin): a peer id may contain characters a ref name may not, and
// encoding it keeps the ref name valid for every possible id rather than for the
// ids that happen to be safe.
func PinPeerPrefix(cellID string) (string, error) {
	if err := CheckID(cellID); err != nil {
		return "", err
	}
	return PinPrefix + hex.EncodeToString([]byte(cellID)) + "/", nil
}

// PinRef names a retention request by this cell, in varvig's pin ref shape:
//
//	refs/pins/<hex peer id>/<16 hex not_after>/<object hash>
//
// notAfter is mandatory, and that is varvig's rule rather than a Factory choice
// (FEDERATION.md §4): an expired pin stops being a GC root and is reclaimed, so
// a pin that never expired would be a permanent claim on another peer's disk.
// The expiry is also why releasing a pin is normally nothing a cell has to do —
// it lapses.
//
// objectHash is a varvig object hash in the hex form varvig prints, not a
// Factory-labelled hash: pins name varvig objects. An external artifact is not a
// varvig object and is therefore not pinnable — it is reachable through an
// artifact-ref, and retaining it is Factory's own business (CELL.md §7).
func PinRef(cellID string, notAfter int64, objectHash string) (string, error) {
	prefix, err := PinPeerPrefix(cellID)
	if err != nil {
		return "", err
	}
	if notAfter <= 0 {
		return "", fmt.Errorf("cell: pin on %s has no expiry; an unexpiring pin is a permanent claim on a peer's disk", short(objectHash))
	}
	if !isHex(objectHash) {
		return "", fmt.Errorf("cell: pin target %q is not a varvig object hash in hex form", objectHash)
	}
	return fmt.Sprintf("%s%016x/%s", prefix, notAfter, objectHash), nil
}

// ParsePinRef decomposes one of this cell's pin refs.
func ParsePinRef(ref string) (cellID string, notAfter int64, objectHash string, err error) {
	rest, ok := strings.CutPrefix(ref, PinPrefix)
	if !ok {
		return "", 0, "", fmt.Errorf("cell: not a pin ref: %q", ref)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", 0, "", fmt.Errorf("cell: malformed pin ref: %q", ref)
	}
	raw, derr := hex.DecodeString(parts[0])
	if derr != nil {
		return "", 0, "", fmt.Errorf("cell: malformed peer id in pin ref %q", ref)
	}
	na, perr := strconv.ParseInt(parts[1], 16, 64)
	if perr != nil {
		return "", 0, "", fmt.Errorf("cell: malformed expiry in pin ref %q", ref)
	}
	if !isHex(parts[2]) {
		return "", 0, "", fmt.Errorf("cell: malformed object hash in pin ref %q", ref)
	}
	return string(raw), na, parts[2], nil
}

func isHex(s string) bool {
	if s == "" || len(s)%2 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ParseAttemptRef decomposes an attempt ref into its parts. It is the inverse
// of AttemptRef and exists so a cell can read a *peer's* attempts out of a ref
// listing without pattern-guessing.
func ParseAttemptRef(ref string) (cellID, taskID string, n int, err error) {
	rest, ok := strings.CutPrefix(ref, AttemptPrefix)
	if !ok {
		return "", "", 0, fmt.Errorf("cell: not an attempt ref: %q", ref)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("cell: malformed attempt ref: %q", ref)
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &n); err != nil || n < 1 {
		return "", "", 0, fmt.Errorf("cell: malformed attempt number in %q", ref)
	}
	if !ValidID(parts[0]) || !validTaskID(parts[1]) {
		return "", "", 0, fmt.Errorf("cell: malformed attempt ref: %q", ref)
	}
	return parts[0], parts[1], n, nil
}
