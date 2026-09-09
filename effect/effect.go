// Package effect is the effectful, non-regenerable capability class
// (FACTORY.md §6.7).
//
// Ordering PCB fabrication, contracting a human, sending a shipment, moving
// money: real-world side effects that cannot be re-run, discarded, or
// regenerated.
//
// # Why this needs its own package
//
// Every other assumption in Factory is inverted here. Speculation is search, so
// attempts are cheap and disposable. Duplicates across a partition are normal
// and are the point. Conflicts resolve by regeneration. **None of that is true
// for an effectful action.** `--attempts 3` on a board order means three orders
// and three invoices.
//
// The hazard is that an effectful capability looks exactly like an ordinary one
// until the invoice arrives. So this package is built as refusals first, and the
// happy path is the small part at the end — the spec's own instruction, and the
// right order when a missing guard costs money rather than compute.
//
// # What is deliberately absent
//
// There is no retry. A failed effectful action escalates; retry is an authorized
// decision, not a loop behaviour. There is no regeneration: a conflicting
// effectful attempt does not re-run, because the external world has already
// moved. Both absences are load-bearing, and both are the kind of thing a later
// "improvement" would add back.
package effect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
)

// CostModel is the cell contract's cost model, aliased here so this package
// reads naturally without there being two definitions to drift apart. It is
// declared in cell/ because a capability's cost model is configuration, and
// where it is declared decides who may declare it (see cell.CostModel).
type CostModel = cell.CostModel

// The cost models, re-exported from the cell contract.
const (
	// CostFixed is a price known from the capability contract.
	CostFixed = cell.CostFixed
	// CostQuoted means the price is not known until an external service is
	// asked. Such capabilities **require connectivity by their nature**, not by
	// policy — so there is no rule here forbidding offline quoted actions, only
	// the observation that they cannot happen.
	CostQuoted = cell.CostQuoted
)

// Capability is an effectful capability's contract, as a cell reads it.
type Capability struct {
	// ID is the interface alias, e.g. "pcb-fabrication@1".
	ID string `json:"id"`
	// Interface is the hash of the interface schema object. It is required, and
	// the alias alone is not enough: two factories may hold the same alias
	// without agreeing who owns the name, so binding to the hash is what keeps
	// a capability reference unambiguous (§2.1).
	Interface string `json:"interface"`
	// Effectful marks the class. It is explicit rather than inferred, because
	// the whole failure mode of this class is that it looks ordinary.
	Effectful bool      `json:"effectful"`
	CostModel CostModel `json:"cost_model,omitempty"`
}

// Validate rejects a capability reference that cannot be matched safely.
func (c Capability) Validate() error {
	if c.ID == "" {
		return errors.New("effect: capability has no id")
	}
	if c.Interface == "" {
		// §2.1: "A capability must reference the interface hash, not only the
		// alias, or the collision problem returns through the back door."
		return fmt.Errorf("effect: capability %q references only an alias and no interface hash; the hash is the identity", c.ID)
	}
	if !cell.IsMultihash(c.Interface) {
		return fmt.Errorf("effect: capability %q names interface %q, which is not an object hash", c.ID, c.Interface)
	}
	if !c.CostModel.Valid() {
		return fmt.Errorf("effect: capability %q has unknown cost model %q", c.ID, c.CostModel)
	}
	return nil
}

// Matches reports whether this capability reference and another name the same
// interface. Comparison is on the **hash**, never the alias: two interfaces
// sharing an alias but not a hash are different interfaces, and an alias match
// with a hash mismatch is exactly the collision the hash binding exists to
// catch.
func (c Capability) Matches(other Capability) bool {
	return c.Interface != "" && c.Interface == other.Interface
}

// Priced reports whether this capability declares a cost model, and therefore
// whether a lease has to back an action against it (§7.0).
//
// The two halves of that sentence are the whole of the budget rule: a
// capability with a cost model needs a lease with headroom, and one without
// needs nothing. **`effectful` and `costs money` are orthogonal** — turning on
// a light, moving an arm, printing with filament already paid for, posting a
// message: every one irreversible, every one free. Requiring a lease for those
// would make an overseer and a budget the price of admission for a factory that
// spends nothing.
func (c Capability) Priced() bool { return c.CostModel.Priced() }

// Request is one proposed effectful action.
type Request struct {
	Capability Capability
	// Task is the varvig ticket id.
	Task string
	// Attempts is what the task asked for. Anything but 1 is rejected.
	Attempts int
	// Payload is the action's canonical parameters. It is hashed into the
	// idempotency key, so two requests differing in any parameter are different
	// actions and two identical ones are the same action.
	Payload any
	// Amount and Quantity are what this action will cost and order.
	Amount   cell.Money
	Quantity int64
	Unit     string
	// AuthorizedBy is the principal that authorized this action, which must be
	// a *different and higher* principal than the executing cell (§6.7 rule 3).
	AuthorizedBy string
}

