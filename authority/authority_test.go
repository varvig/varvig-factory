package authority

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func envelope() Envelope {
	return Envelope{
		Overseer: "overseer-a",
		Ceilings: []Ceiling{
			{Capability: "pcb-fabrication@1", Spend: 5000, Unit: "EUR", Quantity: 100, RatePerDay: 4},
			{Capability: "human-contract@1", Spend: 2000, Unit: "EUR"},
		},
		SetAt: now.Unix(),
	}
}

func lease(cellID, capability string, amount float64) Lease {
	return Lease{
		CellID: cellID, Capability: capability, Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: amount, Unit: "EUR", IssuedAt: now.Unix(),
	}
}

// Test13b_LeaseExclusivity is FACTORY.md §9.13b: leases issued to different
// cells never overlap, and the sum of outstanding leases never exceeds the
// envelope.
func Test13b_LeaseExclusivity(t *testing.T) {
	env := envelope()

	// Three cells splitting one ceiling is fine — that is what allocation is.
	ok := []Lease{
		lease("mini-a", "pcb-fabrication@1", 2000),
		lease("mini-b", "pcb-fabrication@1", 2000),
		lease("micro-c", "pcb-fabrication@1", 1000),
	}
	if err := CheckExclusive(env, ok); err != nil {
		t.Fatalf("leases summing exactly to the ceiling were refused: %v", err)
	}

	// One unit over is refused: the owner's exposure would exceed what they set.
	over := append(append([]Lease(nil), ok...), lease("micro-d", "pcb-fabrication@1", 1))
	err := CheckExclusive(env, over)
	if err == nil {
		t.Fatal("leases exceeding the envelope were accepted")
	}
	if !strings.Contains(err.Error(), "5001") && !strings.Contains(err.Error(), "exposure") {
		t.Fatalf("the refusal does not explain the overage: %v", err)
	}

	// Two leases for one cell and capability is an ambiguity, not an increase:
	// a lease is exclusive, so which of the two is the exclusive one?
	doubled := []Lease{
		lease("mini-a", "pcb-fabrication@1", 100),
		lease("mini-a", "pcb-fabrication@1", 100),
	}
	if err := CheckExclusive(env, doubled); err == nil {
		t.Fatal("one cell holding two leases for one capability was accepted")
	}

	// A lease for a capability the envelope does not bound has no ceiling to be
	// under. Silence is not permission.
	unbounded := []Lease{lease("mini-a", "shipping@1", 10)}
	if err := CheckExclusive(env, unbounded); err == nil {
		t.Fatal("a lease for an unbounded capability was accepted")
	}

	// A lease from another overseer is not drawn from this envelope, so summing
	// it against this ceiling would be meaningless.
	foreign := lease("mini-a", "pcb-fabrication@1", 100)
	foreign.Overseer = "overseer-b"
	if err := CheckExclusive(env, []Lease{foreign}); err == nil {
		t.Fatal("a lease from a different overseer was summed against this envelope")
	}

	// Unlike units are arithmetic errors waiting to happen.
	wrongUnit := lease("mini-a", "pcb-fabrication@1", 100)
	wrongUnit.Unit = "USD"
	if err := CheckExclusive(env, []Lease{wrongUnit}); err == nil {
		t.Fatal("a lease in a different unit from its ceiling was accepted")
	}
}

func TestExposureIsTheNumberToReasonAbout(t *testing.T) {
	// §6.6: the owner's maximum exposure is the sum of outstanding leases, not
	// the envelope. Spent amounts are no longer exposure — they are already
	// gone.
	spent := lease("mini-a", "pcb-fabrication@1", 2000)
	spent.Spent = 1500
	leases := []Lease{spent, lease("mini-b", "pcb-fabrication@1", 1000)}

	got := Exposure(leases)
	if want := 500.0 + 1000.0; got["pcb-fabrication@1"] != want {
		t.Fatalf("exposure = %g, want %g (headroom, not allocation)", got["pcb-fabrication@1"], want)
	}
}

