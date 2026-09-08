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

// offered reserves without taking — the connector-served shape, where the cell's
// part ends at the offer.
func offered(t *testing.T, v repos, g authority.Grant, c Capability, deadline int64) Claim {
	t.Helper()
	claim, err := Reserve(v.F, v.P, order(t, c), "mini-a", g, at.Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = Offer(v.P, claim, deadline)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func TestAConnectorFindsAndTakesWork(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)

	// The inbox is derived from repository state, not a queue: a connector that
	// restarts sees the same list.
	awaiting, err := Awaiting(v.P, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(awaiting) != 1 || awaiting[0].Key != claim.Reservation.Key {
		t.Fatalf("awaiting = %+v, want the one offer", awaiting)
	}

	taken, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-connector", at.Unix()+1, at.Unix()+600)
	if err != nil {
		t.Fatalf("taking an offer: %v", err)
	}
	if taken.Reservation.TakenBy != "fab-connector" || taken.Reservation.State != StatePending {
		t.Fatalf("after taking: %+v", taken.Reservation)
	}

	// Once taken it leaves the inbox, so a second connector does not see it.
	if awaiting, err := Awaiting(v.P, c); err != nil || len(awaiting) != 0 {
		t.Fatalf("a taken reservation is still offered: %+v (err %v)", awaiting, err)
	}
}

func TestTwoConnectorsRacingProduceOneOrder(t *testing.T) {
	// The whole exclusion mechanism, and it is varvig's ordinary ref CAS rather
	// than a lock, a lease or a queue.
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)

	if _, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-a", at.Unix()+1, 0); err != nil {
		t.Fatal(err)
	}
	loser, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-b", at.Unix()+2, 0)
	if !errors.Is(err, ErrNotOffered) {
		t.Fatalf("a second connector took the same offer: %v", err)
	}
	// The loser is told who is acting, not merely that it lost — otherwise its
	// only options are to retry or to guess.
	if !strings.Contains(err.Error(), "fab-a") || loser.Reservation.TakenBy != "fab-a" {
		t.Fatalf("the loser was not told the holder: %v / %+v", err, loser.Reservation)
	}
}

func TestOnlyTheHolderMayReport(t *testing.T) {
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)
	taken, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-a", at.Unix()+1, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Report(v.P, taken, "fab-b", true, "PO-1", 0, "", at.Unix()+5); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("a connector reported on work it does not hold: %v", err)
	}
	// And nothing may be reported on an offer nobody has taken.
	if _, err := Report(v.P, claim, "fab-a", true, "PO-1", 0, "", at.Unix()+5); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("an untaken offer accepted a report: %v", err)
	}
}

