package iface

import (
	"errors"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

func registry(t *testing.T) varvigcli.FactoryRepo {
	t.Helper()
	f, _ := varvigcli.Collapsed(varvigcli.NewFake("coordination"))
	return f
}

var board = map[string]any{"gerber": "string", "quantity": "integer"}

// TestTheHashIsTheObjectId is the property the registry rests on: a resolve is
// an ordinary blob read at the same id the capability names, so the registry
// cannot disagree with the hash it is keyed by.
func TestTheHashIsTheObjectId(t *testing.T) {
	f := registry(t)
	hash, err := Publish(f, "pcb-fabrication@1", board)
	if err != nil {
		t.Fatal(err)
	}
	if !cell.IsMultihash(hash) {
		t.Fatalf("published hash %q is not an object hash", hash)
	}
	body, err := f.ReadBlob(hash)
	if err != nil {
		t.Fatalf("the hash does not name a readable object: %v", err)
	}
	viaRegistry, err := Resolve(f, hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(viaRegistry) {
		t.Error("resolving through the registry gave something other than the object the hash names")
	}
}

// TestFieldOrderDoesNotChangeTheHash: two callers describing the same interface
// with their fields in a different order must publish one hash, or the
// hash-matching rule is defeated from the inside.
func TestFieldOrderDoesNotChangeTheHash(t *testing.T) {
	f := registry(t)
	a, err := Publish(f, "one", map[string]any{"gerber": "string", "quantity": "integer"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Publish(f, "two", map[string]any{"quantity": "integer", "gerber": "string"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("the same interface published two hashes: %s and %s", a, b)
	}
}

// TestAnUnknownHashIsNotAnEmptySchema. An empty schema reads as "this requires
// nothing", which is the most permissive possible answer to a question asked
// immediately before spending money.
func TestAnUnknownHashIsNotAnEmptySchema(t *testing.T) {
	f := registry(t)
	stranger, err := Publish(registry(t), "elsewhere", board)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(f, stranger); !errors.Is(err, ErrUnknown) {
		t.Fatalf("a hash this factory never published should be ErrUnknown, got %v", err)
	}
	if Known(f, stranger) {
		t.Error("Known said yes to a hash this factory does not hold")
	}
}

// TestRepublishingIsIdempotent: a cell declares its interfaces on every start,
// and that must not be a compare-and-swap failure.
func TestRepublishingIsIdempotent(t *testing.T) {
	f := registry(t)
	first, err := Publish(f, "pcb-fabrication@1", board)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Publish(f, "pcb-fabrication@1", board)
	if err != nil {
		t.Fatalf("republishing the same schema must not fail: %v", err)
	}
	if first != again {
		t.Errorf("republishing changed the hash: %s then %s", first, again)
	}
}

// TestAnAliasCanBeRepointed, and doing so changes only what a person types.
func TestAnAliasCanBeRepointed(t *testing.T) {
	f := registry(t)
	v1, err := Publish(f, "pcb-fabrication@1", board)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Publish(f, "pcb-fabrication@1", map[string]any{
		"gerber": "string", "quantity": "integer", "finish": "string",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("a different schema published the same hash")
	}
	if got, _ := ByAlias(f, "pcb-fabrication@1"); got != v2 {
		t.Errorf("the alias points at %s, want the new schema %s", got, v2)
	}
	// The old hash still resolves. An action bound to it is bound to what it
	// was bound to, which is the whole reason capabilities name hashes.
	if _, err := Resolve(f, v1); err != nil {
		t.Errorf("re-pointing an alias made an earlier interface unresolvable: %v", err)
	}
}

// TestAnEmptySchemaIsRefused: a schema requiring nothing describes nothing.
func TestAnEmptySchemaIsRefused(t *testing.T) {
	f := registry(t)
	for _, schema := range []any{map[string]any{}, nil} {
		if _, err := Publish(f, "empty", schema); err == nil {
			t.Errorf("publishing %v should be refused", schema)
		}
	}
}

func TestListIsSortedAndNamesAliases(t *testing.T) {
	f := registry(t)
	for _, alias := range []string{"zeta@1", "alpha@1", "mu@2"} {
		if _, err := Publish(f, alias, map[string]any{"field": alias}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := List(f)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Alias)
	}
	if strings.Join(got, ",") != "alpha@1,mu@2,zeta@1" {
		t.Errorf("registry = %v, want the aliases sorted", got)
	}
}