// ErrSpeculation is returned when a task asks for more than one attempt at an
// effectful action.
//
// It is a **rejection, not a clamp** (§9.10). Silently clamping to 1 would
// deliver something other than what was asked for, on the single class of
// action where "not quite what you asked for" means a wrong order rather than a
// wasted GPU-hour. The task is wrong and its author needs to know.
var ErrSpeculation = errors.New("effect: an effectful capability cannot be speculated on")

// ErrSelfAuthorization is returned when a cell tries to authorize its own
// effectful action.
//
// A cell never authorizes its own effectful action, **even when it holds a
// factory key with promote** (§9.15). Promote rights move refs; they are not a
// licence to spend money, and conflating the two is how a scoped repository
// credential becomes a purchasing credential.
var ErrSelfAuthorization = errors.New("effect: a cell cannot authorize its own effectful action")

// IdempotencyKey derives the mandatory key from (task-id, capability-id, payload
// hash) — §6.7 rule 2.
//
// It is derived rather than supplied so that a retry after a network failure
// computes the same key from the same intent, without the caller having to
// remember one. That is the whole mechanism: the key is a function of what the
// action *is*, so "the same action" and "the same key" cannot drift apart.
//
// The payload goes through canonical JSON, so two payloads that differ only in
// map ordering are one action rather than two orders.
func IdempotencyKey(taskID string, c Capability, payload any) (string, error) {
	if taskID == "" {
		return "", errors.New("effect: idempotency key needs a task id")
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	canonical, err := cell.Canonical(payload)
	if err != nil {
		return "", fmt.Errorf("effect: canonicalizing payload: %w", err)
	}
	h := sha256.New()
	// Length-prefix each component. Without it, ("ab","c") and ("a","bc") would
	// hash identically, and two different actions would share a key — which for
	// this class means one of them silently never happens.
	for _, part := range [][]byte{[]byte(taskID), []byte(c.ID), []byte(c.Interface), canonical} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Decision is the outcome of checking a request. Refusals list every unmet
// rule, in declared order, so an operator fixing one is told about the rest now.
type Decision struct {
	Allowed bool
	// Key is the idempotency key, computed whenever the request was well-formed
	// enough to derive one — including on a refusal, so a caller can look up
	// whether the action already happened.
	Key      string
	Refusals []string
	// Escalate reports that a higher principal must decide.
	Escalate bool
}

func (d Decision) Error() string {
	if d.Allowed {
		return ""
	}
	return "effect: refused: " + strings.Join(d.Refusals, "; ")
}

// Check applies every §6.7 rule to a request.
//
// executingCell is the cell about to act; grant is the envelope and lease it
// acts under; sync is what it knows about the currency of trust state.
//
// hist is what the caller measured about this cell's recent actions against this
// capability; a rate ceiling needs it and cannot be derived from the request.
//
// The spend rules are evaluated against the **envelope-bounded** lease, not the
// lease as issued, so an overseer who tightened the envelope has the tighter
// ceiling honoured here before anything happens (§9.12).
//
// Rules 4 and 5 are separate checks and not a redundancy. Rule 4 bounds the
// action against the overseer's envelope and applies to every effectful action.
// Rule 5 spends from the cell's exclusive lease and applies **only** to a
// capability that declares a cost model. A free capability is bounded by the
// envelope's quantity and rate ceilings alone, which is why those had to start
// being enforced for §7.0 to be safe rather than merely permissive.
//
// The order is cheapest-and-most-decisive first, but every rule is evaluated:
// unlike the promotion path, where an early exit saves an expensive
// re-verification, nothing here is expensive and an operator about to spend
// money should see the full list.
func Check(req Request, executingCell string, grant authority.Grant, sync authority.Sync, now nowFunc, maxAge authority.MaxAge, hist authority.History) Decision {
	var d Decision

	if err := req.Capability.Validate(); err != nil {
		d.Refusals = append(d.Refusals, err.Error())
		// Without a valid capability there is no key to derive and no ceiling to
		// look up; the rest of the rules would be checking nothing.
		return d
	}
	if !req.Capability.Effectful {
		d.Refusals = append(d.Refusals,
			fmt.Sprintf("capability %q is not marked effectful; this path is only for actions that cannot be re-run", req.Capability.ID))
		return d
	}

	// Rule 1: attempts is forced to 1 — by rejection, not by clamping.
	if req.Attempts != 1 {
		d.Refusals = append(d.Refusals, fmt.Sprintf(
			"%v: the task asked for %d attempts at %s, and %d orders is what that would mean",
			ErrSpeculation, req.Attempts, req.Capability.ID, req.Attempts))
	}

	// Rule 2: the idempotency key is mandatory, regardless of promotion mode.
	key, err := IdempotencyKey(req.Task, req.Capability, req.Payload)
	if err != nil {
		d.Refusals = append(d.Refusals, err.Error())
	}
	d.Key = key

	// Rule 3: authorization by a higher principal, in both modes.
	switch {
	case req.AuthorizedBy == "":
		d.Escalate = true
		d.Refusals = append(d.Refusals,
			"no authorizing principal; an effectful action is authorized before execution in both gated and autonomous mode")
	case req.AuthorizedBy == executingCell:
		d.Escalate = true
		d.Refusals = append(d.Refusals, fmt.Sprintf("%v: %s cannot authorize itself", ErrSelfAuthorization, executingCell))
	}

	// The §7.0 guard: **an undeclared cost is a malformed capability, not a free
	// one.** Without this, "no cost model" would be the cheapest way to run
	// something unmetered — the guard and the permission arrive together or the
	// permission is a hole. A negative amount is refused in the same breath: it
	// is not a discount, it is a sign error, and it would pass every headroom
	// check by making the arithmetic run backwards.
	priced := req.Capability.Priced()
	if req.Amount < 0 {
		d.Refusals = append(d.Refusals, fmt.Sprintf(
			"this action reports a negative cost (%s %s) for %s; an amount below zero is a sign error, not a credit",
			req.Amount.In(req.Unit), req.Unit, req.Capability.ID))
	} else if req.Amount > 0 && !priced {
		d.Escalate = true
		d.Refusals = append(d.Refusals, fmt.Sprintf(
			"capability %q declares no cost model but this action costs %s %s; an undeclared cost is a malformed capability, not a free one",
			req.Capability.ID, req.Amount.In(req.Unit), req.Unit))
	}

	// Rule 4: bounded by the overseer's envelope — spend, quantity and rate.
	// This applies whether or not a lease is involved, and for a free capability
	// it is the only bound there is.
	if err := authority.PermitCeiling(grant.Envelope, req.Capability.ID, req.Unit, req.Amount, req.Quantity, hist); err != nil {
		var refusal authority.Refusal
		if errors.As(err, &refusal) && refusal.Escalate {
			d.Escalate = true
		}
		d.Refusals = append(d.Refusals, err.Error())
	}

	// Rule 5: spend comes from this cell's own lease — **if and only if** the
	// capability declares a cost model. Note the asymmetry §6.6 insists on: the
	// lease check needs no freshness, because an exclusive allocation was
	// already committed when it was issued.
	//
	// A free capability holding a lease anyway is not an error and not spent
	// from: nothing here draws on it, so it simply goes unused. That is the
	// state a factory passes through when a capability stops being priced.
	if priced {
		lease, boundErr := grant.Bounded()
		switch {
		case boundErr != nil:
			// A lease read without its envelope is a lease nobody has bounded.
			d.Escalate = true
			d.Refusals = append(d.Refusals, boundErr.Error())
		case lease != nil && lease.CellID != "" && lease.CellID != executingCell:
			d.Refusals = append(d.Refusals, fmt.Sprintf(
				"the lease for %s belongs to %s, not to %s; spend comes from the acting cell's own lease",
				req.Capability.ID, lease.CellID, executingCell))
		default:
			if err := authority.PermitSpend(sync, now(), maxAge, lease, req.Amount, req.Quantity); err != nil {
				var refusal authority.Refusal
				if errors.As(err, &refusal) && refusal.Escalate {
					d.Escalate = true
				}
				d.Refusals = append(d.Refusals, err.Error())
			}
			if lease != nil && lease.Unit != "" && req.Unit != "" && lease.Unit != req.Unit {
				d.Refusals = append(d.Refusals, fmt.Sprintf(
					"this action is priced in %q but the lease for %s is in %q; comparing them would be arithmetic on unlike units",
					req.Unit, req.Capability.ID, lease.Unit))
			}
		}
	}

	// A quoted capability cannot be priced offline. This is stated as an
	// observation rather than enforced as a rule: §6.6 is explicit that one
	// should not write a rule forbidding what is already impossible. It is
	// surfaced so a refusal from the external service is legible when it comes.
	if req.Capability.CostModel == CostQuoted && sync.Configured && !sync.Reachable {
		d.Refusals = append(d.Refusals, fmt.Sprintf(
			"capability %q is quoted, so its price comes from an external service that is not reachable now; this is a property of the capability, not a policy", req.Capability.ID))
	}

	d.Allowed = len(d.Refusals) == 0
	return d
}

// nowFunc lets a caller supply the clock. It is a parameter rather than a
// package variable so two cells in one process cannot disagree about the time.
type nowFunc func() time.Time
