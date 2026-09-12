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
	"encoding/json"
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
//
// A ticket may instead — never as well — name an effectful capability, which
// takes it off the speculation path entirely:
//
//	factory-requires: effect=pcb-fabrication@1 interface=1220a1b2…
//	factory-effect: {"gerber":"…","quantity":5}
//
// The two are mutually exclusive because they are opposite kinds of work.
// `build`/`test` describe a code change that is attempted, scored and promoted;
// `effect` describes an order that is placed once and cannot be scored, retried
// or regenerated (§6.7).
type Requirements struct {
	Build    []string
	Test     []string
	Attempts int
	// Effect is the effectful capability this ticket needs, if any. The
	// interface hash is required alongside the alias — an alias alone is
	// ambiguous between factories (§2.1), and here the ambiguity would be
	// resolved by spending money.
	Effect *EffectRequirement
	// ExternallyOriginated marks a ticket an Ambassador created from an outside
	// request (§5b.3). It is the prompt-injection surface made visible: the
	// ticket body is untrusted input that reached the factory from somewhere
	// nobody here controls, and scoping limits the blast radius without
	// eliminating the problem.
	//
	// The marking does not refuse anything by itself. It is a claim-policy
	// input, so an operator decides whether this cell treats such tickets
	// differently — and a factory that wants a human to see every external
	// request before a cell touches it can say so in one setting rather than by
	// inspecting ticket bodies.
	//
	// Absent means internal, which is the safe default in the direction that
	// matters: a ticket nobody marked is treated as ordinary work, and the
	// mistake worth preventing is an external one that *looks* ordinary. That
	// is why the Ambassador marking it is an obligation of the robe rather than
	// something inferred here.
	ExternallyOriginated bool
}

// EffectRequirement is a ticket's declared effectful action.
type EffectRequirement struct {
	// Capability is the alias, e.g. "pcb-fabrication@1".
	Capability string
	// Interface is the interface hash the alias must resolve to.
	Interface string
	// Payload is the action's parameters, verbatim from the directive. It is
	// kept as raw text rather than parsed here because it is hashed into the
	// idempotency key: re-encoding it, even correctly, risks two readings of one
	// ticket producing two keys and therefore two orders.
	Payload string
	// Malformed explains why a declared effect cannot be acted on. A ticket that
	// names an effectful capability badly must not fall through to the ordinary
	// attempt path — that would answer "order me a circuit board" by writing
	// code — so the requirement survives with the reason attached.
	Malformed string
}

// Effectful reports whether this ticket asks for an effectful action.
func (r Requirements) Effectful() bool { return r.Effect != nil }

// EffectGrant is one effectful capability a cell is equipped to act on.
type EffectGrant struct {
	Capability string
	// Interface is the hash the alias resolves to for this cell. It is compared
	// against the ticket's, because a cell holding "pcb-fabrication@1" for a
	// different interface than the ticket means is not equipped for that ticket
	// — it is equipped for a different one with the same name (§2.1).
	Interface string
}

// Directive is the line prefix that carries requirements.
const Directive = "factory-requires:"

// OriginDirective marks where a ticket came from (§5b.3):
//
//	factory-origin: external
//
// Its own directive rather than a key on factory-requires, because it is not a
// requirement: a ticket with no build, test or effect requirement at all can
// still have arrived from outside, and that is exactly the ticket worth
// knowing about.
const OriginDirective = "factory-origin:"

// OriginExternal is the value that marks a ticket externally originated.
const OriginExternal = "external"

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
			case "effect":
				if r.Effect == nil {
					r.Effect = &EffectRequirement{}
				}
				r.Effect.Capability = value
			case "interface":
				if r.Effect == nil {
					r.Effect = &EffectRequirement{}
				}
				r.Effect.Interface = value
			}
		}
	}
	for _, line := range strings.Split(spec, "\n") {
		rest, ok := cutPrefixFold(strings.TrimSpace(line), OriginDirective)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(rest), OriginExternal) {
			r.ExternallyOriginated = true
		}
	}

	sort.Strings(r.Build)
	sort.Strings(r.Test)

	if r.Effect != nil {
		r.Effect.Payload = parsePayload(spec)
		r.Effect.Malformed = validateEffect(*r.Effect, r)
	}
	return r
}

// PayloadDirective carries an effectful action's parameters as canonical JSON.
//
// One line, because canonical JSON contains no newlines (CELL.md §4.3) — the
// same property that makes note payloads parseable — so a directive line is
// enough and no block syntax is needed.
const PayloadDirective = "factory-effect:"

