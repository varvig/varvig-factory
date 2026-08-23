package claim

import (
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

var now = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func attemptingCell() cell.Capabilities {
	return cell.Capabilities{
		CellID:    "mini-a",
		Inference: cell.Inference{Tier: cell.TierLarge, Models: []cell.Model{{ID: "m"}}},
		Build:     []string{"go", "flutter"},
		Test:      []string{"unit", "integration"},
		Roles:     []cell.Role{cell.RoleAttempt, cell.RoleBuild, cell.RoleVerify},
	}
}

func microCell() cell.Capabilities {
	return cell.Capabilities{
		CellID:    "micro-b",
		Inference: cell.Inference{Tier: cell.TierNone},
		Build:     []string{"go"},
		Test:      []string{"unit"},
		Roles:     []cell.Role{cell.RoleBuild, cell.RoleVerify},
	}
}

func schedulableTicket() Ticket {
	return Ticket{
		ID:     "a1b2c3",
		Spec:   "Add a thing.",
		Scope:  varvigcli.Scope{Reads: []string{"src"}, Writes: []string{"src"}},
		Status: "approved",
	}
}

func baseInputs() Inputs {
	return Inputs{
		Capabilities:       attemptingCell(),
		Ticket:             schedulableTicket(),
		BudgetOK:           true,
		MaxAttemptsPerCell: 3,
		Now:                now,
	}
}

func TestClaimsWhenEverythingHolds(t *testing.T) {
	v := Evaluate(baseInputs())
	if !v.Claim {
		t.Fatalf("did not claim: %s", v.Reason)
	}
	if v.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", v.Attempt)
	}
	if v.Reason == "" {
		t.Fatal("a claim with no stated reason is a claim nobody can debug")
	}
}

func TestMicroDoesNotAttemptByDefault(t *testing.T) {
	// FACTORY.md §3.1: Micro ships with roles verify and build. Attempting is
	// opt-in, and this is the normal, intended state — so the skip has to be
	// reported as such rather than as a problem.
	in := baseInputs()
	in.Capabilities = microCell()
	v := Evaluate(in)
	if v.Claim {
		t.Fatal("a verify/build cell claimed a ticket to attempt")
	}
	if v.Skip != SkipNotAttempting {
		t.Fatalf("skip = %q, want %q", v.Skip, SkipNotAttempting)
	}
	if !strings.Contains(v.Reason, "opt-in") {
		t.Fatalf("the reason does not say attempting is opt-in: %s", v.Reason)
	}
}

func TestUnschedulableTicketIsSkippedNotScoped(t *testing.T) {
	// The cell must not derive a scope for a ticket that has none: that would be
	// a second scheduler (§1). It skips and says why.
	in := baseInputs()
	in.Ticket.Scope = varvigcli.Scope{}
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipUnschedulable {
		t.Fatalf("verdict = %+v, want an unschedulable skip", v)
	}
	if !strings.Contains(v.Reason, "serialize") {
		t.Fatalf("the reason does not say varvig cannot serialize it: %s", v.Reason)
	}
}

func TestVetoedAndBlockedTicketsAreSkipped(t *testing.T) {
	vetoed := baseInputs()
	vetoed.Ticket.Status = "vetoed"
	if v := Evaluate(vetoed); v.Claim || v.Skip != SkipVetoed {
		t.Fatalf("verdict = %+v, want a vetoed skip", v)
	}
	// A veto makes every descendant unpromotable, so attempting would burn budget
	// on work that cannot land.
	if !strings.Contains(Evaluate(vetoed).Reason, "unpromotable") {
		t.Fatalf("the reason does not explain the veto: %s", Evaluate(vetoed).Reason)
	}

	blocked := baseInputs()
	blocked.Ticket.Blockers = []string{"x", "y"}
	v := Evaluate(blocked)
	if v.Claim || v.Skip != SkipBlocked {
		t.Fatalf("verdict = %+v, want a blocked skip", v)
	}
	// Blocking is varvig's derivation, read not recomputed.
	if !strings.Contains(v.Reason, "varvig") {
		t.Fatalf("the reason does not attribute the derivation to varvig: %s", v.Reason)
	}
}

func TestCapabilityMismatchNamesTheMissingCapability(t *testing.T) {
	in := baseInputs()
	in.Ticket.Spec = "Build the app.\nfactory-requires: build=android test=large-memory\n"
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipCapability {
		t.Fatalf("verdict = %+v, want a capability skip", v)
	}
	for _, want := range []string{"build:android", "test:large-memory"} {
		if !strings.Contains(v.Reason, want) {
			t.Fatalf("the reason does not name %s: %s", want, v.Reason)
		}
	}
}

func TestBudgetHaltStopsClaiming(t *testing.T) {
	in := baseInputs()
	in.BudgetOK = false
	in.BudgetReason = "daily inference cap reached"
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipBudget {
		t.Fatalf("verdict = %+v, want a budget skip", v)
	}
	if !strings.Contains(v.Reason, "halted") || !strings.Contains(v.Reason, "cap") {
		t.Fatalf("the reason does not report the halt: %s", v.Reason)
	}
}

