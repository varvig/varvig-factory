package effect

import (
	"context"
	"fmt"
	"sync"

	"github.com/varvig/varvig-factory/cell"
)

// Fake is an in-memory Executor for tests and the simulator.
//
// It exists mostly to make one thing assertable that nothing else can: **how
// many times the effect actually happened.** Every guard in this package is
// ultimately about that number being 1, and a test that cannot count it is
// testing the guard's error message rather than its effect.
//
// It is in the non-test file deliberately, so the simulator and a cell's own
// dry-run configuration can use it. A fake purchasing integration that refuses
// to spend anything is a genuinely useful thing to point a cell at while
// setting one up.
type Fake struct {
	// Capability is what this fake claims to support. Matching is on the
	// interface hash, like the real thing.
	Capability Capability
	// Price is what Quote reports. Actual, when non-zero, is what Execute
	// reports it really cost — the divergence that settlement records.
	Price    Quote
	Actual   cell.Money
	Ref      string
	QuoteErr error
	// ExecErr is returned by Execute. Wrap ErrRejected to model a definite
	// rejection; anything else models an unknown outcome, which is the case
	// worth testing and the one implementations get wrong.
	ExecErr error

	mu sync.Mutex
	// Executed records every key Execute was called with, in order. A repeated
	// key here is a double-spend that got past every guard.
	Executed []string
	Quoted   []string
}

// NewFake returns a fake that quotes amount and succeeds.
func NewFake(c Capability, amount cell.Money, unit string) *Fake {
	return &Fake{
		Capability: c,
		Price:      Quote{Amount: amount, Unit: unit},
		Ref:        "FAKE-" + short(c.Interface),
	}
}

func (f *Fake) Supports(c Capability) bool { return f.Capability.Matches(c) }

func (f *Fake) Quote(_ context.Context, req Request) (Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Quoted = append(f.Quoted, req.Task)
	if f.QuoteErr != nil {
		return Quote{}, f.QuoteErr
	}
	return f.Price, nil
}

func (f *Fake) Execute(_ context.Context, _ Request, key string) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Executed = append(f.Executed, key)
	if f.ExecErr != nil {
		return Outcome{}, f.ExecErr
	}
	return Outcome{ExternalRef: f.Ref, Actual: f.Actual}, nil
}

// Count returns how many times the effect happened.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Executed)
}

// Refusing is an executor that supports a capability and declines to act.
//
// Pointing a cell at this is how an operator proves the wiring works — the
// ticket is claimed, quoted, authorized and reserved — without anything being
// ordered. The refusal is a definite rejection, so the hold is released and
// nothing is left pending.
type Refusing struct{ Capability Capability }

func (r Refusing) Supports(c Capability) bool { return r.Capability.Matches(c) }

func (r Refusing) Quote(context.Context, Request) (Quote, error) {
	return Quote{}, fmt.Errorf("effect: %s is wired to a refusing executor; nothing will be ordered", r.Capability.ID)
}

func (r Refusing) Execute(context.Context, Request, string) (Outcome, error) {
	return Outcome{}, fmt.Errorf("%w: this capability is wired to a refusing executor", ErrRejected)
}
