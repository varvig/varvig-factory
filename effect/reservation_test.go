package effect

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/varvigcli"
)

// repos is the collapsed pair these tests run against: one Fake serving both
// the coordination and the project role.
//
// Collapsed is the honest shape for a unit test — there is one repository — and
// keeping the two handles distinct rather than passing one client twice means
// each call still states which repository it is talking to, so a call wired to
// the wrong one fails here and not in a factory with two.
type repos struct {
	F varvigcli.FactoryRepo
	P varvigcli.ProjectRepo
}

// leased sets up a repository with an envelope and one lease for mini-a, and
// returns the lease with the hash it lives at.
func leased(t *testing.T, amount float64, quantity int64) (repos, authority.Grant) {
	t.Helper()
	fr, pr := varvigcli.Collapsed(varvigcli.NewFake("test"))
	v := repos{F: fr, P: pr}
	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: at.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 50000, Unit: "EUR", Quantity: 1000}},
	}
	if _, err := authority.PublishEnvelope(v.F, env, ""); err != nil {
		t.Fatal(err)
	}
	l := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: amount, Unit: "EUR", Quantity: quantity, IssuedAt: at.Unix(),
	}
	hash, err := authority.PublishLease(v.F, l, "")
	if err != nil {
		t.Fatal(err)
	}
	return v, authority.Grant{Envelope: env, Lease: &l, LeaseHash: hash}
}

// after is the grant as it stands following a claim, for the next call.
func after(g authority.Grant, c Claim) authority.Grant {
	return authority.Grant{Envelope: g.Envelope, Lease: &c.Lease, LeaseHash: c.LeaseHash}
}

// acting reserves and then takes, which is what the in-process path does: a
// fresh reservation is only *offered*, and taking it is what says something is
// now acting on it and the outcome is no longer known to be nothing.
func acting(t *testing.T, v repos, req Request, cellID string, g authority.Grant, at, ttl int64) (Claim, error) {
	t.Helper()
	c, err := Reserve(v.F, v.P, req, cellID, g, at, ttl)
	if err != nil {
		return c, err
	}
	return TakeSelf(v.P, c, at)
}

// Test11b_ReservationExecutesOnce is the durable half of FACTORY.md §9.11.
// Deriving a stable key says what "the same action" means; claiming it in a ref
// before executing is what actually stops the second order.
func Test11b_ReservationExecutesOnce(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	req := order(t, c)

	claim, err := acting(t, v, req, "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatalf("first reservation refused: %v", err)
	}
	if claim.Reservation.State != StatePending {
		t.Fatalf("a taken reservation is %q, want pending: something is acting and the outcome is unknown", claim.Reservation.State)
	}

	// The cell places the order, then records the far end's identifier for it.
	claim, err = Settle(v.F, v.P, claim, "PO-90210", 0, at.Unix()+5)
	if err != nil {
		t.Fatalf("settling: %v", err)
	}
	if claim.Lease.Spent != 320 || claim.Lease.Reserved != 0 {
		t.Fatalf("settlement left the lease at spent=%g reserved=%g, want 320 and 0",
			claim.Lease.Spent, claim.Lease.Reserved)
	}

	// Now the same intent arrives again — a restarted process, a re-run task, a
	// retry after a lost response. It must not execute.
	retry := order(t, c)
	retry.Payload = map[string]any{"quantity": 5, "gerber": "1e20deadbeef"} // other key order
	again, err := Reserve(v.F, v.P, retry, "mini-a", after(g, claim), at.Unix()+60, 3600)
	if !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a repeat of a completed action returned %v, want ErrAlreadyReserved", err)
	}
	// And it is told what happened, not merely that it may not proceed: the
	// answer to "did my order go through" is in the record.
	if again.Reservation.State != StateDone || again.Reservation.ExternalRef != "PO-90210" {
		t.Fatalf("the existing reservation did not report the outcome: %+v", again.Reservation)
	}
	// The refused repeat must not have taken a second hold.
	held, _, err := authority.LoadLease(v.F, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if held.Reserved != 0 {
		t.Fatalf("a refused repeat held %g of lease headroom", held.Reserved)
	}

	// A genuinely different action is not blocked by it.
	other := order(t, c)
	other.Payload = map[string]any{"gerber": "1e20deadbeef", "quantity": 6}
	if _, err := Reserve(v.F, v.P, other, "mini-a", after(g, claim), at.Unix()+60, 3600); err != nil {
		t.Fatalf("a different order was blocked by an unrelated reservation: %v", err)
	}
}

