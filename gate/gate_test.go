package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

func moduleFor(exit int) func([]byte) varvigcli.HookResult {
	return func([]byte) varvigcli.HookResult { return varvigcli.HookResult{ExitCode: exit} }
}

func TestExitCodesMapToVerdicts(t *testing.T) {
	cases := []struct {
		exit int
		want Verdict
	}{
		{0, Promote},
		{1, Refuse},
		{2, Defer},
		// Everything unrecognised defers: a module built against a future
		// version of this contract, or one that crashed with its own exit code,
		// must reach a human rather than be read as consent.
		{3, Defer},
		{42, Defer},
		{-1, Defer},
	}
	for _, tc := range cases {
		v := varvigcli.NewFake("a")
		v.BindHook(Event, moduleFor(tc.exit))
		res, err := (Module{V: v}).Evaluate(context.Background(), Input{})
		if err != nil {
			t.Fatalf("exit %d: %v", tc.exit, err)
		}
		if res.Verdict != tc.want {
			t.Fatalf("exit %d -> %v, want %v", tc.exit, res.Verdict, tc.want)
		}
	}
}

func TestNoModuleIsNotAnApprovingGate(t *testing.T) {
	// §6.2: the promotion rule must be a reviewed, versioned object. An
	// unconfigured gate is the absence of one, and returning a Defer verdict here
	// would let a caller treat "no rule" and "the rule says ask a human" as the
	// same thing — which they are for gated promotion and are emphatically not
	// for autonomous.
	v := varvigcli.NewFake("a")
	res, err := (Module{V: v}).Evaluate(context.Background(), Input{})
	if !errors.Is(err, ErrNoModule) {
		t.Fatalf("err = %v, want ErrNoModule", err)
	}
	if res.Verdict != Defer {
		t.Fatalf("verdict = %v, want the safe answer", res.Verdict)
	}
}

func TestTheMostConservativeVerdictWins(t *testing.T) {
	// Constraints stack, and any one refusing is decisive — the same rule varvig
	// applies to its own composed policies. A second module can only tighten.
	cases := []struct {
		exits []int
		want  Verdict
	}{
		{[]int{0, 0}, Promote},
		{[]int{0, 2}, Defer},
		{[]int{2, 0}, Defer},
		{[]int{0, 1}, Refuse},
		{[]int{2, 1}, Refuse},
		{[]int{1, 0, 2}, Refuse},
	}
	for _, tc := range cases {
		v := varvigcli.NewFake("a")
		for _, e := range tc.exits {
			v.BindHook(Event, moduleFor(e))
		}
		res, err := (Module{V: v}).Evaluate(context.Background(), Input{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Verdict != tc.want {
			t.Fatalf("exits %v -> %v, want %v", tc.exits, res.Verdict, tc.want)
		}
	}
}

func TestTheModulesExplanationTravelsWithTheVerdict(t *testing.T) {
	v := varvigcli.NewFake("a")
	v.BindHook(Event, func([]byte) varvigcli.HookResult {
		return varvigcli.HookResult{ExitCode: 2, Stderr: "cross-class evidence, deferring"}
	})
	res, err := (Module{V: v}).Evaluate(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason() != "cross-class evidence, deferring" {
		t.Fatalf("reason = %q", res.Reason())
	}
	// With no explanation, the reason still names the exit code rather than being
	// empty: a refusal an operator cannot read is a refusal they will override.
	bare := Result{ExitCode: 7}
	if bare.Reason() == "" {
		t.Fatal("a bare result has no reason at all")
	}
}

func TestInputIsCanonicalAndCarriesTheDecisionContext(t *testing.T) {
	var got []byte
	v := varvigcli.NewFake("a")
	v.BindHook(Event, func(in []byte) varvigcli.HookResult {
		got = append([]byte(nil), in...)
		return varvigcli.HookResult{ExitCode: 0}
	})

	in := Input{
		Attempt:  cell.Attempt{CellID: "mini-a", Task: "t1", N: 1, Change: "abc"},
		Evidence: []cell.Evidence{{Attempt: "abc", CellID: "micro-b", Checks: []cell.Check{{Name: "unit", Status: cell.StatusPass}}}},
		Environments: map[string]cell.Environment{
			"sha256:e": {Platform: "linux/amd64"},
		},
		Scope:       "src/",
		Mode:        "autonomous",
		Independent: true,
		Agreement:   AgreementView{Observations: 40, Agreements: 34, Rate: 0.85},
	}
	if _, err := (Module{V: v}).Evaluate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	// The module receives a computed context on stdin, canonically encoded — the
	// same shape varvig's own policy modules take, so a module cannot make the
	// host do anything.
	for _, want := range []string{`"mode":"autonomous"`, `"independent":true`, `"scope":"src/"`, `"cell_id":"micro-b"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("input is missing %s:\n%s", want, got)
		}
	}
	// Canonical means newline-free, which is what makes it storable as a note and
	// recoverable from porcelain output.
	if strings.Contains(string(got), "\n") {
		t.Fatalf("gate input is not canonical (contains a newline):\n%s", got)
	}
}

func TestVerdictStrings(t *testing.T) {
	if Promote.String() != "promote" || Refuse.String() != "refuse" || Defer.String() != "defer-to-human" {
		t.Fatal("verdict names are not stable; they appear in logs and refusals")
	}
}
