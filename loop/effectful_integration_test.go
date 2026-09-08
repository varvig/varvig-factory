package loop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/effect"
	"github.com/varvig/varvig-factory/varvigcli"
)

// TestIntegrationEffectfulTicketAgainstRealCore drives the whole path against
// the real varvig binary: a ticket minted by core, a lease and envelope in real
// refs, a reservation ref core accepts, and a note core can read back.
//
// The unit tests above run against the Fake, which is stricter than reality in
// places and looser in others. This is the only test that can say the ticket
// directive survives core's own spec storage, and that the ordering of writes
// works against a real ref store rather than a map.
func TestIntegrationEffectfulTicketAgainstRealCore(t *testing.T) {
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
	repo := filepath.Join(dir, "repo")
	v := varvigcli.Exec{Bin: bin, Dir: repo}
	fr, pr := varvigcli.Collapsed(v)

	iface := boardInterface(t)
	spec := fmt.Sprintf("Order the prototype run.\nfactory-requires: effect=pcb-fabrication@1 interface=%s\nfactory-effect: {\"gerber\":\"rev-c\",\"quantity\":5}", iface)

	// Mint the ticket through core, so the directive goes through core's own
	// spec storage rather than a fixture this test wrote.
	mint := exec.Command(bin, "tickets", "new", "-m", spec)
	mint.Dir = repo
	out, err := mint.CombinedOutput()
	if err != nil {
		t.Fatalf("tickets new: %v: %s", err, out)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		t.Fatalf("could not read a ticket id from %q", out)
	}
	ticketID := fields[1]

	// The directive must survive the round trip through core verbatim, or the
	// idempotency key derived from it would differ between two readings.
	readBack, err := v.Spec(ticketID)
	if err != nil {
		t.Fatal(err)
	}
	req := claim.ParseRequirements(readBack)
	if !req.Effectful() {
		t.Fatalf("the effect directive did not survive core's spec storage: %q", readBack)
	}
	if req.Effect.Malformed != "" {
		t.Fatalf("the directive read back malformed: %s", req.Effect.Malformed)
	}
	if req.Effect.Interface != iface {
		t.Fatalf("interface = %q, want %q", req.Effect.Interface, iface)
	}

	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: effClock.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 5000, Unit: "EUR", Quantity: 100}},
	}
	if _, err := authority.PublishEnvelope(fr, env, ""); err != nil {
		t.Fatalf("real core refused an envelope: %v", err)
	}
	lease := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 1000, Unit: "EUR", Quantity: 20, IssuedAt: effClock.Unix(),
	}
	if _, err := authority.PublishLease(fr, lease, ""); err != nil {
		t.Fatalf("real core refused a lease: %v", err)
	}

	capability := effect.Capability{ID: "pcb-fabrication@1", Interface: iface, Effectful: true}
	fake := effect.NewFake(capability, 320, "EUR")
	c := &Cell{
		Capabilities: cell.Capabilities{
			CellID:  "mini-a",
			Effects: []cell.EffectCapability{{ID: "pcb-fabrication@1", Interface: iface}},
		},
		Factory:            fr,
		Project:            pr,
		Executors:          effect.Executors{fake},
		EffectAuthorizedBy: "overseer-a",
		EffectTTL:          3600,
		Now:                func() time.Time { return effClock },
		Log:                func(string) {},
	}

	if grants := c.effectGrants(); len(grants) != 1 {
		t.Fatalf("grants = %d against a real repo, want 1", len(grants))
	}

	ticket := claim.Ticket{
		ID: ticketID, Object: ticketID, Spec: readBack, Status: "approved",
		Scope: varvigcli.Scope{Reads: []string{"hardware"}, Writes: []string{"hardware"}},
	}
	res, err := c.performEffect(context.Background(), ticket)
	if err != nil {
		t.Fatalf("performing the effect against a real core: %v", err)
	}
	if !res.Done {
		t.Fatalf("the order did not happen: %+v", res)
	}
	if fake.Count() != 1 {
		t.Fatalf("the effect happened %d times, want 1", fake.Count())
	}

	// Real refs carry the outcome: the lease charged, nothing left pending.
	after, _, err := authority.LoadLease(fr, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Spent != 320 || after.Reserved != 0 {
		t.Fatalf("lease against real core: spent=%g reserved=%g", after.Spent, after.Reserved)
	}
	if pending, err := effect.Pending(pr, "mini-a"); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %v (err %v), want empty", pending, err)
	}

	// A second pass does not re-order, against a real ref store.
	if _, err := c.performEffect(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if fake.Count() != 1 {
		t.Fatalf("a second pass re-ordered: executed %d times", fake.Count())
	}
}