// Test11c_HoldsPreventTwoPendingOrdersExceedingTheLease is the property a
// derived key alone does not give: without a hold, two pending actions each
// check the same headroom, each pass, and together exceed the lease.
func Test11c_HoldsPreventTwoPendingOrdersExceedingTheLease(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 1000, 20)

	first := order(t, c)
	first.Amount, first.Quantity = 700, 5
	claim, err := acting(t, v, first, "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatalf("the first order was refused: %v", err)
	}
	if claim.Lease.Reserved != 700 || claim.Lease.Headroom() != 300 {
		t.Fatalf("after holding 700 of 1000 the lease reports reserved=%g headroom=%g",
			claim.Lease.Reserved, claim.Lease.Headroom())
	}

	// A second order that fits the *allocation* but not the remaining headroom.
	// Nothing has settled yet, so a lease that only counted settled spend would
	// wave this through and the two together would be 1400 of 1000.
	second := order(t, c)
	second.Task, second.Amount = "T-1043", 700
	if _, err := Reserve(v.F, v.P, second, "mini-a", after(g, claim), at.Unix()+1, 3600); err == nil {
		t.Fatal("two pending orders were allowed to exceed the lease together")
	}

	// One that fits the headroom is fine.
	third := order(t, c)
	third.Task, third.Amount, third.Quantity = "T-1044", 300, 2
	if _, err := Reserve(v.F, v.P, third, "mini-a", after(g, claim), at.Unix()+1, 3600); err != nil {
		t.Fatalf("an order inside the remaining headroom was refused: %v", err)
	}

	// The same rule applies to unit counts independently of money: ordering the
	// last boards cheaply must not unlock a further order.
	v2, g2 := leased(t, 100000, 10)
	bulk := order(t, c)
	bulk.Amount, bulk.Quantity = 10, 10
	held, err := acting(t, v2, bulk, "mini-a", g2, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	more := order(t, c)
	more.Task, more.Amount, more.Quantity = "T-1043", 10, 1
	if _, err := Reserve(v2.F, v2.P, more, "mini-a", after(g2, held), at.Unix()+1, 3600); err == nil {
		t.Fatal("a pending order holding every unit still left units to order")
	}
}

// Test14_ReservationExpiry is FACTORY.md §9.14: an unsettled reservation
// releases headroom rather than permanently consuming it — and, crucially, does
// not release the key.
func Test14_ReservationExpiry(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	req := order(t, c)

	claim, err := acting(t, v, req, "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Lease.Reserved != 320 {
		t.Fatalf("reserved = %g, want the quoted 320 held", claim.Lease.Reserved)
	}

	// Nothing came back. Before the timeout the headroom stays held: the action
	// may be in flight.
	lease, leaseHash, released, err := ReleaseExpired(v.F, v.P, claim.Lease, claim.LeaseHash, at.Unix()+60)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 0 || lease.Reserved != 320 {
		t.Fatalf("a hold inside its timeout was released: %+v", lease)
	}

	// Past the timeout the money comes back, so a lost response cannot consume
	// budget for good.
	lease, _, released, err = ReleaseExpired(v.F, v.P, lease, leaseHash, at.Unix()+7200)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 {
		t.Fatalf("released %d holds, want 1", len(released))
	}
	if lease.Reserved != 0 || lease.Headroom() != 1000 {
		t.Fatalf("expiry did not return the headroom: reserved=%g headroom=%g", lease.Reserved, lease.Headroom())
	}

	// But the key does NOT come back. The action may have happened, and letting
	// the key go is how the same order gets placed twice. Two resources, two
	// rules: money on a timer, the right to act not at all.
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", authority.Grant{Envelope: g.Envelope, Lease: &lease}, at.Unix()+7200, 3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("an expired reservation released its key: %v", err)
	}
	// And it is still an action of unknown outcome, so it still shows up as
	// pending for a principal to resolve.
	pending, err := Pending(v.P, "mini-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !pending[0].HoldReleased {
		t.Fatalf("pending = %+v, want the one expired-but-unresolved action", pending)
	}
}

