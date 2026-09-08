package effect

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Every other test in this package runs collapsed: one repository serving both
// roles. That is the honest shape for a unit test and it is also blind to the
// only mistake the split exists to prevent — a lease written into a project
// repo, where every project would grow its own plausible copy of what a cell may
// spend and the sum would exceed the envelope with nothing able to notice.
//
// So these tests run against two *different* repositories, and assert which one
// each ref landed in. A handle wired to the wrong replica passes every collapsed
// test in the suite and fails here.

// split builds a genuinely two-repository cell: separate coordination and
// project replicas, with the envelope and lease published to the coordination
// one.
func split(t *testing.T, amount float64, quantity int64) (*varvigcli.Fake, *varvigcli.Fake, varvigcli.FactoryRepo, varvigcli.ProjectRepo, authority.Grant) {
	t.Helper()
	coord, work := varvigcli.NewFake("coordination"), varvigcli.NewFake("project")
	fr := varvigcli.FactoryRepo{Varvig: coord}
	pr := varvigcli.ProjectRepo{Varvig: work}

	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: at.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 50000, Unit: "EUR", Quantity: 1000}},
	}
	if _, err := authority.PublishEnvelope(fr, env, ""); err != nil {
		t.Fatal(err)
	}
	l := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: amount, Unit: "EUR", Quantity: quantity, IssuedAt: at.Unix(),
	}
	hash, err := authority.PublishLease(fr, l, "")
	if err != nil {
		t.Fatal(err)
	}
	return coord, work, fr, pr, authority.Grant{Envelope: env, Lease: &l, LeaseHash: hash}
}

// only asserts that a ref prefix appears in one replica and not the other.
func only(t *testing.T, prefix string, present, absent *varvigcli.Fake) {
	t.Helper()
	found := func(f *varvigcli.Fake) []string {
		var out []string
		for name := range f.RefSnapshot() {
			if strings.HasPrefix(name, prefix) {
				out = append(out, name)
			}
		}
		return out
	}
	if got := found(present); len(got) == 0 {
		t.Errorf("%s is absent from the %s replica, where it belongs", prefix, present.Label)
	}
	if got := found(absent); len(got) > 0 {
		t.Errorf("%s leaked into the %s replica as %v", prefix, absent.Label, got)
	}
}

// TestEachRefLandsInItsOwnRepository pins the layout: authority in the
// coordination replica, work in the project replica, and neither in the other.
//
// Being precise about what this protects — the signatures make the direct
// mistake a compile error, so this cannot catch a lease passed to the project
// handle today. What it catches is the signature being loosened back to a bare
// Varvig, which compiles fine and puts the whole thing back within one
// substitution of where it started.
func TestEachRefLandsInItsOwnRepository(t *testing.T) {
	coord, work, fr, pr, g := split(t, 1000, 20)

	if _, err := Reserve(fr, pr, order(t, fabrication(t)), "mini-a", g, at.Unix(), 3600); err != nil {
		t.Fatal(err)
	}

	only(t, cell.LeasePrefix, coord, work)
	only(t, cell.EnvelopePrefix, coord, work)
	only(t, cell.ReservationPrefix, work, coord)
}

