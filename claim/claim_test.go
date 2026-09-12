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
		Capabilities: attemptingCell(),
		Ticket:       schedulableTicket(),
		BudgetOK:     true,
		// An attempting cell whose runtime answered. Set explicitly because the
		// zero value means "unreachable" — the safe way round for a caller that
		// forgets, and the reason every vector below has to say it looked.
		ExecutorReachable:  true,
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

// --- effectful tickets (§6.7) ---

const boardIface = "1220a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

func effectSpec(extra string) string {
	return "Order the prototype run.\n" +
		"factory-requires: effect=pcb-fabrication@1 interface=" + boardIface + " " + extra + "\n" +
		`factory-effect: {"gerber":"rev-c","quantity":5}`
}

func effectInputs(spec string, grants ...EffectGrant) Inputs {
	return Inputs{
		// Deliberately a cell with no attempt role and no model: authority to
		// spend comes from a lease, and tying it to holding a model would be a
		// relationship that should not exist.
		Capabilities: cell.Capabilities{CellID: "micro-b", Roles: []cell.Role{cell.RoleBuild, cell.RoleVerify}},
		Ticket: Ticket{
			ID: "c3feed09", Spec: spec, Status: "approved",
			Scope: varvigcli.Scope{Reads: []string{"hardware"}, Writes: []string{"hardware"}},
		},
		EffectGrants: grants,
		Now:          time.Now(),
	}
}

var boardGrant = EffectGrant{Capability: "pcb-fabrication@1", Interface: boardIface}

func TestAnEffectfulTicketIsClaimedWithoutTheAttemptRole(t *testing.T) {
	v := Evaluate(effectInputs(effectSpec(""), boardGrant))
	if !v.Claim {
		t.Fatalf("a verify/build cell holding a lease was refused: %s", v.Reason)
	}
	if v.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 always", v.Attempt)
	}
}

func TestNoLeaseNoOrder(t *testing.T) {
	v := Evaluate(effectInputs(effectSpec("")))
	if v.Claim || v.Skip != SkipNoAuthority {
		t.Fatalf("a cell with no grant claimed an effectful ticket: %+v", v)
	}

	// A grant for the same alias but a different interface is not a grant for
	// this ticket — it is a grant for a different capability with the same name.
	other := EffectGrant{Capability: "pcb-fabrication@1", Interface: "1220ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}
	if v := Evaluate(effectInputs(effectSpec(""), other)); v.Claim {
		t.Fatal("an alias match with a different interface hash was treated as a grant")
	}
}

func TestAMalformedEffectNeverFallsThroughToAttempting(t *testing.T) {
	// The failure mode worth guarding: answering "order me a circuit board" by
	// writing code, because the directive did not parse.
	cases := map[string]string{
		"no interface hash": "Order it.\nfactory-requires: effect=pcb-fabrication@1\nfactory-effect: {}",
		"not a hash":        "Order it.\nfactory-requires: effect=pcb-fabrication@1 interface=nonsense\nfactory-effect: {}",
		"no payload":        "Order it.\nfactory-requires: effect=pcb-fabrication@1 interface=" + boardIface,
		"payload not json":  "Order it.\nfactory-requires: effect=pcb-fabrication@1 interface=" + boardIface + "\nfactory-effect: not json",
		"interface alone":   "Order it.\nfactory-requires: interface=" + boardIface + "\nfactory-effect: {}",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			in := effectInputs(spec, boardGrant)
			// The cell is fully equipped for ordinary work, so a fall-through to
			// the attempt path would be visible as a claim.
			in.Capabilities = cell.Capabilities{
				CellID: "mini-a", Roles: []cell.Role{cell.RoleAttempt},
				Inference: cell.Inference{Tier: cell.TierLarge, Models: []cell.Model{{ID: "m"}}},
			}
			in.BudgetOK = true
			v := Evaluate(in)
			if v.Claim {
				t.Fatalf("a malformed effect ticket was claimed: %s", v.Reason)
			}
			if v.Skip != SkipEffectMalformed {
				t.Fatalf("skip = %q, want %q — this must not read as an ordinary ticket", v.Skip, SkipEffectMalformed)
			}
		})
	}
}

