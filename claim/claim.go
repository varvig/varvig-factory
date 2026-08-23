// Package claim is claim policy (FACTORY.md §5.1): whether *this cell* should
// attempt *this ticket*.
//
// That is the entire question this package answers, and the boundary is the most
// important thing about it. Factory decides whether to attempt. varvig's
// scheduler decides how concurrent work inside a cell interleaves — read/write
// sets, serialization, regeneration on CAS failure (§1). Conflating the two
// means reimplementing affected-set logic badly, in the layer least equipped to
// do it, so nothing here looks at another ticket's write set. The only
// cross-ticket fact it uses is `varvig tickets blockers`, which is varvig's own
// derivation, read and not recomputed.
//
// Three properties the policy must preserve, each stated as a prohibition
// because each has an attractive-looking violation:
//
//   - Claims are advisory. They cannot be exclusive across a partition
//     (varvig-design.md §4b.3). Two cells may each compare-and-swap successfully
//     against their own view, and both are right.
//   - Duplicate attempts are normal and are the point — branching is search
//     (§1.5). Skipping a task another cell has freshly claimed is budget
//     politeness, configurable, and never a correctness mechanism.
//   - A cell may claim and attempt while disconnected. This is required, not
//     tolerated: local-first operation is the property that makes the cell model
//     worth having.
package claim

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Requirements are the capabilities a ticket needs, and how many attempts it
// wants.
//
// varvig has no field for this — capability tokens are a Factory concern and
// putting them in the core would teach varvig about toolchains. So they travel
// in the ticket's spec text as a single directive line, which a human writing a
// ticket can type and a cell can read:
//
//	factory-requires: build=go,flutter test=unit,large-memory attempts=5
//
// A ticket with no directive requires nothing, which is the right default: most
// tickets are ordinary code changes and demanding an annotation on each would
// make the mechanism something people work around.
type Requirements struct {
	Build    []string
	Test     []string
	Attempts int
}

// Directive is the line prefix that carries requirements.
const Directive = "factory-requires:"

// ParseRequirements reads the directive from a spec. Unknown keys are ignored
// rather than rejected: a ticket written for a newer Factory must still be
// attemptable by an older cell, and refusing the whole ticket over an unfamiliar
// key would make every future field a breaking change.
func ParseRequirements(spec string) Requirements {
	var r Requirements
	for _, line := range strings.Split(spec, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := cutPrefixFold(line, Directive)
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rest) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(key) {
			case "build":
				r.Build = append(r.Build, splitList(value)...)
			case "test":
				r.Test = append(r.Test, splitList(value)...)
			case "attempts":
				var n int
				if _, err := fmt.Sscanf(value, "%d", &n); err == nil && n > 0 {
					r.Attempts = n
				}
			}
		}
	}
	sort.Strings(r.Build)
	sort.Strings(r.Test)
	return r
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Ticket is the state of one ticket as the policy sees it. Every field is read
// from varvig; none is computed here.
type Ticket struct {
	ID string
	// Object is what the ticket's ref resolves to — the anchor for notes.
	Object string
	Spec   string
	Scope  varvigcli.Scope
	// Status is varvig's derived governance status.
	Status string
	// Blockers is varvig's derived blocking set, read not recomputed.
	Blockers []string
}

// Requirements parses the ticket's directive.
func (t Ticket) Requirements() Requirements { return ParseRequirements(t.Spec) }

// Inputs are everything the policy considers. The §5.1 list is: capability
// match, budget headroom, staleness of existing claims, and whether this cell
// has already attempted this task. Everything here maps to one of those, plus
// the ticket's own schedulability, which varvig determines.
type Inputs struct {
	Capabilities cell.Capabilities
	Ticket       Ticket
	// BudgetOK and BudgetReason are the ledger's answer. The policy does not
	// consult the ledger itself, so that "would this claim be affordable" is one
	// decision made in one place.
	BudgetOK     bool
	BudgetReason string
	// OwnAttempts is how many attempts this cell has already made at this task.
	OwnAttempts int
	// MaxAttemptsPerCell caps repeat attempts by this cell at this task. Zero
	// means the budget's default.
	MaxAttemptsPerCell int
	// ForeignClaims are other cells' claims on this task, as read from the
	// repository. While partitioned this list is simply shorter, and the
	// duplicate attempts that result are correct.
	ForeignClaims []cell.Claim
	// Offline says upstream is unreachable.
	Offline bool
	// YieldToFreshClaims makes the cell skip a task another cell has freshly
	// claimed. It defaults on because duplicating work costs budget, and it is
	// configurable because duplicates are legitimate — a federation deliberately
	// running redundant attempts turns it off. It must never be mistaken for
	// mutual exclusion: it does nothing across a partition, by construction.
	YieldToFreshClaims bool
	Now                time.Time
}

// Verdict is the policy's answer.
type Verdict struct {
	Claim bool
	// Attempt is the attempt number to use, when claiming.
	Attempt int
	// Reason explains the decision either way. A cell that skips silently is a
	// cell nobody can debug.
	Reason string
	// Skip is the machine-readable reason, for counters and tests.
	Skip SkipReason
}

// SkipReason is why a claim was not made.
type SkipReason string

