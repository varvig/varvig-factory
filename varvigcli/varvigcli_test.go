package varvigcli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The parser tests below pin the exact porcelain formats this adapter depends
// on. They are cheap and they are the early warning: if varvig's human output
// changes, this fails instead of a cell silently seeing no tickets.

func TestParseNotesRecoversCanonicalPayloads(t *testing.T) {
	// The format is a header line then the payload indented by two spaces.
	// Factory payloads are canonical JSON and therefore newline-free, which is
	// what makes this exact rather than best-effort.
	out := strings.Join([]string{
		`[factory/evidence] a1b2c3d4e5f6 by micro-b at 2026-08-22T10:00:00Z`,
		`  {"attempt":"sha256:aa","cell_id":"micro-b"}`,
		`[factory/evidence] 998877665544 by mini-a at 2026-08-22T10:05:00Z`,
		`  {"attempt":"sha256:aa","cell_id":"mini-a"}`,
		``,
	}, "\n")
	notes := parseNotes(out)
	if len(notes) != 2 {
		t.Fatalf("parsed %d notes, want 2: %+v", len(notes), notes)
	}
	if notes[0].Namespace != "factory/evidence" {
		t.Fatalf("namespace = %q", notes[0].Namespace)
	}
	if notes[0].Author != "micro-b" || notes[1].Author != "mini-a" {
		t.Fatalf("authors = %q, %q", notes[0].Author, notes[1].Author)
	}
	if !strings.Contains(string(notes[0].Payload), `"cell_id":"micro-b"`) {
		t.Fatalf("payload = %s", notes[0].Payload)
	}
	// A payload starting with '{' must not be mistaken for a header, and a
	// header with no payload line must not swallow the next header.
	if strings.Contains(string(notes[0].Payload), "998877") {
		t.Fatal("payload absorbed the following header")
	}
}

func TestParseNotesToleratesAHeaderWithNoPayload(t *testing.T) {
	out := "[factory/evidence] a1 by x at t\n[factory/evidence] a2 by y at t\n  {}\n"
	notes := parseNotes(out)
	if len(notes) != 2 {
		t.Fatalf("parsed %d notes, want 2", len(notes))
	}
	if len(notes[0].Payload) != 0 {
		t.Fatalf("first note took the second's payload: %s", notes[0].Payload)
	}
}

func TestParseHookResultsCarriesEveryExitCode(t *testing.T) {
	out := strings.Join([]string{
		"hook 0: exit=0",
		"  stdout: promote",
		"hook 1: exit=2",
		"  stderr: cross-class, deferring",
		"",
	}, "\n")
	res := parseHookResults(out)
	if len(res) != 2 {
		t.Fatalf("parsed %d results, want 2: %+v", len(res), res)
	}
	if res[0].ExitCode != 0 || res[1].ExitCode != 2 {
		t.Fatalf("exit codes = %d, %d", res[0].ExitCode, res[1].ExitCode)
	}
	if res[0].Stdout != "promote" || res[1].Stderr != "cross-class, deferring" {
		t.Fatalf("streams = %+v", res)
	}
}

func TestParseHookResultsDistinguishesNoGateFromAnAllowingGate(t *testing.T) {
	// The promotion gate must never read "no gate configured" as "the gate said
	// yes" (FACTORY.md §6.2), so the no-hooks line has to yield zero results.
	if res := parseHookResults(`no hooks bound to "factory-promote"` + "\n"); len(res) != 0 {
		t.Fatalf("a missing gate parsed as %d results", len(res))
	}
}

func TestParseTrustReadsScopeAndRights(t *testing.T) {
	out := strings.Join([]string{
		"# fingerprint       name    scope        rights",
		"SHA256:aXk9Lm4Qr   jan     /            promote",
		"SHA256:fT8kLm      factory-prod  src/generated/   promote",
		"SHA256:cW3nEf8Zx   ci-01   /            propose",
		"",
	}, "\n")
	entries := parseTrust(out)
	if len(entries) != 3 {
		t.Fatalf("parsed %d entries, want 3: %+v", len(entries), entries)
	}
	factory := entries[1]
	if factory.Name != "factory-prod" || factory.Scope != "src/generated/" {
		t.Fatalf("entry = %+v", factory)
	}
	if !factory.Can("promote", "src/generated/thing.go") {
		t.Fatal("the factory key cannot promote inside its own scope")
	}
	if factory.Can("promote", "src/billing/rates.go") {
		t.Fatal("the factory key can promote outside its scope")
	}
	if entries[2].Can("promote", "anything") {
		t.Fatal("a propose-only key was granted promote")
	}
}

