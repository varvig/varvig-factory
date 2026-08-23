// Package gate is the promotion-policy module interface (FACTORY.md §6.2).
//
// The gate is a content-addressed wasm module, versioned in the repository
// alongside the code it guards. It receives an attempt, its evidence and its
// environment, and returns promote / refuse / defer-to-human. Because the module
// is a repository object, **the promotion rule is itself reviewed, versioned and
// auditable** — which is the minimum bar for letting a machine promote.
//
// # Where the wasm runs
//
// It runs inside varvig, not inside Factory. varvig already has a closed WASI
// sandbox for hooks — no filesystem, no network, no environment, no unbounded
// clock — and a content-addressed module store to go with it. Factory binds its
// module to a Factory-specific event with `varvig hook set` and runs it with
// `varvig hook run`.
//
// That is not a shortcut around a missing runtime. Embedding a second wasm
// runtime in Factory would mean two sandboxes with two sets of escape bugs, and
// a policy module whose behaviour depended on which binary loaded it. One
// sandbox, owned by the layer that already had to get it right.
//
// # The three-valued verdict
//
// A hook signals with an exit code, and a promotion policy needs three answers,
// so the mapping is fixed here:
//
//	0        promote
//	1        refuse
//	2        defer to a human
//	anything else, or no module, or a crash  →  defer to a human
//
// Everything unrecognised defers. A gate that cannot be understood must never
// read as consent — and note that "no module configured" is in that list: an
// unconfigured gate is not an approving gate (see ErrNoModule).
package gate

