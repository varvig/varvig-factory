package authority

import (
	"errors"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

func fake(t *testing.T) *varvigcli.Fake {
	t.Helper()
	return varvigcli.NewFake("test")
}

func TestEnvelopeAndLeaseRoundTrip(t *testing.T) {
	v := fake(t)
	env := envelope()

	if _, err := PublishEnvelope(v, env, ""); err != nil {
		t.Fatalf("publishing an envelope: %v", err)
	}
	got, envHash, err := LoadEnvelope(v, env.Overseer)
	if err != nil {
		t.Fatalf("loading it back: %v", err)
	}
	if got.Overseer != env.Overseer || len(got.Ceilings) != len(env.Ceilings) {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if envHash == "" {
		t.Fatal("a load handed back no hash to CAS against")
	}

	// The hash a load hands back is what a later publish CASes against, so a
	// second publish from a stale read is refused rather than overwriting.
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	if _, err := PublishLease(v, l, ""); err != nil {
		t.Fatalf("issuing a lease: %v", err)
	}
	back, leaseHash, err := LoadLease(v, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatalf("loading the lease: %v", err)
	}

	// Settlement: spend recorded against the value that was read.
	back.Spent = 320
	if _, err := PublishLease(v, back, leaseHash); err != nil {
		t.Fatalf("settling spend: %v", err)
	}
	// The same settlement replayed from the same stale hash is refused, which is
	// what stops two settlements from losing one of the amounts.
	back.Spent = 400
	_, err = PublishLease(v, back, leaseHash)
	if !errors.Is(err, varvigcli.ErrCAS) {
		t.Fatalf("a stale settlement was accepted or failed wrongly: %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), cell.LeasePrefix) {
		t.Fatalf("the refusal does not name the ref: %v", err)
	}
}

func TestMissingEnvelopeIsNotAnEmptyOne(t *testing.T) {
	// No envelope means no authority to spend. An empty value would read as
	// "validated, with no ceilings", which is exactly the malformed state §8.1
	// refuses.
	_, _, err := LoadEnvelope(fake(t), "overseer-a")
	if !errors.Is(err, varvigcli.ErrNoRef) {
		t.Fatalf("a missing envelope returned %v, want ErrNoRef", err)
	}
	_, _, err = LoadLease(fake(t), "mini-a", "pcb-fabrication@1")
	if !errors.Is(err, varvigcli.ErrNoRef) {
		t.Fatalf("a missing lease returned %v, want ErrNoRef", err)
	}
}

func TestLeasesAcrossCellsIsWhatExposureNeeds(t *testing.T) {
	v := fake(t)
	for _, l := range []Lease{
		lease("mini-a", "pcb-fabrication@1", 2000),
		lease("mini-b", "pcb-fabrication@1", 1000),
		lease("mini-b", "human-contract@1", 500),
	} {
		if _, err := PublishLease(v, l, ""); err != nil {
			t.Fatalf("issuing %s: %v", l, err)
		}
	}

	all, err := Leases(v, "")
	if err != nil {
		t.Fatalf("listing leases: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %d leases, want 3", len(all))
	}
	exp := Exposure(all)
	if exp["pcb-fabrication@1"] != 3000 || exp["human-contract@1"] != 500 {
		t.Fatalf("exposure = %v", exp)
	}

	// A cell can also ask about only its own, which is all it can act on.
	mine, err := Leases(v, "mini-b")
	if err != nil {
		t.Fatalf("listing one cell's leases: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("listed %d leases for mini-b, want 2", len(mine))
	}
}

func TestALeaseUnderTheWrongRefIsReportedNotSummed(t *testing.T) {
	// A lease object filed under another cell's ref is either a bug or an attempt
	// to borrow authority. Summing it under the wrong holder would misreport who
	// can spend what, so it is refused loudly.
	v := fake(t)
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	body, err := cell.Canonical(l)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.PutBlob(body)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := cell.LeaseRef("mini-b", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.UpdateRef(wrong, id, ""); err != nil {
		t.Fatal(err)
	}

	got, err := Leases(v, "")
	if err == nil {
		t.Fatal("a lease filed under the wrong cell's ref was accepted")
	}
	if !strings.Contains(err.Error(), "mini-b") || !strings.Contains(err.Error(), "mini-a") {
		t.Fatalf("the refusal does not name both sides of the mismatch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the mismatched lease was still counted: %v", got)
	}
}

func TestReclaimRefusesALeaseWithSpend(t *testing.T) {
	v := fake(t)
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	l.ReclaimAfter = now.Unix()
	l.Spent = 1
	hash, err := PublishLease(v, l, "")
	if err != nil {
		t.Fatal(err)
	}

	past := func() int64 { return now.Unix() + 86400 }
	if err := Reclaim(v, l, hash, past); err == nil {
		t.Fatal("a lease with recorded spend was reclaimed; that risks a double-spend")
	}
	if _, _, err := LoadLease(v, "mini-a", "pcb-fabrication@1"); err != nil {
		t.Fatalf("the refused reclaim removed the lease anyway: %v", err)
	}

	// Unspent and past its timeout: collectable.
	clean := l
	clean.Spent, clean.Ordered = 0, 0
	hash, err = PublishLease(v, clean, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reclaim(v, clean, hash, past); err != nil {
		t.Fatalf("an unspent, timed-out lease was not reclaimable: %v", err)
	}
	if _, _, err := LoadLease(v, "mini-a", "pcb-fabrication@1"); !errors.Is(err, varvigcli.ErrNoRef) {
		t.Fatalf("the lease ref survived reclaim: %v", err)
	}

	// Inside its timeout it is not collectable, spend or no spend.
	if _, err := PublishLease(v, clean, ""); err != nil {
		t.Fatal(err)
	}
	if err := Reclaim(v, clean, "", func() int64 { return now.Unix() - 1 }); err == nil {
		t.Fatal("a lease inside its reclaim timeout was reclaimed")
	}
}