func TestResolvingAnExpiredReservationThatDidHappenStillCharges(t *testing.T) {
	// The nasty case: the hold was returned on the timer, then the overseer finds
	// the order really was placed. The money must still leave the lease.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	lease, leaseHash, _, err := ReleaseExpired(v.F, v.P, claim.Lease, claim.LeaseHash, at.Unix()+7200)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(v.P, "mini-a")
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v (err %v)", pending, err)
	}

	// Rebuild the handles, as an overseer tool would after listing.
	claim2, err := LoadClaim(v.P, "mini-a", pending[0].Key, lease, leaseHash)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(v.F, v.P, claim2, true, "overseer-a", "PO-90210 exists at the fab", at.Unix()+9000)
	if err != nil {
		t.Fatalf("resolving an expired-but-real order: %v", err)
	}
	if resolved.Lease.Spent != 320 {
		t.Fatalf("spent = %g, want 320: the order happened and the lease owes it", resolved.Lease.Spent)
	}
	if resolved.Lease.Reserved != 0 {
		t.Fatalf("reserved = %g, want 0: the hold was already returned", resolved.Lease.Reserved)
	}
}

func TestTheSameCellCannotReserveOneKeyTwice(t *testing.T) {
	// Create-only is the whole locking mechanism. Two attempts produce one winner
	// and one refusal, using nothing but varvig's ref CAS.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	req := order(t, c)

	claim, err := acting(t, v, req, "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(v.F, v.P, req, "mini-a", after(g, claim), at.Unix(), 3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("the same cell reserved one key twice: %v", err)
	}
}

func TestReserveRefusesAnotherCellsLease(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	if _, err := Reserve(v.F, v.P, order(t, c), "micro-b", g, at.Unix(), 3600); err == nil {
		t.Fatal("a cell reserved against a lease held by another cell")
	}
	// And a lease for a different capability than the action.
	wrong := *g.Lease
	wrong.Capability = "shipping@1"
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", authority.Grant{Envelope: g.Envelope, Lease: &wrong, LeaseHash: g.LeaseHash}, at.Unix(), 3600); err == nil {
		t.Fatal("an action was reserved against a lease for a different capability")
	}
}

func TestPendingIsTheStateThatEscalates(t *testing.T) {
	// A crash between reserve and settle leaves pending, which is the honest
	// record of the one state that matters: the cell does not know whether the
	// order was placed.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Reservation.Unresolved() {
		t.Fatal("a reserved-but-unsettled action did not report unresolved")
	}

	pending, err := Pending(v.P, "mini-a")
	if err != nil {
		t.Fatalf("listing pending reservations: %v", err)
	}
	if len(pending) != 1 || pending[0].Key != claim.Reservation.Key {
		t.Fatalf("pending = %+v, want the one unresolved action", pending)
	}

	// The cell cannot clear it by retrying, and cannot clear it by deleting the
	// reservation either: the key stays claimed.
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", after(g, claim), at.Unix()+3600, 3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a pending action was retried: %v", err)
	}

	// A timeout is not a failure. Recording one as a failure would assert that
	// no effect occurred, which is a different claim from "we never heard back".
	if _, err := Fail(v.F, v.P, claim, "", at.Unix()+10); err == nil {
		t.Fatal("a failure was recorded with no rejection behind it")
	}

	// Nor can the cell resolve its own unknown state.
	if _, err := Resolve(v.F, v.P, claim, true, "mini-a", "looked it up", at.Unix()+10); !errors.Is(err, ErrSelfAuthorization) {
		t.Fatalf("a cell resolved its own pending reservation: %v", err)
	}
	if _, err := Resolve(v.F, v.P, claim, true, "", "looked it up", at.Unix()+10); err == nil {
		t.Fatal("a pending reservation was resolved by nobody in particular")
	}

	// A higher principal checks the external system and records what it found.
	// That the record says a principal decided it is the point: it is the
	// difference between a confirmed outcome and an assumed one.
	after, err := Resolve(v.F, v.P, claim, true, "overseer-a", "order PO-90210 exists at the fab", at.Unix()+600)
	if err != nil {
		t.Fatalf("an overseer could not resolve a pending reservation: %v", err)
	}
	if after.Reservation.State != StateDone || !strings.Contains(after.Reservation.Detail, "overseer-a") {
		t.Fatalf("the resolution did not record who decided it: %+v", after.Reservation)
	}
	if after.Lease.Spent != 320 || after.Lease.Reserved != 0 {
		t.Fatalf("resolving as happened left the lease at spent=%g reserved=%g", after.Lease.Spent, after.Lease.Reserved)
	}
	if left, err := Pending(v.P, "mini-a"); err != nil || len(left) != 0 {
		t.Fatalf("pending = %+v (err %v), want empty after resolution", left, err)
	}
}