import (
	"context"
	"errors"
	"fmt"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Event is the varvig hook event a Factory promotion gate binds to. It is
// namespaced so it cannot collide with varvig's own trigger points.
const Event = "factory-promote"

// Verdict is the gate's answer.
type Verdict int

// The three verdicts.
const (
	// Refuse: do not promote this attempt, and do not ask a human either. The
	// module has decided.
	Refuse Verdict = iota
	// Promote: this attempt may be promoted, subject to every other §6.3
	// condition — the gate is one constraint among several, never a bypass.
	Promote
	// Defer: a human decides. This is the safe answer and the default for
	// anything the gate could not resolve.
	Defer
)

func (v Verdict) String() string {
	switch v {
	case Promote:
		return "promote"
	case Refuse:
		return "refuse"
	default:
		return "defer-to-human"
	}
}

// Exit codes, fixed by the contract above.
const (
	exitPromote = 0
	exitRefuse  = 1
	exitDefer   = 2
)

// ErrNoModule reports that no gate module is bound to the event.
//
// It is an error rather than a Defer verdict on purpose. Callers must decide
// explicitly what an unconfigured gate means for them — for `gated` promotion it
// is fine and expected, since a human decides anyway, while for `autonomous` it
// is disqualifying. Returning Defer here would let both paths share one code
// path that silently did the wrong thing in the second case.
var ErrNoModule = errors.New("gate: no promotion-policy module is bound")

// Input is what the module receives on stdin, as canonical JSON.
//
// It is a computed context rather than a set of host functions the module can
// call — the same shape varvig's own policy modules take (TICKETS.md §2.5). Live
// host functions are a later refinement and would not change this struct's
// meaning; passing a context first means the module cannot make the host do
// anything, which is the property worth having while the ABI is young.
type Input struct {
	// Attempt is the record under consideration.
	Attempt cell.Attempt `json:"attempt"`
	// Evidence is every evidence record for the attempt, from every cell.
	Evidence []cell.Evidence `json:"evidence"`
	// Environments maps an environment hash to its descriptor, for every
	// environment named by the evidence above.
	Environments map[string]cell.Environment `json:"environments"`
	// AttemptEnvironment is the environment the attempt was authored in, if
	// known.
	AttemptEnvironment *cell.Environment `json:"attempt_environment,omitempty"`
	// Baseline is the declared environment baseline for the path being promoted,
	// if one is declared (FEDERATION.md §2.3).
	Baseline *cell.Environment `json:"baseline,omitempty"`
	// Scope is the path scope this promotion would write to.
	Scope string `json:"scope"`
	// Mode is "gated" or "autonomous", so a module can hold autonomous
	// promotion to a stricter rule than it holds a human-reviewed one.
	Mode string `json:"mode"`
	// Agreement is the recorded promotion-agreement rate for the scope.
	Agreement AgreementView `json:"agreement"`
	// Independent reports whether at least one evidence record came from a cell
	// other than the attempting one. The host computes it so every module gets
	// the same answer rather than each re-deriving it (CELL.md §4.1).
	Independent bool `json:"independent"`
	// TicketStatus is varvig's derived governance status for the ticket.
	TicketStatus string `json:"ticket_status"`
}

// AgreementView is the agreement rate as the module sees it.
type AgreementView struct {
	Observations int     `json:"observations"`
	Agreements   int     `json:"agreements"`
	Rate         float64 `json:"rate"`
}

// Result is one gate evaluation.
type Result struct {
	Verdict Verdict
	// Stdout and Stderr are the module's own explanation, carried through so a
	// refusal or a deferral can say what the policy said.
	Stdout, Stderr string
	// ExitCode is the raw code, kept for logs: an unrecognised code defers, and
	// an operator needs to see which code it was.
	ExitCode int
}

// Reason renders the module's explanation, or a fallback.
func (r Result) Reason() string {
	switch {
	case r.Stderr != "":
		return r.Stderr
	case r.Stdout != "":
		return r.Stdout
	default:
		return fmt.Sprintf("policy module exited %d", r.ExitCode)
	}
}

// Module runs a promotion-policy module.
type Module struct {
	V varvigcli.Varvig
	// EventName defaults to Event.
	EventName string
}

func (m Module) event() string {
	if m.EventName != "" {
		return m.EventName
	}
	return Event
}

// Bind installs a wasm module as the promotion gate, returning its object id.
// The id is what makes the rule auditable: an attestation or a log line naming
// it says exactly which policy was in force.
func (m Module) Bind(modulePath string) (string, error) {
	return m.V.HookSet(m.event(), modulePath)
}

// Evaluate runs the gate over an input.
//
// Every failure mode lands on Defer, and the reason travels with it. The one
// case that is *not* a verdict is "no module bound", which returns ErrNoModule —
// see that error's comment for why the distinction matters.
func (m Module) Evaluate(ctx context.Context, in Input) (Result, error) {
	payload, err := cell.Canonical(in)
	if err != nil {
		return Result{Verdict: Defer}, err
	}
	results, err := m.V.HookRun(ctx, m.event(), payload)
	if err != nil {
		// A module that could not be run has not approved anything. The error is
		// returned *and* the verdict is Defer, so a caller that logs the error
		// and continues still continues safely.
		return Result{Verdict: Defer, Stderr: err.Error()}, err
	}
	if len(results) == 0 {
		return Result{Verdict: Defer}, ErrNoModule
	}

	// More than one module may be bound. Constraints stack and the most
	// conservative answer wins — the same rule varvig applies to its own
	// composed policies (TICKETS.md §2.5: any one refusing is decisive). Refuse
	// beats Defer beats Promote, so a second module can only ever tighten the
	// first.
	out := Result{Verdict: Promote}
	for _, r := range results {
		v := verdictOf(r.ExitCode)
		if rank(v) > rank(out.Verdict) {
			out.Verdict = v
			out.ExitCode = r.ExitCode
			out.Stdout, out.Stderr = r.Stdout, r.Stderr
		}
	}
	if out.Verdict == Promote && len(results) > 0 {
		out.ExitCode = exitPromote
		out.Stdout, out.Stderr = results[0].Stdout, results[0].Stderr
	}
	return out, nil
}

// verdictOf maps an exit code to a verdict, defaulting to Defer.
func verdictOf(code int) Verdict {
	switch code {
	case exitPromote:
		return Promote
	case exitRefuse:
		return Refuse
	case exitDefer:
		return Defer
	default:
		// An unrecognised code is not a vote. A module built against a future
		// version of this contract, or one that crashed with its own exit code,
		// must reach a human rather than be read as consent.
		return Defer
	}
}

// rank orders verdicts by conservatism, most conservative highest.
func rank(v Verdict) int {
	switch v {
	case Refuse:
		return 2
	case Defer:
		return 1
	default:
		return 0
	}
}
