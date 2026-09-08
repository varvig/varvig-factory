package loop

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/effect"
	"github.com/varvig/varvig-factory/varvigcli"
)

// The loop's other tests run collapsed, so they cannot tell whether the cell
// reads its authority from the coordination replica or merely from the same
// repository that happens to hold everything. These run two distinct replicas
// and assert both halves: that the cell finds the lease where it actually lives,
// and that the work it produces stays out of the coordination repo.

// splitCell is effectCell with the two replicas actually separated: the
// envelope and lease go to a coordination Fake, the ticket to a project Fake.
func splitCell(t *testing.T, amount float64) (*Cell, *varvigcli.Fake, *varvigcli.Fake, *effect.Fake) {
	t.Helper()
	iface := boardInterface(t)
	coord, work := varvigcli.NewFake("coordination"), varvigcli.NewFake("project")
	fr := varvigcli.FactoryRepo{Varvig: coord}
	pr := varvigcli.ProjectRepo{Varvig: work}

	spec := fmt.Sprintf("Order the prototype run.\nfactory-requires: effect=pcb-fabrication@1 interface=%s\nfactory-effect: {\"gerber\":\"rev-c\",\"quantity\":5}", iface)
	work.AddTicket(effTicket, spec, varvigcli.Scope{Reads: []string{"hardware"}, Writes: []string{"hardware"}}, "approved")

	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: effClock.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 5000, Unit: "EUR", Quantity: 100}},
	}
	if _, err := authority.PublishEnvelope(fr, env, ""); err != nil {
		t.Fatal(err)
	}
	lease := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: amount, Unit: "EUR", Quantity: 20, IssuedAt: effClock.Unix(),
	}
	if _, err := authority.PublishLease(fr, lease, ""); err != nil {
		t.Fatal(err)
	}

	capability := effect.Capability{ID: "pcb-fabrication@1", Interface: iface, Effectful: true}
	fake := effect.NewFake(capability, 320, "EUR")
	ledger, err := budget.NewLedger(
		budget.Budget{InferenceDaily: 10, VerifyConcurrent: 1, StorageGB: 1, AttemptsDefault: 1, PerCallCost: 0.01},
		filepath.Join(t.TempDir(), "ledger.json"), effClock)
	if err != nil {
		t.Fatal(err)
	}
	c := &Cell{
		Capabilities: cell.Capabilities{
			CellID:  "mini-a",
			Effects: []cell.EffectCapability{{ID: "pcb-fabrication@1", Interface: iface}},
		},
		Factory:            fr,
		Project:            pr,
		Ledger:             ledger,
		Executors:          effect.Executors{fake},
		EffectAuthorizedBy: "overseer-a",
		EffectTTL:          3600,
		Now:                func() time.Time { return effClock },
		Log:                func(string) {},
	}
	return c, coord, work, fake
}

// TestACellSpendsFromTheCoordinationReplica is the end-to-end claim. The lease
// exists in one repository and the ticket in another, and the cell has to reach
// the right one for each or the order never happens.
func TestACellSpendsFromTheCoordinationReplica(t *testing.T) {
	c, coord, work, fake := splitCell(t, 1000)

	res, err := c.performEffect(t.Context(), effectTicket(t, work))
	if err != nil {
		t.Fatalf("performEffect across two replicas: %v", err)
	}
	if !res.Done {
		t.Fatalf("the order did not happen: %+v", res)
	}
	if fake.Count() != 1 {
		t.Fatalf("the effect happened %d times, want exactly 1", fake.Count())
	}

	// The spend is recorded on the lease, in the repository that issued it.
	lease, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spent != 320 || lease.Reserved != 0 {
		t.Fatalf("lease after settlement: spent=%g reserved=%g, want 320 and 0", lease.Spent, lease.Reserved)
	}

	// The reservation and the ticket's effect note are in the project repo, and
	// nothing about this cell's work reached the coordination repo.
	for name := range work.RefSnapshot() {
		if strings.HasPrefix(name, cell.LeasePrefix) || strings.HasPrefix(name, cell.EnvelopePrefix) {
			t.Errorf("authority ref %s leaked into the project replica", name)
		}
	}
	for name := range coord.RefSnapshot() {
		if strings.HasPrefix(name, cell.ReservationPrefix) {
			t.Errorf("reservation ref %s leaked into the coordination replica", name)
		}
	}
	notes, err := work.Notes(effTicket, cell.NoteEffect)
	if err != nil || len(notes) != 1 {
		t.Fatalf("the effect note belongs on the ticket in the project repo: %d notes, err %v", len(notes), err)
	}
}