func TestRepeatAttemptsAreBoundedAndCounted(t *testing.T) {
	in := baseInputs()
	in.OwnAttempts = 1
	v := Evaluate(in)
	if !v.Claim {
		t.Fatalf("a second attempt within the limit was refused: %s", v.Reason)
	}
	if v.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", v.Attempt)
	}

	in.OwnAttempts = 3
	if v := Evaluate(in); v.Claim || v.Skip != SkipAlreadyAttempted {
		t.Fatalf("verdict = %+v, want an already-attempted skip", v)
	}

	// With no limit configured, one attempt per cell per task.
	in.MaxAttemptsPerCell = 0
	in.OwnAttempts = 1
	if v := Evaluate(in); v.Claim {
		t.Fatal("with no limit configured a cell attempted the same task twice")
	}
}

func TestForeignClaimYieldIsAdvisoryAndOptional(t *testing.T) {
	in := baseInputs()
	in.YieldToFreshClaims = true
	in.ForeignClaims = []cell.Claim{{
		CellID: "mini-b", Task: in.Ticket.ID, NotAfter: now.Add(20 * time.Minute).Unix(),
	}}
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipForeignClaim {
		t.Fatalf("verdict = %+v, want a foreign-claim skip", v)
	}
	// The reason must say it is advisory, so nobody reads this as mutual
	// exclusion (§5.1: duplicate attempts are normal and are the point).
	if !strings.Contains(v.Reason, "advisory") {
		t.Fatalf("the reason does not say the yield is advisory: %s", v.Reason)
	}

	// Yielding is a budget choice, not a correctness mechanism: turning it off
	// makes the cell attempt anyway, deliberately.
	in.YieldToFreshClaims = false
	if v := Evaluate(in); !v.Claim {
		t.Fatalf("a federation running deliberate duplicates was blocked: %s", v.Reason)
	}
}

func TestStaleForeignClaimIsNotAReasonToSkip(t *testing.T) {
	// A stale claim stops being a reason for another cell to skip, and the cell
	// that wrote it has no further standing from it (CELL.md §5).
	in := baseInputs()
	in.YieldToFreshClaims = true
	in.ForeignClaims = []cell.Claim{{
		CellID: "mini-b", Task: in.Ticket.ID, NotAfter: now.Add(-time.Minute).Unix(),
	}}
	if v := Evaluate(in); !v.Claim {
		t.Fatalf("a stale foreign claim blocked a claim: %s", v.Reason)
	}
}

func TestOfflineClaimIsAllowedAndSaysSo(t *testing.T) {
	// §5.1: a cell may claim and attempt while disconnected. This is required,
	// not merely tolerated.
	in := baseInputs()
	in.Offline = true
	v := Evaluate(in)
	if !v.Claim {
		t.Fatalf("a disconnected cell was not allowed to claim: %s", v.Reason)
	}
	if !strings.Contains(v.Reason, "offline") {
		t.Fatalf("the reason does not record that the claim was made offline: %s", v.Reason)
	}
}

func TestParseRequirements(t *testing.T) {
	spec := `Add an endpoint.

Some prose about the endpoint.

factory-requires: build=go,flutter test=unit, integration attempts=5
`
	r := ParseRequirements(spec)
	if strings.Join(r.Build, ",") != "flutter,go" {
		t.Fatalf("build = %v, want sorted [flutter go]", r.Build)
	}
	if strings.Join(r.Test, ",") != "unit" {
		// "integration" is separated by a space after the comma, so it is a
		// separate field and not part of the test list. Documenting the parse
		// rather than silently accepting either: whitespace inside a value would
		// make the directive's grammar depend on how a human typed it.
		t.Fatalf("test = %v, want [unit]", r.Test)
	}
	if r.Attempts != 5 {
		t.Fatalf("attempts = %d, want 5", r.Attempts)
	}
}

func TestParseRequirementsIsForgiving(t *testing.T) {
	// A ticket with no directive requires nothing: most tickets are ordinary code
	// changes and demanding an annotation on each would make the mechanism
	// something people work around.
	if r := ParseRequirements("Just do the thing."); len(r.Build) != 0 || len(r.Test) != 0 || r.Attempts != 0 {
		t.Fatalf("a plain spec produced requirements: %+v", r)
	}
	// An unknown key is ignored, not rejected: a ticket written for a newer
	// Factory must still be attemptable by an older cell.
	r := ParseRequirements("factory-requires: build=go quantum=yes attempts=2")
	if strings.Join(r.Build, ",") != "go" || r.Attempts != 2 {
		t.Fatalf("an unknown key broke the parse: %+v", r)
	}
	// The directive is case-insensitive on the prefix and the keys.
	if got := ParseRequirements("Factory-Requires: BUILD=go"); strings.Join(got.Build, ",") != "go" {
		t.Fatalf("case handling: %+v", got)
	}
	// A non-numeric attempts value is ignored rather than treated as zero, which
	// would silently mean "one attempt".
	if got := ParseRequirements("factory-requires: attempts=many"); got.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 (ignored)", got.Attempts)
	}
}

func TestTicketRequirementsReadsFromTheSpec(t *testing.T) {
	tk := Ticket{Spec: "factory-requires: test=fuzz"}
	if got := tk.Requirements().Test; strings.Join(got, ",") != "fuzz" {
		t.Fatalf("requirements = %v", got)
	}
}
