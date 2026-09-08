package varvigcli

import "testing"

// TestCollapsedServesBothRolesFromOneReplica: the single-project configuration
// is legitimate, and the point of building it in one call is that a reader sees
// it is deliberate rather than inferring it from two identical wraps.
func TestCollapsedServesBothRolesFromOneReplica(t *testing.T) {
	v := NewFake("one")
	f, p := Collapsed(v)
	if f.Varvig != Varvig(v) || p.Varvig != Varvig(v) {
		t.Fatal("Collapsed must hand back the replica it was given, unchanged")
	}
	if _, err := f.PutBlob([]byte("x")); err != nil {
		t.Fatalf("the factory handle must be usable: %v", err)
	}
	if _, err := p.PutBlob([]byte("x")); err != nil {
		t.Fatalf("the project handle must be usable: %v", err)
	}
}

// TestCollapsedHandlesSeeEachOthersWrites is what makes the single-project
// configuration legitimate rather than merely convenient: the two handles are
// one repository, so a lease written through the factory handle is readable
// through the project handle.
//
// It matters because the collapsed mode is the one where cross-repo resolution
// is trivially satisfiable, and a test suite that ran only against it would
// never notice a call reaching for state the other repository holds. Stating the
// property here is what makes the two-replica tests elsewhere legible as the
// contrast they are.
func TestCollapsedHandlesSeeEachOthersWrites(t *testing.T) {
	f, p := Collapsed(NewFake("one"))
	id, err := f.PutBlob([]byte("a lease"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.UpdateRef("refs/factory/leases/mini-a/deploy", id, ""); err != nil {
		t.Fatal(err)
	}
	got, err := p.ResolveRef("refs/factory/leases/mini-a/deploy")
	if err != nil {
		t.Fatalf("the collapsed pair is one repository, so the project handle must see it: %v", err)
	}
	if got != id {
		t.Fatalf("resolved %s, want %s", got, id)
	}
}

// Both kinds embed Varvig, so each satisfies that interface and either can be
// passed anywhere a bare Varvig is wanted. That is intended — it is what lets
// the handles do ordinary repository work — and it is also precisely why the
// functions that write a lease take the concrete FactoryRepo rather than the
// interface: an interface parameter would accept either, and the mistake it
// would let through is resolved by spending money.
//
// These assertions fail to compile if the embedding is removed.
var (
	_ Varvig = FactoryRepo{}
	_ Varvig = ProjectRepo{}
)
