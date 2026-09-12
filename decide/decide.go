// Package decide is the decision task of FACTORY.md §5.3: what a cell submits
// when deterministic policy genuinely cannot decide.
//
// # Why this is small
//
// The temptation here is a subsystem — a reasoning loop the cell consults
// whenever it is unsure. The spec's §5.0 rule stands against it: the main loop
// calls no model, and wanting one every turn is a signal the policy layer is
// underspecified rather than a reason to add intelligence. A decision task is
// one inference call down the ordinary executor path, recorded as an attempt so
// a claim can be audited later, and nothing else.
//
// # Four guardrails, or this eats itself
//
// Each is enforced here rather than documented, and each has a specific way of
// going wrong:
//
//  1. **A separate budget line.** Otherwise deciding consumes the lease meant
//     for doing, invisibly, because it looks like ordinary spend. A cell that
//     thought its way through its whole budget would have nothing left to think
//     *about*.
//  2. **No nesting.** A decision task cannot spawn a decision task. One level,
//     hard stop — the recursion has no natural floor and each level looks
//     locally reasonable.
//  3. **An unreachable executor degrades to policy, never stalls.** Policy must
//     always suffice to at least safely do nothing, so a cell that cannot reach
//     a model is a cell that decides deterministically, not one that waits.
//  4. **No authority.** A decision informs a choice within already-granted
//     bounds. It never authorizes anything, or §6.7's separation collapses and
//     a cell authorizes its own spending by thinking harder about it.
//
// The fourth is the one worth being most careful about, because it is the one
// that would be most useful to break.
package decide

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/executor"
)

// ErrNested is returned when a decision task tries to spawn another.
var ErrNested = errors.New("decide: a decision task cannot spawn a decision task")

// ErrNoMetaBudget is returned when no meta-budget line is configured.
//
// A refusal rather than a fallback to the main budget, which is the whole point
// of guardrail 1: falling back is exactly the invisible consumption the
// separate line exists to prevent.
var ErrNoMetaBudget = errors.New("decide: no meta-budget line is configured for decision tasks")

// Question is what the cell could not decide.
//
// It carries no authority and cannot: there is no field here for a spend, a
// principal, or a permission, and that absence is deliberate. A decision task
// that could name an amount would be a cell asking a model how much it may
// spend, which is the collapse guardrail 4 exists to prevent.
type Question struct {
	// Task is the ticket the cell is deciding about, for attribution.
	Task string
	// Ask is the question in the cell's own words.
	Ask string
	// Options are the answers the deterministic policy considers admissible.
	//
	// Closed rather than open, because a decision task chooses *among* choices
	// policy has already ruled in. An open answer would let the model propose
	// something nothing had authorized, and the cell would have no way to tell
	// the difference between judgment and invention.
	Options []string
	// Fallback is what deterministic policy decides when no model answers. It
	// must be one of Options, and it must exist: guardrail 3 is not "degrade
	// gracefully", it is "policy always suffices".
	Fallback string
}

// Validate rejects a question that cannot be safely asked.
func (q Question) Validate() error {
	if strings.TrimSpace(q.Ask) == "" {
		return errors.New("decide: a decision task needs a question")
	}
	if len(q.Options) < 2 {
		return fmt.Errorf("decide: %q offers %d option(s); a decision among fewer than two is policy's job",
			q.Ask, len(q.Options))
	}
	if q.Fallback == "" {
		return fmt.Errorf("decide: %q names no fallback; policy must suffice to decide with no model at all", q.Ask)
	}
	if !q.admits(q.Fallback) {
		return fmt.Errorf("decide: %q falls back to %q, which is not one of its options", q.Ask, q.Fallback)
	}
	return nil
}

func (q Question) admits(answer string) bool {
	for _, o := range q.Options {
		if o == answer {
			return true
		}
	}
	return false
}

// Decision is the answer and how it was reached.
type Decision struct {
	// Answer is always one of the question's options, whatever happened.
	Answer string
	// FromPolicy reports that deterministic policy decided — because no
	// executor was reachable, because it declined, or because it answered
	// something inadmissible.
	FromPolicy bool
	// Why names the reason policy decided, empty when a model did.
	Why string
	// Spent is what the call cost, for the meta-budget line.
	Spent cell.Money
}

// Ledger is the separate budget line decision tasks spend from (guardrail 1).
//
// An interface rather than the budget package's type so that "the meta line" is
// a distinct thing a caller has to supply deliberately. Passing the cell's main
// ledger here is possible and is the mistake; it is not possible to do it by
// accident.
type Ledger interface {
	// CanSpend reports whether the meta line has headroom.
	CanSpend() bool
	// Spend records what a decision cost.
	Spend(amount cell.Money)
}

// Nested marks a context as already inside a decision task (guardrail 2).
func Nested(ctx context.Context) context.Context {
	return context.WithValue(ctx, nestedKey{}, true)
}

type nestedKey struct{}

func isNested(ctx context.Context) bool {
	v, _ := ctx.Value(nestedKey{}).(bool)
	return v
}

// Ask submits a decision task and returns the answer.
//
// It never returns an error for anything the cell can carry on past: an
// unreachable executor, a refused call, an inadmissible answer and an exhausted
// meta-budget all produce the fallback with FromPolicy set. An error means the
// *question* was malformed, or that it was asked from inside another decision
// task — the two cases where carrying on would mean carrying on wrongly.
func Ask(ctx context.Context, e executor.Authoring, led Ledger, q Question) (Decision, error) {
	if err := q.Validate(); err != nil {
		return Decision{}, err
	}
	if isNested(ctx) {
		return Decision{}, fmt.Errorf("%w: %q", ErrNested, q.Ask)
	}
	policy := func(why string) Decision {
		return Decision{Answer: q.Fallback, FromPolicy: true, Why: why}
	}

	if led == nil {
		return Decision{}, ErrNoMetaBudget
	}
	if !led.CanSpend() {
		// Guardrail 1 biting. The cell decides deterministically rather than
		// borrowing from the budget meant for doing the work.
		return policy("the meta-budget line for decision tasks has no headroom"), nil
	}
	if e == nil {
		return policy("no executor is configured for decision tasks"), nil
	}

	// Guardrail 2: whatever the executor does, it is doing it inside a decision
	// task, and anything it reaches cannot start another.
	resp, err := e.Author(Nested(ctx), executor.Request{
		Task:      q.Task,
		Intent:    prompt(q),
		MaxTokens: 64,
	})
	if err != nil {
		// Guardrail 3. An unreachable or refusing executor is not an error
		// here: policy suffices, and stalling would make a cell's ability to
		// act depend on a model being up.
		return policy(fmt.Sprintf("the executor declined: %v", err)), nil
	}

	answer := strings.TrimSpace(resp.Text)
	if !q.admits(answer) {
		// An answer outside the options is not a decision, it is a suggestion,
		// and taking it would mean acting on something nothing authorized.
		return policy(fmt.Sprintf("the executor answered %q, which is not one of the options", truncate(answer))), nil
	}
	led.Spend(resp.Cost)
	return Decision{Answer: answer, Spent: resp.Cost}, nil
}

func prompt(q Question) string {
	var b strings.Builder
	b.WriteString(q.Ask)
	b.WriteString("\n\nAnswer with exactly one of:\n")
	for _, o := range q.Options {
		b.WriteString("  ")
		b.WriteString(o)
		b.WriteString("\n")
	}
	b.WriteString("\nReply with the option alone and nothing else.\n")
	return b.String()
}

func truncate(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
