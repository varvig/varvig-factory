package loop

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// A rendezvous set is what makes a factory live: any member may serve, several
// at once, and losing one is not losing the factory. These assert the three
// properties that claim rests on — every peer is contacted, no peer is
// privileged, and a set survives its members failing one by one.

// mesh builds a cell whose replicas can reach the named peers, and returns the
// peers so a test can take them down.
func mesh(t *testing.T, addrs ...string) (*Cell, map[string]*varvigcli.Fake, *varvigcli.Fake) {
	t.Helper()
	own := varvigcli.NewFake("mini-a")
	peers := map[string]*varvigcli.Fake{}
	for _, a := range addrs {
		peers[a] = varvigcli.NewFake(a)
	}
	own.Peers = peers
	fr, pr := varvigcli.Collapsed(own)
	c := &Cell{
		Capabilities: cell.Capabilities{
			CellID: "mini-a", Inference: cell.Inference{Tier: cell.TierNone},
			Roles: []cell.Role{cell.RoleBuild},
		},
		Factory:           fr,
		Project:           pr,
		Rendezvous:        Peers(addrs),
		FactoryRendezvous: Peers(addrs),
		Now:               func() time.Time { return time.Unix(1755820800, 0) },
		Log:               func(string) {},
	}
	return c, peers, own
}

// dialled returns the addresses a fake was asked to reach, in order.
func dialled(f *varvigcli.Fake, call string) []string {
	var out []string
	for _, c := range f.Calls {
		if c == call {
			out = append(out, c)
		}
	}
	return out
}

// TestEveryPeerIsContacted is the property that makes the set a mesh rather
// than a fallback list.
//
// Stopping at the first peer that answers would be cheaper and would quietly
// reintroduce a coordinator: peer B may hold a lease or an attempt that A has
// never seen, and taking A's answer and stopping means relying on A to relay
// the rest.
func TestEveryPeerIsContacted(t *testing.T) {
	c, peers, _ := mesh(t, "a", "b", "c")
	// Give each peer a ticket only it knows about.
	for addr, p := range peers {
		p.AddTicket("ticket-from-"+addr, "work", varvigcli.Scope{Reads: []string{"src"}}, "approved")
	}

	if offline := c.fetch(); offline {
		t.Fatal("three reachable peers is not offline")
	}
	ids, err := c.Project.TicketIDs()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	want := []string{"ticket-from-a", "ticket-from-b", "ticket-from-c"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("saw %v, want every peer's ticket %v — a set that stops at the first answer is a fallback list, not a mesh", ids, want)
	}
}

// TestTheSetSurvivesItsMembers: losing peers one at a time degrades the view
// but never the cell. This is the whole reason for a set.
func TestTheSetSurvivesItsMembers(t *testing.T) {
	c, peers, own := mesh(t, "a", "b", "c")
	for addr, p := range peers {
		p.AddTicket("ticket-from-"+addr, "work", varvigcli.Scope{Reads: []string{"src"}}, "approved")
	}

	// Two of three gone: still reachable, still current for trust.
	delete(own.Peers, "a")
	delete(own.Peers, "b")
	if offline := c.fetch(); offline {
		t.Fatal("one reachable peer of three is not offline")
	}
	if !c.sync.Reachable {
		t.Fatal("one reachable coordination peer keeps trust state current")
	}

	// The last one goes: now the cell is offline and trust is stale, and it
	// says so rather than pretending.
	delete(own.Peers, "c")
	if offline := c.fetch(); !offline {
		t.Fatal("no reachable peer is the offline mode")
	}
	if c.sync.Reachable {
		t.Fatal("no reachable coordination peer must mark trust stale (§4.3b)")
	}
	if !c.sync.Configured {
		t.Fatal("a configured set that is unreachable is still configured; forgetting that would read as a single-cell deployment")
	}
}