func TestAConnectorReportsAndOnlyTheCellSpends(t *testing.T) {
	// The line that must not move: a connector states a fact, the cell applies it
	// under the lease. Same containment core gives a bridge, which may sign a
	// weak attestation and never a strong one.
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)
	taken, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-a", at.Unix()+1, 0)
	if err != nil {
		t.Fatal(err)
	}

	reported, err := Report(v.P, taken, "fab-a", true, "PO-90210", 35540, "shipped", at.Unix()+5)
	if err != nil {
		t.Fatalf("reporting: %v", err)
	}
	if reported.Reservation.State != StateReported {
		t.Fatalf("state = %q, want reported", reported.Reservation.State)
	}

	// The connector did not move money: the hold still stands and nothing is
	// spent, because reporting is not settling.
	mid, _, err := authority.LoadLease(v.F, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if mid.Spent != 0 || mid.Reserved != 32000 {
		t.Fatalf("a connector's report moved money: spent=%s reserved=%s", mid.Spent, mid.Reserved)
	}

	// The cell settles from the report, at the reported actual.
	lease, leaseHash, err := authority.LoadLease(v.F, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	toSettle, err := LoadClaim(v.P, "mini-a", reported.Reservation.Key, lease, leaseHash)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := SettleReported(v.F, v.P, toSettle, at.Unix()+10)
	if err != nil {
		t.Fatalf("settling a report: %v", err)
	}
	if settled.Reservation.State != StateDone || settled.Lease.Spent != 35540 || settled.Lease.Reserved != 0 {
		t.Fatalf("after settlement: state=%q spent=%s reserved=%s",
			settled.Reservation.State, settled.Lease.Spent, settled.Lease.Reserved)
	}
}

func TestALyingConnectorIsBoundedByTheLease(t *testing.T) {
	// A connector is untrusted. What contains it is not a check on its honesty
	// but the lease: an actual beyond the allocation cannot be recorded, so the
	// exposure of a compromised connector is an amount the overseer chose.
	c := fabrication(t)
	v, g := leased(t, 40000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)
	taken, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-a", at.Unix()+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Report(v.P, taken, "fab-a", true, "PO-1", 999999, "", at.Unix()+5); err != nil {
		t.Fatalf("reporting: %v", err)
	}

	lease, leaseHash, err := authority.LoadLease(v.F, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	toSettle, err := LoadClaim(v.P, "mini-a", taken.Reservation.Key, lease, leaseHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SettleReported(v.F, v.P, toSettle, at.Unix()+10); err == nil {
		t.Fatal("a connector's inflated figure was applied to the lease")
	}
	after, _, err := authority.LoadLease(v.F, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Spent != 0 {
		t.Fatalf("spent = %s after a refused settlement", after.Spent)
	}
}

func TestAConnectorCannotInventWork(t *testing.T) {
	// It can only answer a reservation a cell created and an overseer
	// authorized. There is no call here that makes one.
	c := fabrication(t)
	v, _ := leased(t, 100000, 20)

	if awaiting, err := Awaiting(v.P, c); err != nil || len(awaiting) != 0 {
		t.Fatalf("awaiting = %+v (err %v), want nothing before anything is offered", awaiting, err)
	}
	if _, err := Take(v.P, "mini-a", strings.Repeat("a", 64), "fab-a", at.Unix(), 0); err == nil {
		t.Fatal("a connector took a reservation that does not exist")
	}
	if _, err := Take(v.P, "mini-a", strings.Repeat("a", 64), "", at.Unix(), 0); err == nil {
		t.Fatal("an anonymous connector took work")
	}
}

func TestAConnectorThatDoesNotKnowReportsNothing(t *testing.T) {
	// "We never heard back" is not a report. A connector that guesses is worse
	// than one that goes quiet, so the shapes that would let it guess are refused.
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)
	taken, err := Take(v.P, "mini-a", claim.Reservation.Key, "fab-a", at.Unix()+1, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Report(v.P, taken, "fab-a", true, "", 0, "", at.Unix()+5); err == nil {
		t.Fatal("a success was reported with no external reference")
	}
	if _, err := Report(v.P, taken, "fab-a", false, "", 0, "", at.Unix()+5); err == nil {
		t.Fatal("a rejection was reported with no refusal behind it")
	}

	// Going quiet leaves it pending, which is the honest record and escalates.
	pending, err := Pending(v.P, "mini-a")
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v (err %v), want the one taken-but-unreported action", pending, err)
	}
}

func TestAnUntakenOfferIsNotAnUnknownOutcome(t *testing.T) {
	// The distinction the state machine exists for. Nothing has taken the offer,
	// so nothing has happened — it must not read as an order that may be in
	// flight, or an operator would escalate on every reservation ever made.
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)

	if claim.Reservation.Unresolved() {
		t.Fatal("an untaken offer reported an unknown outcome")
	}
	if pending, err := Pending(v.P, "mini-a"); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, want empty: nothing has acted yet", pending)
	}
	// It does still hold money, though, which is what the expiry sweep is for.
	if !claim.Reservation.Open() {
		t.Fatal("an offer awaiting a connector did not report open")
	}
	lease, leaseHash, released, err := ReleaseExpired(v.F, v.P, claim.Lease, claim.LeaseHash, at.Unix()+7200)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || lease.Reserved != 0 {
		t.Fatalf("an expired offer did not return its headroom: released=%d reserved=%s", len(released), lease.Reserved)
	}
	_ = leaseHash
}