func TestEmptyEnvelopeIsMalformedNotUnlimited(t *testing.T) {
	// The single worst default available here would be reading an envelope with
	// no ceilings as an unbounded one.
	if err := (Envelope{Overseer: "overseer-a"}).Validate(); err == nil {
		t.Fatal("an envelope with no ceilings validated")
	}
	// And an unlisted capability is refused rather than treated as a limit of
	// zero or as unbounded.
	if _, ok := envelope().Ceiling("shipping@1"); ok {
		t.Fatal("an unlisted capability reported a ceiling")
	}
}

func TestLeaseHeadroomAndExhaustion(t *testing.T) {
	l := lease("mini-a", "pcb-fabrication@1", 100)
	l.Spent = 40
	if l.Headroom() != 60 {
		t.Fatalf("headroom = %g, want 60", l.Headroom())
	}
	if l.Exhausted() {
		t.Fatal("a lease with headroom reported exhausted")
	}
	l.Spent = 100
	if !l.Exhausted() {
		t.Fatal("a fully spent lease did not report exhausted")
	}

	// A quantity ceiling exhausts independently of spend: ordering the last
	// board cheaply must not unlock a further order.
	q := lease("mini-a", "pcb-fabrication@1", 1000)
	q.Quantity, q.Ordered = 10, 10
	if !q.Exhausted() {
		t.Fatal("a lease with no units left reported spendable")
	}
	// "No quantity limit" and "no units left" must not share a value.
	noLimit := lease("mini-a", "pcb-fabrication@1", 100)
	if noLimit.QuantityHeadroom() != -1 {
		t.Fatalf("a lease with no quantity ceiling reported %d units left", noLimit.QuantityHeadroom())
	}
	if noLimit.Exhausted() {
		t.Fatal("a lease with no quantity ceiling reported exhausted")
	}
}

func TestLeaseValidateRejectsOverspend(t *testing.T) {
	// Overspend means either a settlement bug or an action taken outside the
	// lease. Neither is a state to tolerate quietly.
	l := lease("mini-a", "pcb-fabrication@1", 100)
	l.Spent = 101
	if err := l.Validate(); err == nil {
		t.Fatal("a lease that has overspent validated")
	}
}

// Test13c_StrandedLease is FACTORY.md §9.13c: reclaim is provisional; a cell
// returning with an unreported order does not produce a double-spend.
func Test13c_StrandedLease(t *testing.T) {
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	l.ReclaimAfter = now.Add(24 * time.Hour).Unix()

	if l.Reclaimable(now) {
		t.Fatal("a lease inside its timeout was reclaimable")
	}
	if !l.Reclaimable(now.Add(25 * time.Hour)) {
		t.Fatal("an unspent lease past its timeout was not reclaimable")
	}

	// The protection that matters: a cell that has spent anything is NOT
	// reclaimable, because it may have placed an order it has not yet
	// reported. Reclaiming there is how a double-spend happens.
	partly := l
	partly.Spent = 1
	if partly.Reclaimable(now.Add(25 * time.Hour)) {
		t.Fatal("a lease with recorded spend was reclaimable; that risks a double-spend")
	}

	// And the timeout does not make the lease unspendable by its holder — it is
	// a signal to the overseer, not an expiry.
	if err := Permit(ActEffectful, Sync{}, now.Add(25*time.Hour), 0, &l); err != nil {
		t.Fatalf("a lease past its reclaim timeout stopped being spendable: %v", err)
	}
}

