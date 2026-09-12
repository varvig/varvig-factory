package decide

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/executor"
)

// meta is the separate budget line of guardrail 1, with a counter so a vector
// can assert what was *not* spent as well as what was.
type meta struct {
	Headroom bool
	Spent    cell.Money
	Calls    int
}

func (m *meta) CanSpend() bool { return m.Headroom }
func (m *meta) Spend(a cell.Money) {
	m.Calls++
	m.Spent += a
}

func question() Question {
	return Question{
		Task:     "a1b2c3",
		Ask:      "Two attempts disagree about the fix. Which reading of the ticket is right?",
		Options:  []string{"retry", "escalate"},
		Fallback: "escalate",
	}
}

// asker is an executor that tries to ask a decision task of its own, which is
// the nesting case. It is written the way the mistake would actually be made:
// by passing along the context it was handed.
type asker struct {
	*executor.FakeAuthor
	led    Ledger
	inner  error
	called bool
}

func (a *asker) Author(ctx context.Context, r executor.Request) (executor.Response, error) {
	a.called = true
	_, a.inner = Ask(ctx, a.FakeAuthor, a.led, question())
	return a.FakeAuthor.Author(ctx, r)
}

// Test18_DecisionTaskIsolation is §9.18: all four guardrails of §5.3, each
// asserted where it would actually give way.
//
// They are one vector because they are one property — a decision task is a
// bounded, non-recursive, unprivileged, optional call — and because three of
// the four fail *open*: a broken guardrail does not throw, it quietly lets a
// cell think more, or deeper, or with authority it was never given. Nothing
// here would show up as a crash.
func Test18_DecisionTaskIsolation(t *testing.T) {
	ctx := context.Background()

	// Guardrail 1: the meta line is spent, and it is the only thing spent. The
	// main ledger is not passed in at all, which is the structural half of the
	// rule — there is no argument here that could accidentally be the budget
	// meant for doing the work.
	m := &meta{Headroom: true}
	fake := &executor.FakeAuthor{Reply: "retry"}
	d, err := Ask(ctx, fake, m, question())
	if err != nil {
		t.Fatal(err)
	}
	if d.Answer != "retry" || d.FromPolicy {
		t.Fatalf("decision = %+v, want the executor's answer", d)
	}
	if m.Calls != 1 {
		t.Fatalf("the meta line was charged %d times for one decision", m.Calls)
	}

	// Guardrail 1 biting: no headroom is not an error and does not borrow. The
	// cell decides deterministically and the executor is never reached, so a
	// cell that has thought its way through the meta line stops spending rather
	// than starting on the line it needs to do the work.
	empty := &meta{Headroom: false}
	spent := &executor.FakeAuthor{Reply: "retry"}
	d, err = Ask(ctx, spent, empty, question())
	if err != nil {
		t.Fatalf("an exhausted meta line was reported as an error: %v", err)
	}
	if !d.FromPolicy || d.Answer != question().Fallback {
		t.Fatalf("decision = %+v, want the policy fallback", d)
	}
	if spent.Calls != 0 {
		t.Fatal("an executor was called with no meta-budget headroom; the call is the spend")
	}
	if !strings.Contains(d.Why, "meta-budget") {
		t.Fatalf("the reason does not name the line that ran out: %s", d.Why)
	}

	// Guardrail 2: one level, hard stop. The inner call is made with the
	// context the executor was handed, which is how the recursion would
	// actually arrive — each level looking locally reasonable.
	nest := &asker{FakeAuthor: &executor.FakeAuthor{Reply: "retry"}, led: &meta{Headroom: true}}
	if _, err := Ask(ctx, nest, &meta{Headroom: true}, question()); err != nil {
		t.Fatal(err)
	}
	if !nest.called {
		t.Fatal("the nesting executor never ran, so nothing was tested")
	}
	if !errors.Is(nest.inner, ErrNested) {
		t.Fatalf("a decision task spawned a decision task: inner error %v", nest.inner)
	}

	// Guardrail 3: an unreachable executor degrades, never stalls. Policy has
	// to suffice to at least safely do nothing, or a cell's ability to act
	// depends on a model being up.
	down := &executor.FakeAuthor{Err: errors.New("connection refused")}
	d, err = Ask(ctx, down, &meta{Headroom: true}, question())
	if err != nil {
		t.Fatalf("an unreachable executor stalled the cell: %v", err)
	}
	if !d.FromPolicy || d.Answer != question().Fallback {
		t.Fatalf("decision = %+v, want the policy fallback", d)
	}
	// Having no executor configured at all is the same case, not a worse one.
	if d, err := Ask(ctx, nil, &meta{Headroom: true}, question()); err != nil || !d.FromPolicy {
		t.Fatalf("a cell with no decision executor = (%+v, %v), want the fallback", d, err)
	}

	// An answer outside the options is a suggestion, not a decision. Taking it
	// would mean acting on something no policy ruled in — which is guardrail 4
	// arriving by the back door rather than by a new field.
	loose := &executor.FakeAuthor{Reply: "rewrite the ticket"}
	d, err = Ask(ctx, loose, &meta{Headroom: true}, question())
	if err != nil {
		t.Fatal(err)
	}
	if !d.FromPolicy || d.Answer != question().Fallback {
		t.Fatalf("an inadmissible answer was taken as a decision: %+v", d)
	}

	// Guardrail 4, asserted on the type rather than on behaviour. A decision
	// informs a choice within bounds already granted; the way it would stop
	// doing that is a field being added here, not a branch changing, so the
	// type is where the check belongs.
	forbidden := []string{"grant", "authoriz", "permit", "principal", "lease", "budget", "ceiling", "envelope"}
	qt := reflect.TypeOf(Question{})
	for i := 0; i < qt.NumField(); i++ {
		f := qt.Field(i)
		if f.Type == reflect.TypeOf(cell.Money(0)) {
			t.Fatalf("Question.%s names an amount; a cell would be asking a model how much it may spend", f.Name)
		}
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(f.Name), bad) {
				t.Fatalf("Question.%s carries authority into a decision task", f.Name)
			}
		}
	}

	// A malformed question is the one case that is an error, because carrying
	// on past it would mean carrying on wrongly: an unanswerable question with
	// no fallback leaves a cell with nothing deterministic to fall back to.
	for name, q := range map[string]Question{
		"no question":   {Task: "t", Options: []string{"a", "b"}, Fallback: "a"},
		"one option":    {Task: "t", Ask: "which?", Options: []string{"a"}, Fallback: "a"},
		"no fallback":   {Task: "t", Ask: "which?", Options: []string{"a", "b"}},
		"alien default": {Task: "t", Ask: "which?", Options: []string{"a", "b"}, Fallback: "c"},
	} {
		if _, err := Ask(ctx, fake, &meta{Headroom: true}, q); err == nil {
			t.Fatalf("%s: a malformed question was asked anyway", name)
		}
	}

	// And no meta line at all is a refusal, not a fallback to the main one.
	if _, err := Ask(ctx, fake, nil, question()); !errors.Is(err, ErrNoMetaBudget) {
		t.Fatalf("with no meta line configured the error was %v", err)
	}
}
