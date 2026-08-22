package promote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwitchDefaultsToGated(t *testing.T) {
	// Gated is the default, everywhere, always: a cell with no configuration
	// promotes nothing on its own (§6).
	s, err := NewSwitch(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	mode, err := s.Mode()
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeGated {
		t.Fatalf("mode = %q, want %q", mode, ModeGated)
	}
	paths, err := s.AutonomousPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("a fresh switch has enabled paths: %v", paths)
	}
}

func TestSwitchIsReReadOnEveryDecision(t *testing.T) {
	// This is the kill switch (§6.5): a mode flip must take effect immediately
	// across the cell without a restart. Two Switch objects over one file stand
	// in for the CLI and the running loop.
	path := filepath.Join(t.TempDir(), "promotion.json")
	running, err := NewSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := NewSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.SetMode(ModeAutonomous); err != nil {
		t.Fatal(err)
	}
	mode, err := running.Mode()
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeAutonomous {
		t.Fatalf("the running switch did not see the change: %q", mode)
	}
	if err := cli.SetMode(ModeGated); err != nil {
		t.Fatal(err)
	}
	if mode, _ = running.Mode(); mode != ModeGated {
		t.Fatalf("the running switch did not see the flip back to gated: %q", mode)
	}
}

func TestEnablingAPathIsIndependentOfTheMode(t *testing.T) {
	// Enabling a path and switching the cell to autonomous are two deliberate
	// acts, so flipping back to gated does not silently discard which paths an
	// operator reviewed.
	path := filepath.Join(t.TempDir(), "promotion.json")
	s, err := NewSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableAutonomous("src/generated/"); err != nil {
		t.Fatal(err)
	}
	if mode, _ := s.Mode(); mode != ModeGated {
		t.Fatalf("enabling a path changed the mode to %q", mode)
	}
	if err := s.SetMode(ModeAutonomous); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ModeGated); err != nil {
		t.Fatal(err)
	}
	paths, err := s.AutonomousPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "src/generated/" {
		t.Fatalf("the enabled path did not survive a mode round trip: %v", paths)
	}
}

func TestGlobalAutonomousScopeIsRefused(t *testing.T) {
	// §6.3 condition 4: autonomous mode is per-path, never global. A factory key
	// with promote at / is the same risk as an unattended root credential.
	s, err := NewSwitch("")
	if err != nil {
		t.Fatal(err)
	}
	for _, global := range []string{"/", "", ".", "*", "  /  "} {
		if err := s.EnableAutonomous(global); err == nil {
			t.Fatalf("a global autonomous scope %q was accepted", global)
		} else if !strings.Contains(err.Error(), "per-path") {
			t.Fatalf("the refusal does not explain why: %v", err)
		}
	}
}

func TestEnabledForMatchesWholeSegmentsAndPrefersTheMostSpecific(t *testing.T) {
	s, err := NewSwitch("")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"src/", "src/generated/"} {
		if err := s.EnableAutonomous(p); err != nil {
			t.Fatal(err)
		}
	}

	// The most specific enabled scope is the one reported, so a decision can say
	// which entry permitted it.
	enabling, ok, err := s.EnabledFor("src/generated/api.go")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || enabling != "src/generated/" {
		t.Fatalf("enabling scope = %q, ok = %t; want src/generated/", enabling, ok)
	}

	// A raw prefix match would make "src/" cover "srcgen/". Whole segments only.
	if _, ok, _ := s.EnabledFor("srcgen/thing.go"); ok {
		t.Fatal("a sibling directory matched by string prefix")
	}
	if _, ok, _ := s.EnabledFor("docs/readme.md"); ok {
		t.Fatal("an unenabled path was reported as enabled")
	}
	// An empty target is not "everything".
	if _, ok, _ := s.EnabledFor(""); ok {
		t.Fatal("an empty path was reported as enabled")
	}
}

func TestDisableRemovesOnlyTheNamedScope(t *testing.T) {
	s, err := NewSwitch("")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"src/", "docs/"} {
		if err := s.EnableAutonomous(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DisableAutonomous("src"); err != nil { // trailing slash optional
		t.Fatal(err)
	}
	paths, _ := s.AutonomousPaths()
	if len(paths) != 1 || paths[0] != "docs/" {
		t.Fatalf("paths after disable = %v, want [docs/]", paths)
	}
}

func TestACorruptSwitchFailsLoudly(t *testing.T) {
	// An unreadable kill switch must not read as autonomous — and must not
	// silently read as gated either, because that would hide a corrupted switch
	// from the operator who is relying on it.
	path := filepath.Join(t.TempDir(), "promotion.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSwitch(path); err == nil {
		t.Fatal("a corrupt switch state was accepted")
	}
}

func TestParseMode(t *testing.T) {
	for _, ok := range []string{"gated", "autonomous"} {
		if _, err := ParseMode(ok); err != nil {
			t.Fatalf("ParseMode(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "GATED", "auto", "yes"} {
		if _, err := ParseMode(bad); err == nil {
			t.Fatalf("ParseMode(%q) was accepted", bad)
		}
	}
}

func TestOutcomeSummaryReportsEveryUnmetCondition(t *testing.T) {
	// An operator fixing one blocker wants to know about the others now, not
	// after the next run.
	out := Outcome{
		Mode: ModeAutonomous,
		Failed: []Failure{
			{CondGateVerdict, "policy module returned defer-to-human"},
			{CondIndependentEvidence, "no passing evidence from another cell"},
			{CondAgreementMetric, "no metric for this scope"},
		},
	}
	sortFailures(out.Failed)
	// Declared order, so two runs with the same unmet conditions read the same.
	if out.Failed[0].Condition != CondIndependentEvidence {
		t.Fatalf("failures are not in declared order: %+v", out.Failed)
	}
	s := out.Summary()
	for _, want := range []string{"independent-evidence", "agreement-metric", "gate-verdict", "deferred-to-human"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary is missing %q:\n%s", want, s)
		}
	}
	if !out.DeferredToHuman() {
		t.Fatal("an unpromoted outcome did not report as deferred to a human")
	}
}