func TestScopeCoversMatchesWholeSegments(t *testing.T) {
	// A raw string prefix match would make "src/generated/" cover
	// "src/generated-secrets/". That is a privilege escalation by typo, so the
	// comparison is on whole path segments.
	if ScopeCovers("src/generated/", "src/generated-secrets/keys.go") {
		t.Fatal("scope matched a sibling directory by string prefix")
	}
	if !ScopeCovers("src/generated/", "src/generated/api.go") {
		t.Fatal("scope did not cover a path inside it")
	}
	if !ScopeCovers("/", "anything/at/all") {
		t.Fatal("root scope did not cover everything")
	}
	if !ScopeCovers("src/generated", "src/generated/api.go") {
		t.Fatal("a scope without a trailing slash did not cover its contents")
	}
}

func TestParseGCReportsExternalArtifacts(t *testing.T) {
	out := "swept 4 objects\nexternal-unreachable:2\n  sha256:aa\tapplication/vnd.oci.image.manifest.v1+json\toci://r/a oci://r/b\n  sha256:bb\tapplication/octet-stream\t\n"
	rep := parseGC(out)
	if len(rep.ExternalUnreachable) != 2 {
		t.Fatalf("parsed %d artifacts, want 2: %+v", len(rep.ExternalUnreachable), rep.ExternalUnreachable)
	}
	if rep.ExternalUnreachable[0].ContentHash != "sha256:aa" {
		t.Fatalf("hash = %q", rep.ExternalUnreachable[0].ContentHash)
	}
	if len(rep.ExternalUnreachable[0].Locators) != 2 {
		t.Fatalf("locators = %v", rep.ExternalUnreachable[0].Locators)
	}
}

func TestClassifyMapsTheThreeDistinctionsFactoryBranchesOn(t *testing.T) {
	cases := []struct {
		msg  string
		want error
	}{
		{"ref refs/claims/a/b does not exist", ErrNoRef},
		{"compare-and-swap failed: ref moved", ErrCAS},
		{"dial tcp 10.0.0.1:9418: connection refused", ErrUnreachable},
	}
	for _, tc := range cases {
		err := classify(errors.New("wrapped: "+tc.msg), tc.msg)
		if !errors.Is(err, tc.want) {
			t.Fatalf("classify(%q) = %v, want %v", tc.msg, err, tc.want)
		}
	}
	// Anything else stays generic rather than being forced into a category:
	// guessing wrong here would make a real failure look like a partition and a
	// cell would keep working through it.
	generic := classify(errors.New("boom"), "boom")
	for _, sentinel := range []error{ErrNoRef, ErrCAS, ErrUnreachable} {
		if errors.Is(generic, sentinel) {
			t.Fatalf("an unrecognised error was classified as %v", sentinel)
		}
	}
}

func TestScopeDeclared(t *testing.T) {
	// A ticket with no scope is unschedulable by construction (TICKETS.md §3.1).
	if (Scope{}).Declared() {
		t.Fatal("an empty scope reported as declared")
	}
	if !(Scope{Writes: []string{"src/"}}).Declared() {
		t.Fatal("a write-only scope reported as undeclared")
	}
}

// --- Fake semantics ---

func TestFakeUpdateRefEnforcesCAS(t *testing.T) {
	f := NewFake("a")
	if err := f.UpdateRef("refs/x", "v1", ""); err != nil {
		t.Fatal(err)
	}
	// Creating twice must fail: an attempt ref is created once and never moved
	// (CELL.md §2), and the create-only CAS is what enforces it.
	if err := f.UpdateRef("refs/x", "v2", ""); !errors.Is(err, ErrCAS) {
		t.Fatalf("err = %v, want ErrCAS", err)
	}
	if err := f.UpdateRef("refs/x", "v2", "wrong"); !errors.Is(err, ErrCAS) {
		t.Fatalf("err = %v, want ErrCAS", err)
	}
	if err := f.UpdateRef("refs/x", "v2", "v1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.ResolveRef("refs/x"); got != "v2" {
		t.Fatalf("ref = %q", got)
	}
	if _, err := f.ResolveRef("refs/absent"); !errors.Is(err, ErrNoRef) {
		t.Fatalf("err = %v, want ErrNoRef", err)
	}
}