// Test13_StaleStateBehaviour is FACTORY.md §9.13: an offline cell may propose
// and may execute effectfully within an outstanding lease, must refuse to
// promote, and must refuse effectful action beyond the lease.
func Test13_StaleStateBehaviour(t *testing.T) {
	offline := Sync{Configured: true, Reachable: false, At: now.Add(-72 * time.Hour)}
	held := lease("mini-a", "pcb-fabrication@1", 1000)

	// Propose: allowed. Append-only bounds the damage, so a revoked principal
	// that has not heard yet wastes compute and nothing more.
	if err := Permit(ActPropose, offline, now, 0, nil); err != nil {
		t.Fatalf("a disconnected cell was refused permission to propose: %v", err)
	}

	// Promote: refused. It moves a ref, and a shared ceiling cannot be enforced
	// from a stale view.
	err := Permit(ActPromote, offline, now, 0, nil)
	if err == nil {
		t.Fatal("a disconnected cell was permitted to promote")
	}
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Act != ActPromote {
		t.Fatalf("err = %v, want a promote Refusal", err)
	}
	if !strings.Contains(refusal.Reason, "unreachable") {
		t.Fatalf("the refusal does not name the cause: %s", refusal.Reason)
	}

	// Effectful within the lease: allowed, offline, indefinitely. This is the
	// autonomy that matters, and it needs no connectivity in the path.
	if err := PermitSpend(offline, now, 0, &held, 400, 0); err != nil {
		t.Fatalf("a disconnected cell was refused spend inside its own lease: %v", err)
	}
	if err := PermitSpend(offline, now.Add(365*24*time.Hour), 0, &held, 400, 0); err != nil {
		t.Fatalf("a lease stopped being spendable with age: %v", err)
	}

	// Effectful beyond the lease: refused, and it escalates rather than falling
	// back to the envelope. Falling back would make the lease advisory, and an
	// advisory exclusive allocation is a shared one.
	err = PermitSpend(offline, now, 0, &held, 1500, 0)
	if err == nil {
		t.Fatal("spend beyond the lease was permitted")
	}
	if !errors.As(err, &refusal) || !refusal.Escalate {
		t.Fatalf("spend beyond the lease did not escalate: %v", err)
	}

	// No lease at all: refused. An effectful action spends from an exclusive
	// allocation, never from a shared envelope.
	if err := Permit(ActEffectful, Sync{Configured: true, Reachable: true, At: now}, now, 0, nil); err == nil {
		t.Fatal("an effectful action with no lease was permitted, even while online")
	}
}

func TestPromoteIsPermittedWhenFresh(t *testing.T) {
	fresh := Sync{Configured: true, Reachable: true, At: now}
	if err := Permit(ActPromote, fresh, now, 0, nil); err != nil {
		t.Fatalf("a synced cell was refused permission to promote: %v", err)
	}
	// A single-cell deployment with no upstream is not stale: there is no peer
	// whose state it could be behind, and refusing here would make promotion
	// impossible for the simplest working configuration.
	if err := Permit(ActPromote, Sync{Configured: false}, now, 0, nil); err != nil {
		t.Fatalf("a cell with no upstream was refused permission to promote: %v", err)
	}
	// Never synced is not fresh, even if the last attempt did not fail.
	if err := Permit(ActPromote, Sync{Configured: true, Reachable: true}, now, 0, nil); err == nil {
		t.Fatal("a cell that has never synced was permitted to promote")
	}
}

func TestMaxAgeIsOptionalAndOffByDefault(t *testing.T) {
	// The default is exactly what §4.3b asks for and nothing more: a successful
	// sync establishes current state. Inventing an age threshold would be
	// inventing policy.
	old := Sync{Configured: true, Reachable: true, At: now.Add(-30 * 24 * time.Hour)}
	if err := Permit(ActPromote, old, now, 0, nil); err != nil {
		t.Fatalf("with no age bound configured, a stale-but-reachable sync was refused: %v", err)
	}
	// A stricter operator sets one, and then it bites — the case it exists for
	// is a sync loop that has stalled without failing.
	err := Permit(ActPromote, old, now, MaxAge(time.Hour), nil)
	if err == nil {
		t.Fatal("a configured max age did not apply")
	}
	if !strings.Contains(err.Error(), "beyond the configured maximum") {
		t.Fatalf("the refusal does not name the bound: %v", err)
	}
}

func TestActNames(t *testing.T) {
	// These appear in refusals an operator reads.
	if ActPropose.String() != "propose" || ActPromote.String() != "promote" || ActEffectful.String() != "effectful action" {
		t.Fatal("act names are not stable")
	}
}

