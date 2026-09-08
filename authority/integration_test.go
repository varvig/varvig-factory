package authority

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/varvig/varvig-factory/varvigcli"
)

// This file drives the *real* varvig binary, and skips when one is not on PATH.
//
// It exists because of a specific class of bug: every other test here runs
// against varvigcli.Fake, and a fake that is stricter than reality hides a
// broken client rather than exposing one. That has already happened once in this
// repository. The questions only a real core can answer are whether it accepts
// `refs/envelopes/`, `refs/leases/` and `refs/reservations/` at all — the
// namespaces are new, and core reserves some prefixes for itself — and whether a
// create-only publish and a CAS settlement behave the way the Fake models them.
func realRepo(t *testing.T) varvigcli.FactoryRepo {
	t.Helper()
	bin, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	cmd := exec.Command(bin, "init", "repo")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("varvig init: %v: %s", err, out)
	}
	return varvigcli.FactoryRepo{Varvig: varvigcli.Exec{Bin: bin, Dir: repo}}
}

func TestIntegrationEnvelopeAndLeaseRefsAreAccepted(t *testing.T) {
	v := realRepo(t)
	env := envelope()

	if _, err := PublishEnvelope(v, env, ""); err != nil {
		t.Fatalf("real core refused an envelope ref: %v", err)
	}
	back, envHash, err := LoadEnvelope(v, env.Overseer)
	if err != nil {
		t.Fatalf("reading the envelope back: %v", err)
	}
	if len(back.Ceilings) != len(env.Ceilings) || back.Ceilings[0].Spend != env.Ceilings[0].Spend {
		t.Fatalf("the envelope did not survive the round trip: %+v", back)
	}

	// Create-only must actually be create-only. This is the exact bug that got
	// through before: omitting the expected-old argument makes varvig treat an
	// update as unconditional, and the Fake enforced the rule the client did not.
	if _, err := PublishEnvelope(v, env, ""); !errors.Is(err, varvigcli.ErrCAS) {
		t.Fatalf("a second create-only publish returned %v, want ErrCAS", err)
	}
	// And a CAS against the value that was read succeeds.
	env.SetAt++
	if _, err := PublishEnvelope(v, env, envHash); err != nil {
		t.Fatalf("a CAS update against the read value was refused: %v", err)
	}

	l := lease("mini-a", "pcb-fabrication@1", 1000)
	l.ReclaimAfter = now.Unix()
	if _, err := PublishLease(v, l, ""); err != nil {
		t.Fatalf("real core refused a lease ref: %v", err)
	}
	held, leaseHash, err := LoadLease(v, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatalf("reading the lease back: %v", err)
	}

	// Settlement through a real CAS.
	held.Spent = 320
	if _, err := PublishLease(v, held, leaseHash); err != nil {
		t.Fatalf("settling spend: %v", err)
	}
	if _, err := PublishLease(v, held, leaseHash); !errors.Is(err, varvigcli.ErrCAS) {
		t.Fatalf("a replayed settlement returned %v, want ErrCAS", err)
	}

	// Listing must find them through `varvig read refs`, not only through the
	// Fake's map.
	all, err := Leases(v, "")
	if err != nil {
		t.Fatalf("listing leases: %v", err)
	}
	if len(all) != 1 || all[0].Spent != 320 {
		t.Fatalf("listed %d leases: %+v", len(all), all)
	}
	if got := Exposure(all)["pcb-fabrication@1"]; got != 680 {
		t.Fatalf("exposure = %g, want 680", got)
	}

	// Reclaim refuses a lease with spend even against a real core, then collects
	// the unspent one — and the delete must actually remove the ref.
	settled, settledHash, err := LoadLease(v, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	past := func() int64 { return now.Unix() + 86400 }
	if err := Reclaim(v, settled, settledHash, past); err == nil {
		t.Fatal("a lease with recorded spend was reclaimed")
	}
	settled.Spent = 0
	settledHash, err = PublishLease(v, settled, settledHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reclaim(v, settled, settledHash, past); err != nil {
		t.Fatalf("reclaiming an unspent, timed-out lease: %v", err)
	}
	if _, _, err := LoadLease(v, "mini-a", "pcb-fabrication@1"); !errors.Is(err, varvigcli.ErrNoRef) {
		t.Fatalf("the lease ref survived reclaim against a real core: %v", err)
	}
}
