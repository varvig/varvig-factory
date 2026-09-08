package loop

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// freePort asks the kernel for a port nothing is using, so three peers can be
// served concurrently without a fixed-port collision.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// servedRepo initialises a repository, seeds it, and serves it until the test
// ends. It returns the address to dial.
func servedRepo(t *testing.T, bin, dir, label string, seed func(v varvigcli.Exec)) string {
	t.Helper()
	init := exec.Command(bin, "init", label)
	init.Dir = dir
	init.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("varvig init %s: %v: %s", label, err, out)
	}
	path := filepath.Join(dir, label)
	seed(varvigcli.Exec{Bin: bin, Dir: path})

	addr := freePort(t)
	srv := exec.Command(bin, "serve", addr)
	srv.Dir = path
	if err := srv.Start(); err != nil {
		t.Fatalf("varvig serve %s: %v", label, err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
	})
	// Wait for the listener rather than sleeping a guessed interval.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("peer %s never started listening on %s", label, addr)
	return ""
}

// TestIntegrationARealThreePeerMesh is the rendezvous set against real varvig:
// three peers each holding something the others do not, and a cell that ends the
// pass knowing all of it and having reported its spend to every one of them.
//
// The Fake cannot carry this on its own. Its exchange is a map copy, so "every
// peer was contacted" is nearly true by construction; here each peer is a
// separate process with its own ref store and its own compare-and-swap, and the
// head disagreements that make a mesh awkward are real.
func TestIntegrationARealThreePeerMesh(t *testing.T) {
	bin, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()

	// Each peer holds one ticket only it knows about, so "did we contact all
	// three" has an answer that cannot be faked by contacting one of them.
	var addrs []string
	for _, label := range []string{"peer-a", "peer-b", "peer-c"} {
		label := label
		addrs = append(addrs, servedRepo(t, bin, dir, label, func(v varvigcli.Exec) {
			mint := exec.Command(bin, "tickets", "new", "-m", "work only "+label+" knows about")
			mint.Dir = v.Dir
			mint.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
			if out, err := mint.CombinedOutput(); err != nil {
				t.Fatalf("tickets new in %s: %v: %s", label, err, out)
			}
		}))
	}

	// The cell's own repository, with no peers' state in it yet.
	cellInit := exec.Command(bin, "init", "cell")
	cellInit.Dir = dir
	cellInit.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
	if out, err := cellInit.CombinedOutput(); err != nil {
		t.Fatalf("varvig init cell: %v: %s", err, out)
	}
	own := varvigcli.Exec{Bin: bin, Dir: filepath.Join(dir, "cell")}
	fr, pr := varvigcli.Collapsed(own)

	var logs []string
	c := &Cell{
		Capabilities: cell.Capabilities{
			CellID: "mini-a", Inference: cell.Inference{Tier: cell.TierNone},
			Roles: []cell.Role{cell.RoleBuild},
		},
		Factory:           fr,
		Project:           pr,
		Rendezvous:        Peers(addrs),
		FactoryRendezvous: Peers(addrs),
		Branch:            "refs/heads/main",
		Now:               func() time.Time { return time.Unix(1755820800, 0) },
		Log:               func(s string) { logs = append(logs, s) },
	}

	if offline := c.fetch(); offline {
		t.Fatalf("three reachable peers is not offline; logs: %v", logs)
	}
	if !c.sync.Reachable {
		t.Fatalf("three reachable coordination peers keeps trust current; logs: %v", logs)
	}

	// Every peer's ticket arrived, which only happens if every peer was asked.
	ids, err := c.Project.TicketIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("holding %d tickets, want one from each of three peers — logs: %v", len(ids), logs)
	}

	// Now the spend. It must reach every coordination peer, not whichever one
	// happens to accept a head push.
	lease := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 100000, Unit: "EUR", Quantity: 20,
		IssuedAt: c.now().Unix(), Spent: 32000,
	}
	if _, err := authority.PublishLease(c.Factory, lease, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.pushFactory(); err != nil {
		t.Fatalf("pushFactory: %v", err)
	}

	ref, err := cell.LeaseRef("mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	for i, label := range []string{"peer-a", "peer-b", "peer-c"} {
		peer := varvigcli.Exec{Bin: bin, Dir: filepath.Join(dir, label)}
		if _, err := peer.ResolveRef(ref); err != nil {
			t.Errorf("%s (%s) never received the settled lease: %v — spend reported to some peers and not others is spend nobody can total",
				label, addrs[i], err)
		}
	}

	// And losing peers degrades the view without stopping the cell.
	c.Rendezvous, c.FactoryRendezvous = Peers{addrs[0]}, Peers{addrs[0]}
	if offline := c.fetch(); offline {
		t.Error("one reachable peer of a set is not offline")
	}
	c.Rendezvous = Peers{"127.0.0.1:1"} // nothing listening
	c.FactoryRendezvous = Peers{"127.0.0.1:1"}
	if offline := c.fetch(); !offline {
		t.Error("a set with nothing reachable is the offline mode")
	}
	if c.sync.Reachable {
		t.Error("a set with nothing reachable must mark trust stale (§4.3b)")
	}
	if !strings.Contains(fmt.Sprint(logs), "no project peer reachable") {
		t.Errorf("the cell should say why it went offline; logs: %v", logs)
	}
}
