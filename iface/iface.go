// Package iface is the interface registry: the thing an interface hash points
// into.
//
// A capability reference already binds to the interface *hash* rather than the
// alias, which is the half that matters for safety — two factories may hold the
// same alias without agreeing who owns the name, so matching on the hash is what
// catches the collision, and the hash is in the idempotency key so re-pointing
// an alias cannot make a new action look like an old one (CELL.md §8.2).
//
// What was missing is what the hash points *into*. A hash with nothing behind it
// answers "is this the same interface as that one" and nothing else. It cannot
// answer "what does this action actually require", which is the question an
// operator asks when an order is refused and the question a connector asks
// before deciding it can serve a capability at all.
//
// # The hash is the object id
//
// Publishing stores the canonical schema as a varvig object, and the id that
// comes back *is* the interface hash. That is the property the registry rests
// on: anyone holding the object can recompute the hash and check it, and a
// resolve is an ordinary blob read rather than a lookup in something that could
// disagree with the hash it is keyed by. A registry whose contents could drift
// from their own identifiers would be worse than none.
//
// # The alias is a convenience, never authority
//
// refs/factory/interfaces/<alias> points at the schema, so a person can type a
// name. Nothing in the effectful path reads it. Re-pointing an alias changes
// what a human types and nothing about what a lease bounds or what an
// idempotency key covers.
//
// # It lives in the coordination replica
//
// Interface schemas are factory-wide (CELL.md §2.1): what "pcb-fabrication@1"
// requires is a fact about the factory, not about any one codebase, and a
// per-project registry would let two projects disagree about the same hash's
// meaning while both looking correct.
package iface

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// ErrUnknown is returned for a hash the registry does not hold.
var ErrUnknown = errors.New("iface: unknown interface")

// Publish stores a schema and points an alias at it, returning the interface
// hash.
//
// The schema is canonicalized first, so two callers describing the same
// interface with their fields in a different order publish the same hash rather
// than two hashes that mean one thing — which is the collision the whole
// hash-matching rule exists to prevent, arriving from the inside.
func Publish(f varvigcli.FactoryRepo, alias string, schema any) (string, error) {
	if strings.TrimSpace(alias) == "" {
		return "", errors.New("iface: publishing needs an alias")
	}
	body, err := cell.Canonical(schema)
	if err != nil {
		return "", fmt.Errorf("iface: canonicalizing %q: %w", alias, err)
	}
	if err := validSchema(body); err != nil {
		return "", fmt.Errorf("iface: %q: %w", alias, err)
	}
	hash, err := f.PutBlob(body)
	if err != nil {
		return "", err
	}
	name, err := cell.InterfaceRef(alias)
	if err != nil {
		return "", err
	}
	old, err := f.ResolveRef(name)
	if err != nil && !errors.Is(err, varvigcli.ErrNoRef) {
		return "", err
	}
	if old == hash {
		// Publishing the same schema twice is not a change and must not be a
		// compare-and-swap failure: an idempotent publish is what lets a cell
		// declare its interfaces on every start.
		return hash, nil
	}
	if err := f.UpdateRef(name, hash, old); err != nil {
		return "", fmt.Errorf("iface: pointing %s at %s: %w", name, hash, err)
	}
	return hash, nil
}

// Resolve returns the schema an interface hash names.
//
// A hash the registry does not hold is ErrUnknown rather than an empty schema.
// An empty schema would read as "this interface requires nothing", which is the
// most permissive possible answer to a question asked immediately before
// spending money.
func Resolve(f varvigcli.FactoryRepo, hash string) ([]byte, error) {
	if !cell.IsMultihash(hash) {
		return nil, fmt.Errorf("iface: %q is not an object hash", hash)
	}
	body, err := f.ReadBlob(hash)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, hash)
	}
	if err := validSchema(body); err != nil {
		return nil, fmt.Errorf("iface: %s holds something that is not a schema: %w", hash, err)
	}
	return body, nil
}

// Known reports whether a hash resolves, for the common case of a caller that
// only needs to refuse rather than read.
func Known(f varvigcli.FactoryRepo, hash string) bool {
	_, err := Resolve(f, hash)
	return err == nil
}

// Entry is one registered alias and what it currently points at.
type Entry struct {
	Alias string `json:"alias"`
	Hash  string `json:"hash"`
}

// List returns the registry, sorted by alias.
//
// A malformed entry is reported rather than skipped: an alias whose ref will not
// parse is a name somebody meant to register, and dropping it silently turns a
// registry that is wrong into a registry that looks short.
func List(f varvigcli.FactoryRepo) ([]Entry, error) {
	refs, err := f.Refs()
	if err != nil {
		return nil, err
	}
	var out []Entry
	var bad []string
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, cell.InterfacePrefix) {
			continue
		}
		alias, perr := cell.ParseInterfaceRef(r.Name)
		if perr != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, perr))
			continue
		}
		out = append(out, Entry{Alias: alias, Hash: r.Hash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	if len(bad) > 0 {
		return out, fmt.Errorf("iface: %d unreadable registry refs: %s", len(bad), strings.Join(bad, "; "))
	}
	return out, nil
}

// ByAlias resolves an alias to the hash it currently points at.
//
// Callers that then act on it should carry the *hash* onwards, not the alias:
// resolving once and acting on the result is the difference between an action
// bound to an interface and one bound to whatever the name means later.
func ByAlias(f varvigcli.FactoryRepo, alias string) (string, error) {
	name, err := cell.InterfaceRef(alias)
	if err != nil {
		return "", err
	}
	hash, err := f.ResolveRef(name)
	if err != nil {
		return "", fmt.Errorf("%w: alias %q", ErrUnknown, alias)
	}
	return hash, nil
}

// validSchema keeps the registry to things a reader can interpret. It is a
// shape check, not a schema language: what an interface *means* is between the
// ticket that names it and the connector that serves it, and inventing a
// dialect here would put this module in the middle of that agreement.
func validSchema(body []byte) error {
	if len(body) == 0 {
		return errors.New("an empty schema requires nothing, which is not a description of anything")
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return fmt.Errorf("a schema must be a JSON object: %w", err)
	}
	if len(obj) == 0 {
		return errors.New("a schema with no fields requires nothing, which is not a description of anything")
	}
	return nil
}