func TestTheKeyStaysClaimedEvenForAnUntakenOffer(t *testing.T) {
	// Releasing the headroom must not release the right to act. Re-offering the
	// same action would be a second order the moment a slow connector wakes up.
	c := fabrication(t)
	v, g := leased(t, 100000, 20)
	claim := offered(t, v, g, c, at.Unix()+600)
	lease, _, _, err := ReleaseExpired(v.F, v.P, claim.Lease, claim.LeaseHash, at.Unix()+7200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(v.F, v.P, order(t, c), "mini-a", authority.Grant{Envelope: g.Envelope, Lease: &lease}, at.Unix()+7200, 3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("an expired offer released its key: %v", err)
	}
}

// TestIntegrationConnectorExchangeAgainstRealCore drives the whole exchange
// through real refs, and skips when no varvig binary is present.
//
// The exclusion here rests entirely on varvig's compare-and-swap. Against the
// Fake that is a map guarded by a mutex; against a real core it is a ref store on
// disk, and only this can say the two behave the same. A fake stricter than
// reality has hidden a bug in this repository before.
func TestIntegrationConnectorExchangeAgainstRealCore(t *testing.T) {
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
	// One real repository, both roles — the collapsed configuration, which is
	// also the one this test can build with a single `varvig init`.
	fr, pr := varvigcli.Collapsed(varvigcli.Exec{Bin: bin, Dir: filepath.Join(dir, "repo")})
	v := repos{F: fr, P: pr}

	c := fabrication(t)
	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: at.Unix(),
		Ceilings: []authority.Ceiling{{Capability: c.ID, Spend: 500000, Unit: "EUR", Quantity: 100}},
	}
	if _, err := authority.PublishEnvelope(v.F, env, ""); err != nil {
		t.Fatal(err)
	}
	l := authority.Lease{
		CellID: "mini-a", Capability: c.ID, Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 100000, Unit: "EUR", Quantity: 20, IssuedAt: at.Unix(),
	}
	leaseHash, err := authority.PublishLease(v.F, l, "")
	if err != nil {
		t.Fatal(err)
	}
	g := authority.Grant{Envelope: env, Lease: &l, LeaseHash: leaseHash}

	claim := offered(t, v, g, c, at.Unix()+600)
	awaiting, err := Awaiting(v.P, c)
	if err != nil || len(awaiting) != 1 {
		t.Fatalf("awaiting against a real core = %+v (err %v)", awaiting, err)
	}

	// Two connectors race on real refs. Exactly one wins.
	first, firstErr := Take(v.P, "mini-a", claim.Reservation.Key, "fab-a", at.Unix()+1, 0)
	_, secondErr := Take(v.P, "mini-a", claim.Reservation.Key, "fab-b", at.Unix()+2, 0)
	if firstErr != nil {
		t.Fatalf("the first take failed: %v", firstErr)
	}
	if !errors.Is(secondErr, ErrNotOffered) {
		t.Fatalf("a real core let two connectors take one offer: %v", secondErr)
	}

	reported, err := Report(v.P, first, "fab-a", true, "PO-90210", 0, "", at.Unix()+5)
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := authority.LoadLease(v.F, "mini-a", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Spent != 0 {
		t.Fatalf("a report moved money against a real core: spent=%s", pending.Spent)
	}

	toSettle, err := LoadClaim(v.P, "mini-a", reported.Reservation.Key, pending, mustHash(t, v, "mini-a", reported.Reservation.Key))
	if err != nil {
		t.Fatal(err)
	}
	settled, err := SettleReported(v.F, v.P, toSettle, at.Unix()+10)
	if err != nil {
		t.Fatalf("settling against a real core: %v", err)
	}
	if settled.Reservation.State != StateDone || settled.Lease.Spent != 32000 {
		t.Fatalf("after settlement: state=%q spent=%s", settled.Reservation.State, settled.Lease.Spent)
	}
}

// mustHash reloads the lease hash a settlement needs to CAS against.
func mustHash(t *testing.T, v repos, cellID, key string) string {
	t.Helper()
	name, err := cell.LeaseRef(cellID, "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	h, err := v.F.ResolveRef(name)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