// TestCapabilitiesArePublishedToTheCoordinationReplica pins a binding that was
// wrong on the first pass of the split and that nothing else caught: a cell's
// capabilities object says what the cell *is* within the factory, not anything
// about a codebase.
//
// Published per project, one cell would advertise as many identities as it has
// projects — and the peers that read it are asking factory-wide questions: an
// overseer sizing a lease, another cell deciding whether this one is equipped to
// verify its work.
func TestCapabilitiesArePublishedToTheCoordinationReplica(t *testing.T) {
	c, coord, work, _ := splitCell(t, 1000)
	c.Capabilities.Inference = cell.Inference{Tier: cell.TierNone}
	c.Capabilities.Roles = []cell.Role{cell.RoleBuild}

	if err := c.PublishCapabilities(); err != nil {
		t.Fatal(err)
	}
	ref, err := cell.CapabilitiesRef("mini-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ResolveRef(ref); err != nil {
		t.Errorf("%s is missing from the coordination replica: %v", ref, err)
	}
	if _, err := work.ResolveRef(ref); err == nil {
		t.Errorf("%s was published into the project replica; a cell has one identity per factory, not one per codebase", ref)
	}
}

// TestACellWithoutACoordinationReplicaWillNotStart: a cell whose authority has
// no home cannot know what it may do, so it refuses at startup rather than
// discovering it at the first order.
func TestACellWithoutACoordinationReplicaWillNotStart(t *testing.T) {
	c, _, work, _ := splitCell(t, 1000)
	c.Factory = varvigcli.FactoryRepo{}
	c.ClaimTTL = time.Minute
	// Enough configuration to get past the earlier checks, so the refusal under
	// test is the one that fires.
	c.Capabilities.Inference = cell.Inference{Tier: cell.TierNone}
	c.Capabilities.Roles = []cell.Role{cell.RoleBuild}
	err := c.Validate(t.Context())
	if err == nil {
		t.Fatal("a cell with no coordination replica must not start")
	}
	if !strings.Contains(err.Error(), "coordination") {
		t.Errorf("the refusal should name what is missing, got: %v", err)
	}
	_ = work
}

// TestTrustCurrencyFollowsTheFactoryPeer covers the semantic the split forces
// apart: promotion's freshness requirement (§4.3b) is about membership and
// allowed_keys, which live in the coordination repo, so it is that peer's
// reachability that decides it — not the project peer's.
func TestTrustCurrencyFollowsTheFactoryPeer(t *testing.T) {
	c, coord, work, _ := splitCell(t, 1000)
	c.Upstream, c.FactoryUpstream = "project-peer", "factory-peer"
	coord.Upstream = varvigcli.NewFake("factory-peer")
	work.Upstream = varvigcli.NewFake("project-peer")

	// Both reachable: trust is current and the cell is not offline.
	if offline := c.fetch(); offline {
		t.Fatal("both peers reachable should not be offline")
	}
	if !c.sync.Reachable {
		t.Fatal("both peers reachable should leave trust state current")
	}

	// The project peer goes away. The cell is offline from its work, but it has
	// not lost track of who may sign — so promotion is not what breaks.
	work.Partitioned = true
	if offline := c.fetch(); !offline {
		t.Fatal("an unreachable project peer is the offline mode")
	}
	if !c.sync.Reachable {
		t.Fatal("losing the project peer must not mark trust state stale; membership lives in the coordination repo")
	}

	// The factory peer goes away instead. Now trust is stale even though the
	// cell can see current work.
	work.Partitioned, coord.Partitioned = false, true
	if offline := c.fetch(); offline {
		t.Fatal("a reachable project peer is not offline, whatever the factory peer is doing")
	}
	if c.sync.Reachable {
		t.Fatal("an unreachable factory peer must mark trust state stale (§4.3b)")
	}
}