func TestASettledSpendMustBeLookUpAble(t *testing.T) {
	// The next question about an unexpected invoice is "which order was it", and
	// the answer has to be in the record.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(v.F, v.P, claim, "", 0, at.Unix()+5); err == nil {
		t.Fatal("a spend was settled with no external reference to look up")
	}
	// Settling twice would spend the lease twice.
	settled, err := Settle(v.F, v.P, claim, "PO-1", 0, at.Unix()+5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(v.F, v.P, settled, "PO-1", 0, at.Unix()+6); err == nil {
		t.Fatal("a settled reservation was settled again")
	}
}

func TestSettlementRecordsActualAgainstQuoted(t *testing.T) {
	// A quote that is not exact is normal; a pattern of them is a capability
	// whose quotes cannot be trusted, which is worth surfacing rather than
	// absorbing.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := Settle(v.F, v.P, claim, "PO-90210", 355.40, at.Unix()+5)
	if err != nil {
		t.Fatalf("settling above the quote: %v", err)
	}
	if settled.Lease.Spent != 355.40 {
		t.Fatalf("spent = %g, want the actual 355.40 rather than the quoted 320", settled.Lease.Spent)
	}
	if settled.Reservation.Actual != 355.40 || !strings.Contains(settled.Reservation.Detail, "quoted 320") {
		t.Fatalf("the divergence was absorbed rather than recorded: %+v", settled.Reservation)
	}

	// An actual that would push the lease past its allocation is refused: the
	// lease cannot record a state it says is invalid.
	v2, g2 := leased(t, 400, 20)
	claim2, err := acting(t, v2, order(t, c), "mini-a", g2, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(v2.F, v2.P, claim2, "PO-2", 900, at.Unix()+5); err == nil {
		t.Fatal("an actual beyond the whole lease was recorded without complaint")
	}
}

func TestAFailedActionReleasesItsHoldButKeepsItsKey(t *testing.T) {
	// A definite rejection means no effect occurred, so the money comes back —
	// but the key stays claimed. Whether to authorize a fresh attempt is a
	// decision for a higher principal, not a loop behaviour (§6.7 rule 5).
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Fail(v.F, v.P, claim, "fab rejected the gerber: layer count", at.Unix()+5)
	if err != nil {
		t.Fatalf("recording a definite rejection: %v", err)
	}
	if failed.Lease.Reserved != 0 || failed.Lease.Spent != 0 || failed.Lease.Headroom() != 1000 {
		t.Fatalf("a rejection did not return the headroom: %+v", failed.Lease)
	}
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", after(g, failed), at.Unix()+60, 3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a failed action was silently retried: %v", err)
	}
	// It is not pending, though — a confirmed rejection is resolved, and listing
	// it as unknown-outcome would bury the reservations that really are unknown.
	if left, err := Pending(v.P, "mini-a"); err != nil || len(left) != 0 {
		t.Fatalf("pending = %+v (err %v), want empty: a confirmed rejection is resolved", left, err)
	}
}

func TestReservationRecordsTheInterfaceHash(t *testing.T) {
	// A reservation read back years later must still name an unambiguous
	// contract, even if the alias has since been re-pointed (§2.1).
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Reservation.Interface != c.Interface {
		t.Fatalf("interface = %q, want the hash %q", claim.Reservation.Interface, c.Interface)
	}
	if claim.Reservation.AuthorizedBy != "overseer-a" {
		t.Fatalf("authorized_by = %q; who authorized a spend is the first question asked about it", claim.Reservation.AuthorizedBy)
	}
}

