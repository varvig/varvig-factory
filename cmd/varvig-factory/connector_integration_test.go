package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The connector door, driven the way a vendor would drive it: as a separate
// process running this binary, with no Go import anywhere.
//
// That is the whole point of the protocol. A connector that had to be compiled
// into the factory binary would be the rebuild the protocol exists to avoid, and
// until these verbs existed that was the only way to write one. This test is the
// proof they are reachable — it shells out, parses JSON, and never links against
// the effect package.
func TestIntegrationAConnectorCanBeAShellScript(t *testing.T) {
	varvig, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()

	// Build the factory binary, because a connector runs it rather than imports
	// it, and a test that called the functions directly would prove nothing
	// about whether a vendor could.
	factory := filepath.Join(dir, "varvig-factory")
	build := exec.Command("go", "build", "-o", factory, ".")
	if out, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("building the factory binary: %v: %s", berr, out)
	}

	repo := filepath.Join(dir, "repo")
	init := exec.Command(varvig, "init", "repo")
	init.Dir = dir
	init.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
	if out, ierr := init.CombinedOutput(); ierr != nil {
		t.Fatalf("varvig init: %v: %s", ierr, out)
	}

	run := func(args ...string) (string, error) {
		cmd := exec.Command(factory, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
		out, rerr := cmd.CombinedOutput()
		return string(out), rerr
	}

	if out, err := run("init", "--profile", "micro", "--cell-id", "mini-a"); err != nil {
		t.Fatalf("factory init: %v: %s", err, out)
	}

	// Publish an interface, exactly as an operator would.
	schema := filepath.Join(dir, "board.json")
	if err := os.WriteFile(schema, []byte(`{"gerber":"string","quantity":"integer"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run("interfaces", "publish", "--alias", "pcb-fabrication@1", "--schema", schema)
	if err != nil {
		t.Fatalf("publishing an interface: %v: %s", err, out)
	}
	var published struct{ Alias, Hash string }
	if jerr := json.Unmarshal([]byte(out), &published); jerr != nil {
		t.Fatalf("publish did not emit JSON a script could read: %v\n%s", jerr, out)
	}
	if published.Hash == "" {
		t.Fatalf("publish emitted no hash: %s", out)
	}

	// The registry lists it, which is how a connector discovers what it could
	// serve without being told out of band.
	out, err = run("interfaces", "list")
	if err != nil {
		t.Fatalf("listing interfaces: %v: %s", err, out)
	}
	var listed []struct{ Alias, Hash string }
	if jerr := json.Unmarshal([]byte(out), &listed); jerr != nil {
		t.Fatalf("list did not emit JSON: %v\n%s", jerr, out)
	}
	if len(listed) != 1 || listed[0].Alias != "pcb-fabrication@1" || listed[0].Hash != published.Hash {
		t.Fatalf("registry = %+v, want the alias just published", listed)
	}

	// Awaiting with nothing offered is an empty answer, not an error: a
	// connector polls, and a poll that found nothing is the normal case.
	out, err = run("connector", "awaiting", "--alias", "pcb-fabrication@1")
	if err != nil {
		t.Fatalf("awaiting with nothing offered should succeed: %v: %s", err, out)
	}
	var offers []map[string]any
	if jerr := json.Unmarshal([]byte(out), &offers); jerr != nil {
		t.Fatalf("awaiting did not emit JSON: %v\n%s", jerr, out)
	}
	if len(offers) != 0 {
		t.Fatalf("awaiting found %d offers in an empty repository", len(offers))
	}

	// Taking something that was never offered is refused, and says so rather
	// than inventing a claim.
	out, err = run("connector", "take", "--cell", "mini-a", "--key", strings.Repeat("ab", 32), "--connector", "fab-a")
	if err == nil {
		t.Fatalf("taking an offer that does not exist should be refused, got: %s", out)
	}

	// And the flags that must not be defaultable are enforced.
	if out, err := run("connector", "report", "--cell", "mini-a", "--key", strings.Repeat("ab", 32), "--connector", "fab-a"); err == nil {
		t.Fatalf("reporting without --happened should be refused, got: %s", out)
	}
}