// TestAnEmptySetIsNotOffline: a single-cell deployment has nothing to be
// disconnected from, and treating it as offline would apply the tighter offline
// budget forever and make promotion impossible for the simplest configuration.
func TestAnEmptySetIsNotOffline(t *testing.T) {
	c, _, _ := mesh(t)
	if offline := c.fetch(); offline {
		t.Fatal("a cell with no peers is not offline")
	}
	if c.sync.Configured {
		t.Fatal("no configured set must not read as a stale one")
	}
}

// TestNoPeerIsSystematicallyFirst is the anti-coordinator property, and it is
// not cosmetic.
//
// Reserved-ref replication takes what we lack and reports rather than overwrites
// when both sides hold a ref at unrelated values, so on a contested claim the
// peer contacted first is the one whose version we adopt. A fixed order would
// hand that to whoever was typed into the config first.
func TestNoPeerIsSystematicallyFirst(t *testing.T) {
	c, _, own := mesh(t, "a", "b", "c")

	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		own.Calls = nil
		var order []string
		c.Shuffle = nil // the real source
		c.visit(c.Rendezvous, "test", func(addr string) error {
			order = append(order, addr)
			return nil
		})
		if len(order) != 3 {
			t.Fatalf("visited %v, want all three", order)
		}
		seen[order[0]] = true
	}
	if len(seen) < 2 {
		t.Fatalf("only %v was ever contacted first over 40 passes; a fixed order makes that peer a coordinator by the back door", seen)
	}
}

// TestAnInjectedOrderIsHonoured: the shuffle source is injectable, because a
// test that cannot fix the order cannot assert what a pass did.
func TestAnInjectedOrderIsHonoured(t *testing.T) {
	c, _, _ := mesh(t, "a", "b", "c")
	c.Shuffle = func(n int, swap func(i, j int)) {} // identity: leave as configured

	var order []string
	c.visit(c.Rendezvous, "test", func(addr string) error {
		order = append(order, addr)
		return nil
	})
	if strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("visited %v, want the configured order when the shuffle is the identity", order)
	}
}

// TestAPeerThatDisagreesAboutTheBranchIsStillReached is the distinction between
// *reached* and *answered*, and it is load-bearing.
//
// varvig's head push moves under a force-with-lease against one tracking ref, so
// with several peers at most one can accept a head push and the rest are refused
// every pass as a matter of course. Those peers were reached — notes and
// reserved refs replicated, the authority state arrived, only the branch
// disagreed. Counting that as unreachable would mark trust stale and stop
// promotion for a reason that has nothing to do with trust.
func TestAPeerThatDisagreesAboutTheBranchIsStillReached(t *testing.T) {
	c, _, _ := mesh(t, "a", "b")

	v := c.visit(c.Rendezvous, "test", func(addr string) error {
		return fmt.Errorf("push rejected: compare-and-swap conflict on refs/heads/main")
	})
	if v.unreachable() {
		t.Fatal("a refused head compare-and-swap is a disagreement about code, not a failure to make contact")
	}
	if v.answered != 0 {
		t.Fatalf("answered = %d, want 0: nothing fully applied", v.answered)
	}
	if v.reached != 2 {
		t.Fatalf("reached = %d, want 2", v.reached)
	}
	if len(v.failures) != 2 {
		t.Fatalf("both refusals should be reported, got %v", v.failures)
	}
}

// TestSpendReachesEveryCoordinationPeer: a settled lease is the only record that
// money was spent, so it goes to every peer rather than to one and a hope.
func TestSpendReachesEveryCoordinationPeer(t *testing.T) {
	c, peers, own := mesh(t, "a", "b", "c")
	lease := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 100000, Unit: "EUR", Quantity: 20,
		IssuedAt: c.now().Unix(), Spent: 32000,
	}
	if _, err := authority.PublishLease(c.Factory, lease, ""); err != nil {
		t.Fatal(err)
	}
	_ = own

	if err := c.pushFactory(); err != nil {
		t.Fatal(err)
	}
	ref, err := cell.LeaseRef("mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	for addr, p := range peers {
		if _, ok := p.RefSnapshot()[ref]; !ok {
			t.Errorf("peer %s never received the settled lease; spend reported to some peers and not others is spend nobody can total", addr)
		}
	}
}