func TestReserveRefusesAMalformedRequest(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	aliasOnly := c
	aliasOnly.Interface = ""
	if _, err := Reserve(v.F, v.P, order(t, aliasOnly), "mini-a", g, at.Unix(), 3600); err == nil {
		t.Fatal("an alias-only capability was reserved")
	}
	noTask := order(t, c)
	noTask.Task = ""
	if _, err := Reserve(v.F, v.P, noTask, "mini-a", g, at.Unix(), 3600); err == nil {
		t.Fatal("a request with no task id was reserved")
	}
}

// TestIntegrationReservationRefsAreAccepted drives the real varvig binary, and
// skips when one is not on PATH.
//
// `refs/reservations/` is a new namespace, and core reserves some prefixes for
// itself. If it rejected this one the whole idempotency mechanism would be
// undeployable — and no test against the Fake could tell us, because the Fake
// accepts any name.
func TestIntegrationReservationRefsAreAccepted(t *testing.T) {
	bin, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()
	init := exec.Command(bin, "init", "repo")
	init.Dir = dir
	init.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("varvig init: %v: %s", err, out)
	}
	// One real repository, both roles: the collapsed configuration.
	fr, pr := varvigcli.Collapsed(varvigcli.Exec{Bin: bin, Dir: filepath.Join(dir, "repo")})
	v := repos{F: fr, P: pr}

	l := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 1000, Unit: "EUR", Quantity: 20, IssuedAt: at.Unix(),
	}
	leaseHash, err := authority.PublishLease(v.F, l, "")
	if err != nil {
		t.Fatalf("real core refused a lease ref: %v", err)
	}
	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: at.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 50000, Unit: "EUR", Quantity: 1000}},
	}
	if _, err := authority.PublishEnvelope(v.F, env, ""); err != nil {
		t.Fatalf("real core refused an envelope ref: %v", err)
	}
	g := authority.Grant{Envelope: env, Lease: &l, LeaseHash: leaseHash}

	c := fabrication(t)
	req := order(t, c)
	claim, err := acting(t, v, req, "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatalf("real core refused a reservation ref: %v", err)
	}
	if claim.Lease.Reserved != 320 {
		t.Fatalf("the hold did not reach the lease ref: %+v", claim.Lease)
	}

	// Create-only against a real core, which is the property the whole mechanism
	// rests on: whoever creates the ref executes, and everyone else is refused.
	if _, err := Reserve(v.F, v.P, req, "mini-a", after(g, claim), at.Unix()+1, 3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a repeat reservation returned %v, want ErrAlreadyReserved", err)
	}
	if pending, err := Pending(v.P, "mini-a"); err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v (err %v), want the one unresolved action", pending, err)
	}
	settled, err := Settle(v.F, v.P, claim, "PO-90210", 0, at.Unix()+5)
	if err != nil {
		t.Fatalf("settling against a real core: %v", err)
	}
	if settled.Lease.Spent != 320 || settled.Lease.Reserved != 0 {
		t.Fatalf("settlement against a real core left %+v", settled.Lease)
	}
	done, err := Reserve(v.F, v.P, req, "mini-a", after(g, settled), at.Unix()+60, 3600)
	if !errors.Is(err, ErrAlreadyReserved) || done.Reservation.ExternalRef != "PO-90210" {
		t.Fatalf("a settled reservation did not report its outcome: %+v (err %v)", done.Reservation, err)
	}
	if pending, err := Pending(v.P, "mini-a"); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v (err %v), want empty after settlement", pending, err)
	}
}

