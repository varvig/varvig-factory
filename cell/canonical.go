// Package cell is the cell contract (CELL.md) in code: identity, ref and note
// names, capabilities, evidence, the environment descriptor and its hash, and
// claims. It is the one package in this module with no dependencies on any
// other, because it is the part a peer built years from now still has to agree
// with (FACTORY.md §2).
//
// Nothing here runs, claims, spends, or talks to a network. Everything here is
// a shape, a name, or a hash rule. That separation is deliberate: the loop is
// replaceable and the contract is not.
package cell

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Canonical encodes v as canonical JSON per CELL.md §4.3: object keys sorted by
// byte order, no insignificant whitespace, no trailing newline, numbers
// verbatim as the Go encoder rendered them, and strings with minimal escaping
// (HTML escaping off — it is legal but not shortest, and turning it off here
// keeps a locator URI readable in the note a human eventually reads).
//
// The implementation deliberately round-trips through a generic tree rather
// than relying on struct field declaration order. Declaration order *is*
// deterministic in Go, which is exactly the trap: it would make canonical bytes
// depend on the source layout of this module, so reordering two fields during a
// refactor would silently change every environment hash in a federation. Going
// through sorted map keys makes the encoding a property of the data.
func Canonical(v any) ([]byte, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers stay as their source literal: decoding into float64 and
	// re-encoding would turn a size or a unix timestamp into a value that
	// happens to round-trip today and stops doing so above 2^53.
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("cell: canonicalize: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// marshal encodes v with HTML escaping disabled and without the trailing
// newline json.Encoder appends.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("cell: marshal: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(t.String())
	case string:
		s, err := marshal(t)
		if err != nil {
			return err
		}
		buf.Write(s)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			s, err := marshal(k)
			if err != nil {
				return err
			}
			buf.Write(s)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("cell: canonicalize: unexpected %T", v)
	}
	return nil
}

// HashAlgorithm is the label prefix on every hash this module writes. It is
// self-describing so that written state stays unambiguous when a second
// algorithm exists (CELL.md §4.3) — the label is the contract, the algorithm
// behind it is not.
const HashAlgorithm = "sha256"

// Hash renders bytes as "<algorithm>:<hex>".
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return HashAlgorithm + ":" + hex.EncodeToString(sum[:])
}

// CanonicalHash canonicalizes v and hashes the result. Two invocations over
// equal data always produce the same string; that is the property cross-cell
// comparison rests on (CELL.md §4.3, FACTORY.md §9.4).
func CanonicalHash(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	return Hash(b), nil
}