// TestSettleRecordsSpendBeforeTheReservation holds the write order by taking the
// coordination replica away.
//
// If the reservation were written first, this failure would leave a record
// saying an irreversible action was paid for against a lease with no record of
// paying — the one state nobody can undo. The lease going first means a failure
// here has changed nothing at all.
func TestSettleRecordsSpendBeforeTheReservation(t *testing.T) {
	_, work, fr, pr, g := split(t, 1000, 20)
	claimed, err := Reserve(fr, pr, order(t, fabrication(t)), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = TakeSelf(pr, claimed, at.Unix())
	if err != nil {
		t.Fatal(err)
	}

	// The coordination peer's disk fills, its process dies, the network eats the
	// write — the cause does not matter, only that the lease cannot be written.
	fr.Varvig.(*varvigcli.Fake).RefuseWrites = errors.New("coordination replica is unwritable")

	if _, err := Settle(fr, pr, claimed, "PO-1", 320, at.Unix()); err == nil {
		t.Fatal("settling with an unwritable coordination replica must fail, not proceed to mark the reservation done")
	}

	// The reservation must still read as open work, not as settled spend.
	after, _, err := loadReservation(pr, mustReservationRef(t, "mini-a", claimed.Reservation.Key))
	if err != nil {
		t.Fatal(err)
	}
	if after.State == StateDone {
		t.Fatal("the reservation was marked done although the lease never recorded the spend")
	}
	if after.HoldReleased {
		t.Fatal("the hold was released although the lease never recorded the spend")
	}
	_ = work
}

// TestReleaseIsAlsoLeaseFirst covers the other direction of the same rule. Fail
// gives headroom back, and it too writes the lease first — so an unwritable
// coordination replica leaves the reservation open rather than marking it failed
// against a hold that was never returned.
func TestReleaseIsAlsoLeaseFirst(t *testing.T) {
	_, _, fr, pr, g := split(t, 1000, 20)
	claimed, err := Reserve(fr, pr, order(t, fabrication(t)), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = TakeSelf(pr, claimed, at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	fr.Varvig.(*varvigcli.Fake).RefuseWrites = errors.New("coordination replica is unwritable")

	if _, err := Fail(fr, pr, claimed, "the vendor rejected the order", at.Unix()); err == nil {
		t.Fatal("failing with an unwritable coordination replica must not silently mark the reservation failed")
	}
	after, _, err := loadReservation(pr, mustReservationRef(t, "mini-a", claimed.Reservation.Key))
	if err != nil {
		t.Fatal(err)
	}
	if after.State == StateFailed {
		t.Fatal("the reservation was marked failed although the hold was never returned")
	}
}

// TestAnUnwritableProjectReplicaOverReportsSpend pins the *accepted* cost of the
// order, so nobody removes it as a bug.
//
// The lease has recorded the spend and the reservation has not caught up. That
// over-reports what this cell spent, which an overseer reading the lease can
// correct. It is the deliberate trade: this direction is recoverable, and the
// reverse — a settled record against an unpaid lease — is not.
func TestAnUnwritableProjectReplicaOverReportsSpend(t *testing.T) {
	_, work, fr, pr, g := split(t, 1000, 20)
	claimed, err := Reserve(fr, pr, order(t, fabrication(t)), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = TakeSelf(pr, claimed, at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	work.RefuseWrites = errors.New("project replica is unwritable")

	if _, err := Settle(fr, pr, claimed, "PO-1", 320, at.Unix()); err == nil {
		t.Fatal("a failed reservation write must be reported, not swallowed")
	}
	lease, _, err := authority.LoadLease(fr, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spent != 320 {
		t.Fatalf("lease spent = %g, want 320 — the spend must be durable even when the reservation write fails", lease.Spent)
	}
	if lease.Reserved != 0 {
		t.Fatalf("lease reserved = %g, want 0 — the hold was converted", lease.Reserved)
	}
}

// TestAReservationCannotBeSettledAgainstAnotherRepositorysLease is the type
// discipline stated as behaviour: a cell holding a lease the coordination
// replica does not have cannot settle against it, because the compare-and-swap
// is against a ref that is not there.
//
// Without the split this test could not exist — the lease would always be in the
// same repository as the reservation, so "which repository holds the authority"
// would never be a question with an answer.
func TestAReservationCannotBeSettledAgainstAnotherRepositorysLease(t *testing.T) {
	_, _, fr, pr, g := split(t, 1000, 20)
	claimed, err := Reserve(fr, pr, order(t, fabrication(t)), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = TakeSelf(pr, claimed, at.Unix())
	if err != nil {
		t.Fatal(err)
	}

	// A second factory's coordination replica, which has never issued this lease.
	stranger := varvigcli.FactoryRepo{Varvig: varvigcli.NewFake("someone-elses-factory")}
	if _, err := Settle(stranger, pr, claimed, "PO-1", 320, at.Unix()); err == nil {
		t.Fatal("spend was settled against a coordination replica that never issued the lease")
	}
}

func mustReservationRef(t *testing.T, cellID, key string) string {
	t.Helper()
	name, err := cell.ReservationRef(cellID, key)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// TestIntegrationTwoRealRepositoriesKeepTheirOwnRefs is the split against real
// varvig: two repositories, two `varvig init`s, and the refs where they belong.
//
// The Fake cannot fully stand in for this. Its ref store is a map, so "the lease
// is in the other repository" is true by construction there; here the two
// repositories are two directories with two independent ref stores and two
// independent compare-and-swaps, which is what a factory actually runs.
func TestIntegrationTwoRealRepositoriesKeepTheirOwnRefs(t *testing.T) {
	bin, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()
	mk := func(name string) string {
		init := exec.Command(bin, "init", name)
		init.Dir = dir
		init.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
		if out, err := init.CombinedOutput(); err != nil {
			t.Fatalf("varvig init %s: %v: %s", name, err, out)
		}
		return filepath.Join(dir, name)
	}
	fr := varvigcli.FactoryRepo{Varvig: varvigcli.Exec{Bin: bin, Dir: mk("coordination")}}
	pr := varvigcli.ProjectRepo{Varvig: varvigcli.Exec{Bin: bin, Dir: mk("project")}}

	c := fabrication(t)
	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: at.Unix(),
		Ceilings: []authority.Ceiling{{Capability: c.ID, Spend: 5000, Unit: "EUR", Quantity: 100}},
	}
	if _, err := authority.PublishEnvelope(fr, env, ""); err != nil {
		t.Fatalf("the coordination repo refused an envelope: %v", err)
	}
	l := authority.Lease{
		CellID: "mini-a", Capability: c.ID, Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 1000, Unit: "EUR", Quantity: 20, IssuedAt: at.Unix(),
	}
	leaseHash, err := authority.PublishLease(fr, l, "")
	if err != nil {
		t.Fatalf("the coordination repo refused a lease: %v", err)
	}
	g := authority.Grant{Envelope: env, Lease: &l, LeaseHash: leaseHash}

	claimed, err := Reserve(fr, pr, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatalf("Reserve across two real repositories: %v", err)
	}
	claimed, err = TakeSelf(pr, claimed, at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(fr, pr, claimed, "PO-1", 320, at.Unix()+60); err != nil {
		t.Fatalf("Settle across two real repositories: %v", err)
	}

	// The lease is in the coordination repo, carrying the spend, and the
	// project repo has never heard of it.
	spent, _, err := authority.LoadLease(fr, "mini-a", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spent.Spent != 320 || spent.Reserved != 0 {
		t.Fatalf("lease in the coordination repo: spent=%g reserved=%g, want 320 and 0", spent.Spent, spent.Reserved)
	}
	leaseRef, err := cell.LeaseRef("mini-a", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pr.ResolveRef(leaseRef); err == nil {
		t.Errorf("%s exists in the project repository; authority must live in exactly one place", leaseRef)
	}

	// And the reservation is in the project repo, where the work is, and the
	// coordination repo has never heard of *it*.
	resRef := mustReservationRef(t, "mini-a", claimed.Reservation.Key)
	if _, err := pr.ResolveRef(resRef); err != nil {
		t.Errorf("%s is missing from the project repository: %v", resRef, err)
	}
	if _, err := fr.ResolveRef(resRef); err == nil {
		t.Errorf("%s exists in the coordination repository; a reservation belongs to the codebase it acted on", resRef)
	}
}