// The skip reasons.
const (
	SkipNone SkipReason = ""
	// SkipNotAttempting: this cell does not hold the attempt role. For a Micro
	// cell this is the normal, intended state (§3.1).
	SkipNotAttempting SkipReason = "cell does not attempt"
	// SkipUnschedulable: the ticket has no declared read/write set, so varvig
	// cannot serialize it (TICKETS.md §3.1).
	SkipUnschedulable SkipReason = "ticket is unschedulable"
	// SkipBlocked: varvig derives blockers for this ticket.
	SkipBlocked SkipReason = "ticket is blocked"
	// SkipVetoed: a veto makes every descendant unpromotable, so attempting
	// would burn budget on work that cannot land (TICKETS.md §2.3).
	SkipVetoed SkipReason = "ticket is vetoed"
	// SkipCapability: this cell does not declare a required capability.
	SkipCapability SkipReason = "capability not declared"
	// SkipBudget: no headroom. The cell halts rather than degrading (§7).
	SkipBudget SkipReason = "budget"
	// SkipAlreadyAttempted: this cell has attempted this task enough times.
	SkipAlreadyAttempted SkipReason = "already attempted by this cell"
	// SkipForeignClaim: another cell holds a fresh claim and this cell is
	// configured to yield. Advisory, and inert across a partition.
	SkipForeignClaim SkipReason = "another cell holds a fresh claim"
)

// Evaluate applies the policy.
//
// The checks run cheapest-first and every one returns immediately, so the
// reported reason is the first thing that would have to change for this cell to
// attempt this ticket — which is the reason an operator can act on.
func Evaluate(in Inputs) Verdict {
	if !in.Capabilities.Has(cell.RoleAttempt) {
		return Verdict{Skip: SkipNotAttempting, Reason: fmt.Sprintf(
			"cell %s holds roles %v; attempting is opt-in", in.Capabilities.CellID, roleNames(in.Capabilities.Roles))}
	}
	if !in.Ticket.Scope.Declared() {
		return Verdict{Skip: SkipUnschedulable, Reason: fmt.Sprintf(
			"%s has no declared read/write set, so varvig cannot serialize it", shortID(in.Ticket.ID))}
	}
	if strings.EqualFold(in.Ticket.Status, "vetoed") {
		return Verdict{Skip: SkipVetoed, Reason: fmt.Sprintf(
			"%s is vetoed; every state descending from it is unpromotable", shortID(in.Ticket.ID))}
	}
	if len(in.Ticket.Blockers) > 0 {
		return Verdict{Skip: SkipBlocked, Reason: fmt.Sprintf(
			"%s is blocked by %d ticket(s), as derived by varvig", shortID(in.Ticket.ID), len(in.Ticket.Blockers))}
	}

	req := in.Ticket.Requirements()
	if missing := in.Capabilities.Missing(req.Build, req.Test); len(missing) > 0 {
		return Verdict{Skip: SkipCapability, Reason: fmt.Sprintf(
			"%s requires %s, which cell %s does not declare",
			shortID(in.Ticket.ID), strings.Join(missing, " "), in.Capabilities.CellID)}
	}

	maxAttempts := in.MaxAttemptsPerCell
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if in.OwnAttempts >= maxAttempts {
		return Verdict{Skip: SkipAlreadyAttempted, Reason: fmt.Sprintf(
			"cell %s has already made %d attempt(s) at %s", in.Capabilities.CellID, in.OwnAttempts, shortID(in.Ticket.ID))}
	}

	if !in.BudgetOK {
		reason := in.BudgetReason
		if reason == "" {
			reason = "no headroom"
		}
		return Verdict{Skip: SkipBudget, Reason: "halted: " + reason}
	}

	// Foreign claims are considered last, and only as politeness. Note what this
	// check cannot do: while partitioned, ForeignClaims is empty, so two
	// disconnected cells both claim and both attempt. That is the §9.2
	// behaviour, and it falls out of the data rather than being special-cased.
	if in.YieldToFreshClaims {
		for _, c := range freshest(in.ForeignClaims, in.Now) {
			return Verdict{Skip: SkipForeignClaim, Reason: fmt.Sprintf(
				"cell %s claimed %s until %s; yielding to avoid duplicate spend (advisory only)",
				c.CellID, shortID(in.Ticket.ID), time.Unix(c.NotAfter, 0).UTC().Format(time.RFC3339))}
		}
	}

	attempt := in.OwnAttempts + 1
	reason := fmt.Sprintf("claiming %s as attempt %d", shortID(in.Ticket.ID), attempt)
	if in.Offline {
		reason += " (offline: upstream unreachable, spending against the offline cap)"
	}
	return Verdict{Claim: true, Attempt: attempt, Reason: reason}
}

// freshest returns the non-stale foreign claims, soonest expiry first. A stale
// claim stops being a reason to skip, and the cell that wrote it has no further
// standing from it (CELL.md §5).
func freshest(claims []cell.Claim, now time.Time) []cell.Claim {
	var out []cell.Claim
	for _, c := range claims {
		if !c.Stale(now) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NotAfter < out[j].NotAfter })
	return out
}

func roleNames(roles []cell.Role) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

func shortID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:16] + "…"
}
