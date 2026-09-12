package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The robe projection, driven through the binary against a real varvig.
//
// It is an integration test rather than a unit one because the property being
// checked spans three things the unit tests each see only one of: that `robes`
// survives the canonical encoding, that it reaches a real repository as a ref
// another cell would read, and that what comes back out is the projection. A
// field that serialized but never replicated would pass every test in cell/ and
// robe/ and be useless.
func TestIntegrationRobesRoundTripThroughARealRepository(t *testing.T) {
	varvig, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()

	factory := filepath.Join(dir, "varvig-factory")
	if out, berr := exec.Command("go", "build", "-o", factory, ".").CombinedOutput(); berr != nil {
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

	if out, err := run("init", "--profile", "micro", "--cell-id", "micro-b"); err != nil {
		t.Fatalf("factory init: %v: %s", err, out)
	}

	// Written the way an operator would: robes are configuration, because a
	// cell wears what it says it wears and no cell can give another one a robe.
	cfgPath := filepath.Join(repo, "factory.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	// Deliberately out of order and duplicated: what lands in the repository
	// has to be canonical, or two cells configured identically would publish
	// different bytes and hash differently.
	cfg["robes"] = []string{"monitor", "ambassador", "monitor"}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	published, err := run("capabilities")
	if err != nil {
		t.Fatalf("capabilities: %v: %s", err, published)
	}
	if !strings.Contains(published, `"robes":["ambassador","monitor"]`) {
		t.Fatalf("the published object is not canonical in robes:\n%s", published)
	}
	if !strings.Contains(published, "published") {
		t.Fatalf("capabilities were never written to the repository:\n%s", published)
	}

	// And read back through the projection, which is what every other cell in
	// the factory would do — no registry, no assignment protocol, just refs.
	seen, err := run("robes")
	if err != nil {
		t.Fatalf("robes: %v: %s", err, seen)
	}
	for _, want := range []string{"micro-b", "ambassador", "monitor"} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the projection is missing %q:\n%s", want, seen)
		}
	}
	// An unworn robe is reported as a fact about the factory, not as a fault: a
	// factory with no procurement robe simply does not grow.
	if !strings.Contains(seen, "procurement") {
		t.Fatalf("the projection does not say which robes nobody wears:\n%s", seen)
	}

	// The scope is the same bundle whatever is worn, and it is what the output
	// above points at — so a reader who takes the projection for a permissions
	// list has somewhere to be corrected.
	scope, err := run("robes", "--scope")
	if err != nil {
		t.Fatalf("robes --scope: %v: %s", err, scope)
	}
	if !strings.Contains(scope, "refs/factory/cells/micro-b/") {
		t.Fatalf("the enrolment scope does not name this cell's own namespace:\n%s", scope)
	}
	if strings.Contains(scope, "ambassador") || strings.Contains(scope, "monitor") {
		t.Fatalf("a robe appears in the enrolment scope; robes carry no authority:\n%s", scope)
	}
}
