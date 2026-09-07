package effect

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Test11b_ReservationExecutesOnce is the durable half of FACTORY.md §9.11.
// Deriving a stable key says what "the same action" means; claiming it in a ref
// before executing is what actually stops the second order.
func Test11b_ReservationExecutesOnce(t *testing.T) {
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	req := order(t, c)

	r, hash, err := Reserve(v, req, "mini-a", at.Unix())
	if err != nil {
		t.Fatalf("first reservation refused: %v", err)
	}
	if r.State != StatePending {
		t.Fatalf("a fresh reservation is %q, want pending: the effect has not happened yet", r.State)
	}

	// The cell places the order, then records the far end's identifier for it.
	if _, err := Settle(v, r, hash, "PO-90210", at.Unix()+5); err != nil {
		t.Fatalf("settling: %v", err)
	}

	// Now the same intent arrives again — a restarted process, a re-run task, a
	// retry after a lost response. It must not execute.
	retry := order(t, c)
	retry.Payload = map[string]any{"quantity": 5, "gerber": "1e20deadbeef"} // other key order
	existing, _, err := Reserve(v, retry, "mini-a", at.Unix()+60)
	if !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a repeat of a completed action returned %v, want ErrAlreadyReserved", err)
	}
	// And it is told what happened, not merely that it may not proceed: the
	// answer to "did my order go through" is in the record.
	if existing.State != StateDone || existing.ExternalRef != "PO-90210" {
		t.Fatalf("the existing reservation did not report the outcome: %+v", existing)
	}

	// A genuinely different action is not blocked by it.
	other := order(t, c)
	other.Payload = map[string]any{"gerber": "1e20deadbeef", "quantity": 6}
	if _, _, err := Reserve(v, other, "mini-a", at.Unix()+60); err != nil {
		t.Fatalf("a different order was blocked by an unrelated reservation: %v", err)
	}
}

func TestTheSameCellCannotReserveOneKeyTwice(t *testing.T) {
	// Create-only is the whole locking mechanism. Two cells that both decide to
	// act produce one winner and one refusal, using nothing but varvig's ref CAS.
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	req := order(t, c)

	if _, _, err := Reserve(v, req, "mini-a", at.Unix()); err != nil {
		t.Fatal(err)
	}
	// The loser here reserves under its own cell's prefix, so it does *not*
	// collide — a reservation is scoped to the cell that holds the lease, and two
	// cells cannot hold one lease (§8.1). Same cell, same key is the collision
	// that matters.
	if _, _, err := Reserve(v, req, "mini-a", at.Unix()); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("the same cell reserved one key twice: %v", err)
	}
}

func TestPendingIsTheStateThatEscalates(t *testing.T) {
	// A crash between reserve and settle leaves pending, which is the honest
	// record of the one state that matters: the cell does not know whether the
	// order was placed.
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	r, hash, err := Reserve(v, order(t, c), "mini-a", at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Unresolved() {
		t.Fatal("a reserved-but-unsettled action did not report unresolved")
	}

	pending, err := Pending(v, "mini-a")
	if err != nil {
		t.Fatalf("listing pending reservations: %v", err)
	}
	if len(pending) != 1 || pending[0].Key != r.Key {
		t.Fatalf("pending = %+v, want the one unresolved action", pending)
	}

	// The cell cannot clear it by retrying, and cannot clear it by deleting the
	// reservation either: the key stays claimed.
	if _, _, err := Reserve(v, order(t, c), "mini-a", at.Unix()+3600); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a pending action was retried: %v", err)
	}

	// A timeout is not a failure. Recording one as a failure would assert that
	// no effect occurred, which is a different claim from "we never heard back".
	if _, err := Fail(v, r, hash, "", at.Unix()+10); err == nil {
		t.Fatal("a failure was recorded with no rejection behind it")
	}

	// Nor can the cell resolve its own unknown state.
	if _, err := Resolve(v, r, hash, true, "mini-a", "looked it up", at.Unix()+10); !errors.Is(err, ErrSelfAuthorization) {
		t.Fatalf("a cell resolved its own pending reservation: %v", err)
	}
	if _, err := Resolve(v, r, hash, true, "", "looked it up", at.Unix()+10); err == nil {
		t.Fatal("a pending reservation was resolved by nobody in particular")
	}

	// A higher principal checks the external system and records what it found.
	// That the record says a principal decided it is the point: it is the
	// difference between a confirmed outcome and an assumed one.
	if _, err := Resolve(v, r, hash, true, "overseer-a", "order PO-90210 exists at the fab", at.Unix()+600); err != nil {
		t.Fatalf("an overseer could not resolve a pending reservation: %v", err)
	}
	after, _, err := loadReservation(v, mustRef(t, "mini-a", r.Key))
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateDone || !strings.Contains(after.Detail, "overseer-a") {
		t.Fatalf("the resolution did not record who decided it: %+v", after)
	}
	if left, err := Pending(v, "mini-a"); err != nil || len(left) != 0 {
		t.Fatalf("pending = %+v (err %v), want empty after resolution", left, err)
	}
}