func TestHoldsAreCountedAgainstHeadroom(t *testing.T) {
	// §7.1: a reservation holds headroom before the action is taken. Without
	// that, two pending actions each check the same headroom and pass.
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	l.Quantity = 20

	held, err := l.Hold(700, 5)
	if err != nil {
		t.Fatalf("holding 700 of 1000: %v", err)
	}
	if held.Headroom() != 300 || held.QuantityHeadroom() != 15 {
		t.Fatalf("after a hold: headroom=%g units=%d, want 300 and 15", held.Headroom(), held.QuantityHeadroom())
	}
	// Held is not spent. The distinction matters because a hold can come back
	// and a spend cannot.
	if held.Spent != 0 {
		t.Fatalf("a hold was recorded as spend: %g", held.Spent)
	}
	if _, err := held.Hold(400, 0); err == nil {
		t.Fatal("a second hold exceeded the lease")
	}
	if _, err := held.Hold(0, 16); err == nil {
		t.Fatal("a second hold exceeded the unit ceiling")
	}
	if _, err := held.Hold(-1, 0); err == nil {
		t.Fatal("a negative hold was accepted")
	}

	// A lease over-committed by holds is invalid, not merely tight: it means a
	// hold was taken without a check.
	bogus := l
	bogus.Spent, bogus.Reserved = 600, 600
	err = bogus.Validate()
	if err == nil {
		t.Fatal("a lease committing 1200 of 1000 validated")
	}
	if !strings.Contains(err.Error(), "held by reservations") {
		t.Fatalf("the refusal does not distinguish held from spent: %v", err)
	}
}

func TestReleaseAndConvert(t *testing.T) {
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	l.Quantity = 20
	held, err := l.Hold(320, 5)
	if err != nil {
		t.Fatal(err)
	}

	// Release: a confirmed rejection, or an expiry. The money comes back and
	// nothing was spent.
	back, err := held.Release(320, 5)
	if err != nil {
		t.Fatal(err)
	}
	if back.Headroom() != 1000 || back.Spent != 0 {
		t.Fatalf("release left headroom=%g spent=%g", back.Headroom(), back.Spent)
	}
	// Releasing twice is refused rather than clamped at zero: clamping would
	// hand back headroom that a still-pending action might yet consume.
	if _, err := back.Release(320, 0); err == nil {
		t.Fatal("a double release was accepted")
	}

	// Convert: settlement. The hold becomes spend, at the *actual* price.
	settled, err := held.Convert(320, 355.40, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Spent != 355.40 || settled.Reserved != 0 || settled.Ordered != 5 {
		t.Fatalf("settlement left %+v", settled)
	}
	if settled.Headroom() != 1000-355.40 {
		t.Fatalf("headroom = %g", settled.Headroom())
	}
	// An actual beyond the whole lease cannot be recorded: the lease would be in
	// a state it calls invalid.
	small := lease("mini-a", "pcb-fabrication@1", 400)
	smallHeld, err := small.Hold(320, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := smallHeld.Convert(320, 900, 0, 0); err == nil {
		t.Fatal("an actual beyond the lease was recorded")
	}
}

func TestAHeldLeaseIsNotReclaimable(t *testing.T) {
	// A hold means an action may be in flight right now, which is a sharper
	// reason not to reclaim than settled spend is.
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	l.ReclaimAfter = now.Unix()
	held, err := l.Hold(320, 0)
	if err != nil {
		t.Fatal(err)
	}
	if held.Reclaimable(now.Add(48 * time.Hour)) {
		t.Fatal("a lease with an outstanding hold was reclaimable")
	}
	if !l.Reclaimable(now.Add(48 * time.Hour)) {
		t.Fatal("an untouched, timed-out lease was not reclaimable")
	}
}

func TestExposureExcludesHeldHeadroom(t *testing.T) {
	// Held amounts are already committed — they may be real orders at the far
	// end. Reporting them as still-allocatable exposure would understate the
	// commitment and overstate what is left.
	l := lease("mini-a", "pcb-fabrication@1", 1000)
	held, err := l.Hold(400, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := Exposure([]Lease{held})["pcb-fabrication@1"]; got != 600 {
		t.Fatalf("exposure = %g, want 600", got)
	}
}