func TestFakePartitionIsUnreachableNotAFailure(t *testing.T) {
	// A disconnected cell must be able to tell "no upstream" from "upstream
	// refused": collapsing them is how local-first operation quietly stops
	// being real (FACTORY.md §5.2).
	up := NewFake("upstream")
	cellA := NewFake("a")
	cellA.Upstream = up
	cellA.Partitioned = true
	if err := cellA.Fetch("upstream", "main"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if err := cellA.Push("upstream", "main"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	// No upstream configured at all is also unreachable, not a crash.
	if err := NewFake("lonely").Fetch("upstream", "main"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestFakeSyncCopiesObjectsAndLeavesDivergentRefsAlone(t *testing.T) {
	up := NewFake("upstream")
	a := NewFake("a")
	a.Upstream = up

	if _, err := a.PutBlob([]byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateRef("refs/attempts/a/t1/1", "sha256:aaa", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Push("upstream", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := up.ResolveRef("refs/attempts/a/t1/1"); err != nil || got != "sha256:aaa" {
		t.Fatalf("upstream ref = %q, %v", got, err)
	}

	// A ref that diverged is refused rather than clobbered.
	if err := up.UpdateRef("refs/heads/main", "sha256:upstream", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateRef("refs/heads/main", "sha256:local", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Push("upstream", ""); !errors.Is(err, ErrCAS) {
		t.Fatalf("err = %v, want ErrCAS", err)
	}
	if got, _ := up.ResolveRef("refs/heads/main"); got != "sha256:upstream" {
		t.Fatalf("upstream ref was clobbered: %q", got)
	}
}

func TestFakeRejectsAMultilineNotePayload(t *testing.T) {
	// The Exec client recovers a payload from the line after the header, so a
	// multiline payload would be truncated there. The Fake refuses it so a test
	// cannot pass against behaviour the real client would break on.
	f := NewFake("a")
	if err := f.AddNote("sha256:aa", "factory/evidence", []byte("{\n  \"a\": 1\n}")); err == nil {
		t.Fatal("a multiline note payload was accepted")
	}
	if err := f.AddNote("sha256:aa", "factory/evidence", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
}

func TestFakePromotesTheHighestScoringCandidate(t *testing.T) {
	// The selection rule must match varvig's, because the agreement metric is
	// about that rule (FACTORY.md §6.4).
	f := NewFake("a")
	for _, c := range []struct {
		change string
		score  float64
	}{{"sha256:low", 0.2}, {"sha256:high", 0.9}, {"sha256:mid", 0.5}} {
		if err := f.SpecAdd("t1", c.change); err != nil {
			t.Fatal(err)
		}
		if err := f.SpecScore("t1", c.change, c.score); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.SpecPromote("t1", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:high" {
		t.Fatalf("promoted %q, want sha256:high", got)
	}
	// Adding the same change twice is one candidate: the pool is
	// content-addressed.
	if err := f.SpecAdd("t1", "sha256:high"); err != nil {
		t.Fatal(err)
	}
	props, _ := f.Proposals("t1")
	if len(props) != 3 {
		t.Fatalf("pool has %d candidates, want 3", len(props))
	}
}

func TestFakeHookRunReturnsEachModulesVerdict(t *testing.T) {
	f := NewFake("a")
	f.BindHook("factory-promote", func(in []byte) HookResult {
		if strings.Contains(string(in), "cross") {
			return HookResult{ExitCode: 2}
		}
		return HookResult{ExitCode: 0}
	})
	res, err := f.HookRun(context.Background(), "factory-promote", []byte(`{"class":"cross"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ExitCode != 2 {
		t.Fatalf("results = %+v", res)
	}
	// An event with no module bound yields no results, not an allow.
	none, err := f.HookRun(context.Background(), "unbound", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("an unbound event produced %d results", len(none))
	}
}

var _ Varvig = Exec{}