// Test12_EnvelopeTightening is FACTORY.md §9.12: an overseer tightens an
// envelope mid-run and the cell honours the tighter ceiling before its next
// effectful action. Loosening does not apply until sync.
func Test12_EnvelopeTightening(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 1000, 20)

	// Baseline: the envelope is wide, so the lease is the binding constraint.
	if bounded, err := g.Bounded(); err != nil || bounded.Headroom() != 1000 {
		t.Fatalf("under a wide envelope, headroom = %v (err %v), want 1000", bounded, err)
	}

	// The overseer tightens to 200, below this cell's 1000 lease. The cell has
	// not been reissued anything and its lease still says 1000 — but the
	// envelope is what the overseer will stand behind now.
	tight := g.Envelope
	tight.Ceilings = []authority.Ceiling{{Capability: c.ID, Spend: 200, Unit: "EUR", Quantity: 3}}
	tightened := authority.Grant{Envelope: tight, Lease: g.Lease, LeaseHash: g.LeaseHash}

	bounded, err := tightened.Bounded()
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Headroom() != 200 || bounded.QuantityHeadroom() != 3 {
		t.Fatalf("after tightening: headroom=%g units=%d, want 200 and 3", bounded.Headroom(), bounded.QuantityHeadroom())
	}
	// The lease as issued is untouched. Rewriting it would destroy the record of
	// what the overseer actually committed to and when.
	if g.Lease.Amount != 1000 {
		t.Fatalf("the stored lease was rewritten to %g; the allocation record must survive a tightening", g.Lease.Amount)
	}

	// The 320 EUR order that was fine a moment ago is now refused — before it
	// happens, which is the only point at which refusing helps.
	d := Check(order(t, c), "mini-a", tightened, online, clock, 0)
	if d.Allowed {
		t.Fatal("an order above the tightened ceiling was allowed")
	}
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", tightened, at.Unix(), 3600); err == nil {
		t.Fatal("an order above the tightened ceiling was reserved")
	}
	// And nothing was held on the lease by that refused attempt.
	stored, _, err := authority.LoadLease(v.F, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reserved != 0 {
		t.Fatalf("a refused reservation held %g of headroom", stored.Reserved)
	}

	// An order inside the tightened ceiling still goes through: tightening is a
	// lower ceiling, not a freeze.
	small := order(t, c)
	small.Amount, small.Quantity = 150, 2
	claim, err := Reserve(v.F, v.P, small, "mini-a", tightened, at.Unix(), 3600)
	if err != nil {
		t.Fatalf("an order inside the tightened ceiling was refused: %v", err)
	}
	if claim.Lease.Amount != 1000 {
		t.Fatalf("the written lease was the bounded view (%g), not the allocation", claim.Lease.Amount)
	}

	// Tightening applies with NO fresh sync. §4.3b's whole point is that
	// effectful action inside a lease needs no connectivity, and a staleness
	// check here would take that back — so a tighter ceiling is honoured from a
	// three-day-old view, because adopting it can only reduce spend.
	offline := authority.Sync{Configured: true, Reachable: false, At: at.Add(-72 * time.Hour)}
	if d := Check(order(t, c), "mini-a", tightened, offline, clock, 0); d.Allowed {
		t.Fatal("a disconnected cell ignored a tightened envelope")
	}

	// Loosening does not apply. A wider envelope grants this cell nothing,
	// because the minimum is still the lease — more headroom needs a new lease,
	// which only the overseer can write and the cell cannot see without syncing.
	loose := g.Envelope
	loose.Ceilings = []authority.Ceiling{{Capability: c.ID, Spend: 999999, Unit: "EUR", Quantity: 9999}}
	loosened := authority.Grant{Envelope: loose, Lease: g.Lease, LeaseHash: g.LeaseHash}
	wide, err := loosened.Bounded()
	if err != nil {
		t.Fatal(err)
	}
	if wide.Amount != 1000 || wide.Quantity != 20 {
		t.Fatalf("a loosened envelope raised the lease to %g/%d; loosening must require a new lease", wide.Amount, wide.Quantity)
	}
}