func TestAnEffectfulTicketCannotAskForSpeculation(t *testing.T) {
	// §9.10 at the earliest point it can be caught. Three attempts at a board
	// order means three invoices, and the ticket's author needs to know.
	v := Evaluate(effectInputs(effectSpec("attempts=3"), boardGrant))
	if v.Claim {
		t.Fatal("a ticket asking for three attempts at an effectful capability was claimed")
	}
	if !strings.Contains(v.Reason, "3 orders") && !strings.Contains(v.Reason, "never speculated") {
		t.Fatalf("the refusal does not explain what three attempts would mean: %s", v.Reason)
	}
	// attempts=1 is fine — it is the only number that is.
	if v := Evaluate(effectInputs(effectSpec("attempts=1"), boardGrant)); !v.Claim {
		t.Fatalf("attempts=1 was refused: %s", v.Reason)
	}
}

func TestATicketDoesOneKindOfWork(t *testing.T) {
	// build/test and effect describe opposite kinds of work, and a ticket that
	// asks for both has not been thought through.
	spec := "Order it and test it.\nfactory-requires: effect=pcb-fabrication@1 interface=" + boardIface +
		" build=go\nfactory-effect: {}"
	v := Evaluate(effectInputs(spec, boardGrant))
	if v.Claim || v.Skip != SkipEffectMalformed {
		t.Fatalf("a ticket mixing build and effect requirements was claimed: %+v", v)
	}
}

func TestAnEffectfulTicketIsNotGatedByInferenceBudget(t *testing.T) {
	// An order is paid from its lease. Refusing to place one because the day's
	// model spend is exhausted would couple two unrelated budgets, and the
	// coupling would surface as an order that silently did not happen.
	in := effectInputs(effectSpec(""), boardGrant)
	in.BudgetOK, in.BudgetReason = false, "daily inference cap reached"
	if v := Evaluate(in); !v.Claim {
		t.Fatalf("an effectful ticket was blocked by the inference budget: %s", v.Reason)
	}
}

func TestAnEffectfulTicketIsClaimedOnlyOnce(t *testing.T) {
	in := effectInputs(effectSpec(""), boardGrant)
	in.OwnAttempts = 1
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipAlreadyAttempted {
		t.Fatalf("a cell re-claimed an effectful ticket it had already acted on: %+v", v)
	}
}

func TestYieldingMattersMoreForEffectfulTickets(t *testing.T) {
	in := effectInputs(effectSpec(""), boardGrant)
	in.YieldToFreshClaims = true
	in.ForeignClaims = []cell.Claim{{
		CellID: "mini-a", Task: "c3feed09", NotAfter: time.Now().Add(time.Hour).Unix(),
	}}
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipForeignClaim {
		t.Fatalf("a cell ignored a fresh foreign claim on an effectful ticket: %+v", v)
	}
	if !strings.Contains(v.Reason, "two orders") {
		t.Fatalf("the reason does not say why yielding matters here: %s", v.Reason)
	}
}

func TestAnEffectfulTicketStillNeedsToBeSchedulable(t *testing.T) {
	in := effectInputs(effectSpec(""), boardGrant)
	in.Ticket.Scope = varvigcli.Scope{}
	if v := Evaluate(in); v.Claim || v.Skip != SkipUnschedulable {
		t.Fatalf("an unscoped effectful ticket was claimed: %+v", v)
	}

	blocked := effectInputs(effectSpec(""), boardGrant)
	blocked.Ticket.Blockers = []string{"other"}
	if v := Evaluate(blocked); v.Claim || v.Skip != SkipBlocked {
		t.Fatalf("a blocked effectful ticket was claimed: %+v", v)
	}

	vetoed := effectInputs(effectSpec(""), boardGrant)
	vetoed.Ticket.Status = "vetoed"
	if v := Evaluate(vetoed); v.Claim || v.Skip != SkipVetoed {
		t.Fatalf("a vetoed effectful ticket was claimed: %+v", v)
	}
}