func parsePayload(spec string) string {
	for _, line := range strings.Split(spec, "\n") {
		if rest, ok := cutPrefixFold(strings.TrimSpace(line), PayloadDirective); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// validateEffect returns why a declared effect cannot be acted on, or "".
//
// Every one of these is a refusal rather than a correction. This is the class of
// action where "we assumed you meant X" buys a wrong order.
func validateEffect(e EffectRequirement, r Requirements) string {
	switch {
	case e.Capability == "":
		return "the ticket names an interface but no effectful capability"
	case e.Interface == "":
		return fmt.Sprintf("the ticket names %s by alias with no interface hash; the hash is the identity, and an alias alone is ambiguous between factories (§2.1)", e.Capability)
	case !cell.IsMultihash(e.Interface):
		return fmt.Sprintf("%q is not an object hash, so it names no interface", e.Interface)
	case e.Payload == "":
		return fmt.Sprintf("the ticket asks for %s but declares no %s parameters", e.Capability, PayloadDirective)
	case !json.Valid([]byte(e.Payload)):
		return fmt.Sprintf("the %s parameters are not valid JSON", PayloadDirective)
	case r.Attempts > 1:
		// §9.10, at the earliest point it can be caught: rejected, never
		// clamped. Three attempts at a board order means three invoices, and the
		// ticket's author is the one who needs to know.
		return fmt.Sprintf("the ticket asks for %d attempts at %s, and %d orders is what that would mean; an effectful capability is never speculated on",
			r.Attempts, e.Capability, r.Attempts)
	case len(r.Build) > 0 || len(r.Test) > 0:
		return fmt.Sprintf("the ticket asks for %s and also declares build/test requirements; those are opposite kinds of work and a ticket does one of them", e.Capability)
	}
	return ""
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
	// ExecutorReachable says whether the executor that would author an attempt
	// answered when the cell last asked, and ExecutorReason carries what it said
	// when it did not.
	//
	// Measured by the caller and passed in, for the same reason the budget is: a
	// policy that reached out to a model runtime itself would make evaluating a
	// claim a network operation, and this function is meant to be a
	// deterministic read over refs.
	//
	// **A cell with no attempt role never reaches this check**, so a policy cell
	// need not set it. The zero value therefore reads as "unreachable", which is
	// the safe way round: a caller that forgets declines work rather than
	// attempting it against a runtime nobody confirmed.
	ExecutorReachable bool
	ExecutorReason    string
	// DeclineExternallyOriginated makes this cell skip tickets an Ambassador
	// marked as coming from outside (§9.22).
	//
	// A policy input rather than a rule, because which cells may act on
	// external requests is an operator's decision and not this function's: a
	// factory may want every external ticket seen by a person first, or may
	// want one hardened cell taking them, or may not care. What claim policy
	// guarantees is only that the marking is *available* to decide on.
	DeclineExternallyOriginated bool
	// OwnAttempts is how many attempts this cell has already made at this task.
	OwnAttempts int
	// MaxAttemptsPerCell caps repeat attempts by this cell at this task. Zero
	// means the budget's default.
	MaxAttemptsPerCell int
	// ForeignClaims are other cells' claims on this task, as read from the
	// repository. While partitioned this list is simply shorter, and the
	// duplicate attempts that result are correct.
	ForeignClaims []cell.Claim
	// EffectGrants are the effectful capabilities this cell can actually act on:
	// for each, it holds a lease and has an executor that supports it. A cell
	// with none — which is most cells — declines every effectful ticket, and
	// that is the correct default rather than a misconfiguration.
	EffectGrants []EffectGrant
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
	// SkipEffectMalformed: the ticket names an effectful capability badly. It is
	// its own reason rather than folded into SkipCapability because the fix is
	// the ticket's author's, not the operator's.
	SkipEffectMalformed SkipReason = "effect declaration is malformed"
	// SkipNoAuthority: this cell holds no lease, or no executor, for the
	// capability the ticket needs.
	SkipNoAuthority SkipReason = "no authority for this effectful capability"
	// SkipExternallyOriginated: the ticket came from outside through an
	// Ambassador and this cell is configured not to take such work
	// unsupervised. It is its own reason rather than folded into SkipVetoed
	// because nothing is wrong with the ticket — the operator has decided where
	// external requests get looked at, and a reason that said "vetoed" would
	// send them looking for a veto that does not exist.
	SkipExternallyOriginated SkipReason = "externally originated"
	// SkipNoExecutor: this cell attempts, and the executor that would do the
	// authoring is not reachable right now.
	//
	// It is a *skip*, which is the whole point of §9.17. A cell whose model has
	// gone away is not misconfigured and must not refuse to run: it keeps
	// syncing, keeps verifying other cells' attempts, keeps building, and
	// declines the work it cannot do — saying which. The alternative, refusing
	// to start, takes a cell that can still do every deterministic job in the
	// factory and turns it into one that does nothing at all.
	SkipNoExecutor SkipReason = "no executor reachable"
)

// Evaluate applies the policy.
//
// The checks run cheapest-first and every one returns immediately, so the
// reported reason is the first thing that would have to change for this cell to
// attempt this ticket — which is the reason an operator can act on.
func Evaluate(in Inputs) Verdict {
	req := in.Ticket.Requirements()

	// Where the ticket came from is asked before anything else, and above the
	// effectful split deliberately: an external request that orders a physical
	// thing is the case that matters *most*, not one to fall through to a
	// different branch. A cell that declines external work declines all of it.
	if in.DeclineExternallyOriginated && req.ExternallyOriginated {
		return Verdict{Skip: SkipExternallyOriginated, Reason: fmt.Sprintf(
			"%s was created from an external request and this cell does not take those unsupervised",
			shortID(in.Ticket.ID))}
	}

	// An effectful ticket is not attempted, so the attempt role does not gate it.
	// What gates it is holding a lease — authority to spend, not a declared
	// toolchain — and a Micro cell with a lease is as entitled to place an order
	// as a Mini one. Conflating the two would tie the right to spend money to
	// the presence of a model, which is not a relationship that should exist.
	if req.Effectful() {
		return evaluateEffect(in, req)
	}

	if !in.Capabilities.Has(cell.RoleAttempt) {
		return Verdict{Skip: SkipNotAttempting, Reason: fmt.Sprintf(
			"cell %s holds roles %v; attempting is opt-in", in.Capabilities.CellID, roleNames(in.Capabilities.Roles))}
	}
	// The executor comes right after the role, because it is the cheapest and
	// most decisive thing that can stop an attempt: there is no point weighing
	// scope, capabilities and budget for work nothing can author.
	if !in.ExecutorReachable {
		reason := in.ExecutorReason
		if reason == "" {
			reason = "the executor that would author this attempt is not reachable"
		}
		return Verdict{Skip: SkipNoExecutor, Reason: reason}
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

// evaluateEffect is the claim decision for a ticket that names an effectful
// capability (§6.7).
//
// It shares the schedulability and politeness checks with the ordinary path and
// differs in three ways, each for a reason:
//
//   - **No role gate.** Authority to spend comes from a lease, not from
//     declaring a toolchain.
//   - **No inference-budget gate.** An order is paid from its lease; refusing to
//     place one because the day's model spend is exhausted would couple two
//     unrelated budgets, and the coupling would surface as an order that
//     silently did not happen.
//   - **One attempt, always.** Not the ticket's default and not the budget's:
//     the number is 1, and a ticket asking for more was already refused as
//     malformed.
func evaluateEffect(in Inputs, req Requirements) Verdict {
	id := shortID(in.Ticket.ID)

	if req.Effect.Malformed != "" {
		return Verdict{Skip: SkipEffectMalformed, Reason: fmt.Sprintf("%s: %s", id, req.Effect.Malformed)}
	}
	if !in.Ticket.Scope.Declared() {
		// The same rule as any other ticket. An effectful ticket usually writes
		// nothing, but it still has to be schedulable — and a ticket nobody
		// declared a scope for is one varvig cannot order against the rest.
		return Verdict{Skip: SkipUnschedulable, Reason: fmt.Sprintf(
			"%s has no declared read/write set, so varvig cannot serialize it", id)}
	}
	if strings.EqualFold(in.Ticket.Status, "vetoed") {
		return Verdict{Skip: SkipVetoed, Reason: fmt.Sprintf("%s is vetoed", id)}
	}
	if len(in.Ticket.Blockers) > 0 {
		return Verdict{Skip: SkipBlocked, Reason: fmt.Sprintf(
			"%s is blocked by %d ticket(s), as derived by varvig", id, len(in.Ticket.Blockers))}
	}

	if !hasGrant(in.EffectGrants, *req.Effect) {
		return Verdict{Skip: SkipNoAuthority, Reason: fmt.Sprintf(
			"%s needs %s (%s), which cell %s holds no lease and executor for",
			id, req.Effect.Capability, shortID(req.Effect.Interface), in.Capabilities.CellID)}
	}

	// One attempt per cell, enforced here as well as by the reservation ref.
	// Belt and braces is right for this class: the ref is the guarantee, and this
	// is what stops the cell wasting a claim to discover it.
	if in.OwnAttempts >= 1 {
		return Verdict{Skip: SkipAlreadyAttempted, Reason: fmt.Sprintf(
			"cell %s has already acted on %s; an effectful action is never re-run", in.Capabilities.CellID, id)}
	}

	if in.YieldToFreshClaims {
		for _, c := range freshest(in.ForeignClaims, in.Now) {
			// Yielding matters more here than anywhere else: two cells that both
			// proceed produce two orders, and only the reservation ref stops
			// them — which it cannot do across a partition, since each cell
			// writes under its own prefix.
			return Verdict{Skip: SkipForeignClaim, Reason: fmt.Sprintf(
				"cell %s claimed %s until %s; yielding, because two cells acting here means two orders",
				c.CellID, id, time.Unix(c.NotAfter, 0).UTC().Format(time.RFC3339))}
		}
	}

	return Verdict{Claim: true, Attempt: 1, Reason: fmt.Sprintf(
		"claiming %s to perform %s", id, req.Effect.Capability)}
}

func hasGrant(grants []EffectGrant, want EffectRequirement) bool {
	for _, g := range grants {
		// Both must match. The alias alone would let a cell act on a ticket
		// meaning a different interface of the same name, and the hash alone
		// would let it act under a name it was never granted.
		if g.Capability == want.Capability && g.Interface == want.Interface {
			return true
		}
	}
	return false
}