func Test12b_TighteningBelowWhatIsAlreadySpent(t *testing.T) {
	// The awkward case: the overseer tightens under what the cell has already
	// spent. That money is gone and is not clawed back — the lease simply has
	// nothing further to give.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)
	claim, err := acting(t, v, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := Settle(v.F, v.P, claim, "PO-1", 0, at.Unix()+5)
	if err != nil {
		t.Fatal(err)
	}

	tight := g.Envelope
	tight.Ceilings = []authority.Ceiling{{Capability: c.ID, Spend: 100, Unit: "EUR"}}
	bounded, err := authority.Grant{Envelope: tight, Lease: &settled.Lease}.Bounded()
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Headroom() != 0 {
		t.Fatalf("headroom = %g, want 0: nothing further is spendable", bounded.Headroom())
	}
	if bounded.Spent != 320 {
		t.Fatalf("spent = %g, want 320: a tightening does not un-spend money", bounded.Spent)
	}
	if err := bounded.Validate(); err != nil {
		t.Fatalf("the bounded lease is not a valid state to reason about: %v", err)
	}
}

func Test12c_RemovingACapabilityIsTheSharpestTightening(t *testing.T) {
	// Dropping a capability from the envelope leaves the lease with no ceiling to
	// be under. Silence is not permission, so this refuses rather than reading
	// the absence as unbounded.
	c := fabrication(t)
	v, g := leased(t, 1000, 20)

	empty := g.Envelope
	empty.Ceilings = []authority.Ceiling{{Capability: "shipping@1", Spend: 100, Unit: "EUR"}}
	revoked := authority.Grant{Envelope: empty, Lease: g.Lease, LeaseHash: g.LeaseHash}

	if _, err := revoked.Bounded(); err == nil {
		t.Fatal("a lease for a capability the envelope no longer bounds was allowed")
	}
	d := Check(order(t, c), "mini-a", revoked, online, clock, 0)
	if d.Allowed {
		t.Fatal("an order under a revoked capability was allowed")
	}
	if !d.Escalate {
		t.Fatal("a revoked capability did not escalate; nothing the cell can do alone changes it")
	}
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", revoked, at.Unix(), 3600); err == nil {
		t.Fatal("an order under a revoked capability was reserved")
	}

	// A malformed envelope is refused for the same reason, rather than being
	// treated as absent — which would make it the widest envelope there is.
	broken := authority.Grant{Envelope: authority.Envelope{Overseer: "overseer-a"}, Lease: g.Lease}
	if _, err := broken.Bounded(); err == nil {
		t.Fatal("a malformed envelope bounded a lease")
	}

	// And an envelope from a different overseer bounds nothing here.
	foreign := g.Envelope
	foreign.Overseer = "overseer-b"
	if _, err := (authority.Grant{Envelope: foreign, Lease: g.Lease}).Bounded(); err == nil {
		t.Fatal("a lease was bounded by another overseer's envelope")
	}
}

// Test12d_TighteningIsNotTheEnforcementMechanism records what the cell-side cap
// does *not* buy, so nobody later mistakes it for a guarantee.
func Test12d_TighteningIsNotTheEnforcementMechanism(t *testing.T) {
	c := fabrication(t)
	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: at.Unix(),
		Ceilings: []authority.Ceiling{{Capability: c.ID, Spend: 3000, Unit: "EUR"}},
	}
	// Three cells each holding 2000 against a shared ceiling of 3000. Each one
	// capping itself at the ceiling still permits 6000 in total — which is
	// exactly why §6.6 says a shared ceiling cannot be enforced locally, and why
	// tightening means the overseer not replenishing.
	var total float64
	for _, id := range []string{"mini-a", "mini-b", "micro-c"} {
		l := authority.Lease{
			CellID: id, Capability: c.ID, Overseer: "overseer-a",
			Envelope: "1e20abc", Amount: 2000, Unit: "EUR", IssuedAt: at.Unix(),
		}
		bounded, err := (authority.Grant{Envelope: env, Lease: &l}).Bounded()
		if err != nil {
			t.Fatal(err)
		}
		total += bounded.Headroom()
	}
	if total <= env.Ceilings[0].Spend {
		t.Fatalf("this test is meant to demonstrate that local capping does not bound the sum; total=%g ceiling=%g",
			total, env.Ceilings[0].Spend)
	}
	// The real bound is the one the overseer chose when issuing: outstanding
	// leases. CheckExclusive is what refuses to issue them this way.
	leases := []authority.Lease{
		{CellID: "mini-a", Capability: c.ID, Overseer: "overseer-a", Envelope: "1e20abc", Amount: 2000, Unit: "EUR", IssuedAt: at.Unix()},
		{CellID: "mini-b", Capability: c.ID, Overseer: "overseer-a", Envelope: "1e20abc", Amount: 2000, Unit: "EUR", IssuedAt: at.Unix()},
	}
	if err := authority.CheckExclusive(env, leases); err == nil {
		t.Fatal("leases summing past the ceiling were accepted; that is the check that actually bounds exposure")
	}
}