func TestOrdinaryTicketsAreUnaffected(t *testing.T) {
	// The regression that matters: adding an effectful branch must not change
	// what an ordinary ticket does.
	in := Inputs{
		Capabilities: cell.Capabilities{
			CellID: "mini-a", Roles: []cell.Role{cell.RoleAttempt}, Build: []string{"go"},
			Inference: cell.Inference{Tier: cell.TierLarge, Models: []cell.Model{{ID: "m"}}},
		},
		Ticket: Ticket{
			ID: "abc", Spec: "Fix the parser.\nfactory-requires: build=go", Status: "approved",
			Scope: varvigcli.Scope{Reads: []string{"src"}, Writes: []string{"src"}},
		},
		BudgetOK:          true,
		ExecutorReachable: true,
		Now:               time.Now(),
	}
	if v := Evaluate(in); !v.Claim {
		t.Fatalf("an ordinary ticket was refused: %s", v.Reason)
	}
}

// Test22_ExternallyOriginatedMarking is §9.22: a ticket an Ambassador created
// from an outside request carries that fact, and claim policy can act on it.
//
// The marking is an obligation of the robe, not a property claim policy can
// infer, and that asymmetry is the whole risk: an Ambassador that forgot to mark
// a ticket produces one that is indistinguishable from work the factory set
// itself. So what is tested here is not that external tickets are refused —
// whether to refuse them is an operator's call — but that the fact survives the
// round trip through the spec and reaches the decision unchanged.
func Test22_ExternallyOriginatedMarking(t *testing.T) {
	external := "Ship the thing.\n" + OriginDirective + " " + OriginExternal + "\n"

	req := ParseRequirements(external)
	if !req.ExternallyOriginated {
		t.Fatal("an Ambassador-marked ticket parsed as ordinary internal work")
	}
	// Its own directive, so a ticket carrying no requirement at all still
	// carries its origin. A ticket asking for nothing in particular is exactly
	// the external one that would otherwise look like housekeeping.
	if len(req.Build) != 0 || len(req.Test) != 0 || req.Effect != nil {
		t.Fatalf("the origin directive leaked into requirements: %+v", req)
	}

	// A cell that takes external work is unaffected — the marking is
	// information, not a veto.
	in := baseInputs()
	in.Ticket.Spec = external
	if v := Evaluate(in); !v.Claim {
		t.Fatalf("a cell that accepts external work declined one anyway: %s", v.Reason)
	}

	// A cell configured to decline it skips, and says so specifically enough
	// that an operator can tell "nobody is looking at external requests" from
	// "this ticket was rejected".
	in.DeclineExternallyOriginated = true
	v := Evaluate(in)
	if v.Claim || v.Skip != SkipExternallyOriginated {
		t.Fatalf("verdict = %+v, want an externally-originated skip", v)
	}
	if !strings.Contains(v.Reason, "external") {
		t.Fatalf("the reason does not name the origin: %s", v.Reason)
	}

	// Declining external work declines all of it, including the effectful case.
	// Reading the effectful branch first would leave the request that orders a
	// physical thing — the one that matters most — governed by a different rule
	// than the request that edits a file.
	eff := effectInputs(OriginDirective+" "+OriginExternal+"\n"+effectSpec(""), boardGrant)
	if v := Evaluate(eff); !v.Claim {
		t.Fatalf("the effectful external fixture is refused for some other reason: %+v", v)
	}
	eff.DeclineExternallyOriginated = true
	if v := Evaluate(eff); v.Claim || v.Skip != SkipExternallyOriginated {
		t.Fatalf("an effectful external ticket took a different branch: %+v", v)
	}

	// The directive's *value* is what marks it, not its presence. A ticket
	// saying where it came from and saying "not outside" is the case a check
	// that only looked for the line would get exactly backwards.
	if ParseRequirements("Do it.\n" + OriginDirective + " internal\n").ExternallyOriginated {
		t.Fatal("a ticket marked as internally originated parsed as external")
	}

	// And an unmarked ticket is never inferred to be external. The failure
	// nobody would notice is the opposite of this one, so it is worth pinning:
	// policy that guessed would decline ordinary work at random.
	plain := baseInputs()
	plain.DeclineExternallyOriginated = true
	if v := Evaluate(plain); !v.Claim {
		t.Fatalf("an unmarked ticket was treated as external: %s", v.Reason)
	}
}
