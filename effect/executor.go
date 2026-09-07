package effect

import (
	"context"
	"errors"
	"fmt"
)

// The executor seam: the one place Factory touches an external world that
// charges money.
//
// It is an interface for the same reason the model runtime and the build sandbox
// are (§4): the thing behind it is a vendor, and a vendor reached directly from
// the loop is a vendor that has to be there for the loop to be testable. Here
// the argument is sharper than for the other seams — the alternative to a fake
// is a real board order.
//
// # Why quoting is separate from executing
//
// §7.1's lifecycle is quote → reserve → authorize → execute → settle, and the
// split between the first and the fourth is what makes the reservation possible:
// a hold needs an amount, and for a quoted capability the amount is not known
// until the service is asked. Merging them would mean either reserving a guess
// or executing before anything is held.
//
// The contract is therefore blunt: **Quote must not cause an effect, and Execute
// must be called at most once per idempotency key.** An implementation that
// cannot honour the first has no business being a quoted capability; the second
// is enforced by the reservation ref, not by the implementation.

// Quote is what an action will cost, as the external service reports it.
type Quote struct {
	// Amount and Quantity are what the action will cost and order. Amount may
	// still differ from the invoice — a quote is a quote — which is why
	// settlement records the actual (§7.1).
	Amount   float64 `json:"amount"`
	Quantity int64   `json:"quantity,omitempty"`
	Unit     string  `json:"unit"`
	// Detail is anything the operator should see before the money leaves:
	// lead time, a line-item breakdown, a substitution the vendor made.
	Detail string `json:"detail,omitempty"`
}

// Outcome is the result of an effect that happened.
type Outcome struct {
	// ExternalRef is the far end's own identifier — the order number, the
	// contract id. It is required: a spend nobody can look up is not a settled
	// one, and Settle refuses without it.
	ExternalRef string `json:"external_ref"`
	// Actual is what it really cost, when that differs from the quote. Zero
	// means "as quoted".
	Actual float64 `json:"actual,omitempty"`
	Detail string  `json:"detail,omitempty"`
}

// Executor performs effectful actions for one or more capabilities.
type Executor interface {
	// Supports reports whether this executor can act for a capability. Matching
	// is on the interface **hash**, never the alias (§2.1).
	Supports(c Capability) bool

	// Quote prices an action **without causing an effect**. It is called before
	// anything is reserved.
	Quote(ctx context.Context, req Request) (Quote, error)

	// Execute causes the effect, at most once per key.
	//
	// The key is passed through to the external service wherever that service
	// supports idempotency keys of its own, so the guarantee holds end to end
	// rather than only up to our side of the wire.
	//
	// A returned error means the outcome is **unknown**, not that nothing
	// happened — the reservation stays pending and escalates. An implementation
	// that has definitely been rejected must say so with ErrRejected, because
	// "the vendor said no" and "we never heard back" lead to opposite decisions.
	Execute(ctx context.Context, req Request, key string) (Outcome, error)
}

// ErrRejected reports that the external service definitely refused, so **no
// effect occurred**.
//
// This is the only way an implementation may claim that nothing happened. Every
// other error leaves the reservation pending, because a timeout, a dropped
// connection and a 500 are all consistent with the order having been placed.
// Wrapping this error is an assertion about the outside world; make it only when
// the service actually said no.
var ErrRejected = errors.New("effect: the external service rejected this action")

// ErrNoExecutor is returned when nothing is configured to perform a capability.
var ErrNoExecutor = errors.New("effect: no executor is configured for this capability")

// Executors dispatches to the first executor that supports a capability.
//
// An empty set is legal and is the default: most cells hold no effectful
// capabilities at all, and requiring every deployment to configure a purchasing
// integration in order to build code would be absurd. Such a cell declines the
// ticket and says why.
type Executors []Executor

// For returns the executor for a capability, or ErrNoExecutor.
func (e Executors) For(c Capability) (Executor, error) {
	for _, x := range e {
		if x.Supports(c) {
			return x, nil
		}
	}
	return nil, fmt.Errorf("%w: %s (%s)", ErrNoExecutor, c.ID, short(c.Interface))
}