func TestASettledSpendMustBeLookUpAble(t *testing.T) {
	// The next question about an unexpected invoice is "which order was it", and
	// the answer has to be in the record.
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	r, hash, err := Reserve(v, order(t, c), "mini-a", at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(v, r, hash, "", at.Unix()+5); err == nil {
		t.Fatal("a spend was settled with no external reference to look up")
	}
}

func TestAFailedActionIsNotRetriedAutomatically(t *testing.T) {
	// A definite rejection means no effect occurred — but the key stays claimed.
	// Whether to authorize a fresh attempt is a decision for a higher principal,
	// not a loop behaviour (§6.7 rule 5).
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	r, hash, err := Reserve(v, order(t, c), "mini-a", at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Fail(v, r, hash, "fab rejected the gerber: layer count", at.Unix()+5); err != nil {
		t.Fatalf("recording a definite rejection: %v", err)
	}
	if _, _, err := Reserve(v, order(t, c), "mini-a", at.Unix()+60); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a failed action was silently retried: %v", err)
	}
	// It is not pending, though — a confirmed rejection is resolved, and listing
	// it as unknown-outcome would bury the reservations that really are unknown.
	if left, err := Pending(v, "mini-a"); err != nil || len(left) != 0 {
		t.Fatalf("pending = %+v (err %v), want empty: a confirmed rejection is resolved", left, err)
	}
}

func TestReservationRecordsTheInterfaceHash(t *testing.T) {
	// A reservation read back years later must still name an unambiguous
	// contract, even if the alias has since been re-pointed (§2.1).
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	r, _, err := Reserve(v, order(t, c), "mini-a", at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if r.Interface != c.Interface {
		t.Fatalf("interface = %q, want the hash %q", r.Interface, c.Interface)
	}
	if r.AuthorizedBy != "overseer-a" {
		t.Fatalf("authorized_by = %q; who authorized a spend is the first question asked about it", r.AuthorizedBy)
	}
}

func TestReserveRefusesAMalformedRequest(t *testing.T) {
	v := varvigcli.NewFake("test")
	c := fabrication(t)
	aliasOnly := c
	aliasOnly.Interface = ""
	if _, _, err := Reserve(v, order(t, aliasOnly), "mini-a", at.Unix()); err == nil {
		t.Fatal("an alias-only capability was reserved")
	}
	noTask := order(t, c)
	noTask.Task = ""
	if _, _, err := Reserve(v, noTask, "mini-a", at.Unix()); err == nil {
		t.Fatal("a request with no task id was reserved")
	}
}

func mustRef(t *testing.T, cellID, key string) string {
	t.Helper()
	name, err := cell.ReservationRef(cellID, key)
	if err != nil {
		t.Fatal(err)
	}
	return name
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
	v := varvigcli.Exec{Bin: bin, Dir: filepath.Join(dir, "repo")}

	c := fabrication(t)
	req := order(t, c)
	r, hash, err := Reserve(v, req, "mini-a", at.Unix())
	if err != nil {
		t.Fatalf("real core refused a reservation ref: %v", err)
	}

	// Create-only against a real core, which is the property the whole mechanism
	// rests on: whoever creates the ref executes, and everyone else is refused.
	if _, _, err := Reserve(v, req, "mini-a", at.Unix()+1); !errors.Is(err, ErrAlreadyReserved) {
		t.Fatalf("a repeat reservation returned %v, want ErrAlreadyReserved", err)
	}
	if pending, err := Pending(v, "mini-a"); err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v (err %v), want the one unresolved action", pending, err)
	}
	if _, err := Settle(v, r, hash, "PO-90210", at.Unix()+5); err != nil {
		t.Fatalf("settling against a real core: %v", err)
	}
	done, _, err := Reserve(v, req, "mini-a", at.Unix()+60)
	if !errors.Is(err, ErrAlreadyReserved) || done.ExternalRef != "PO-90210" {
		t.Fatalf("a settled reservation did not report its outcome: %+v (err %v)", done, err)
	}
	if pending, err := Pending(v, "mini-a"); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v (err %v), want empty after settlement", pending, err)
	}
}
