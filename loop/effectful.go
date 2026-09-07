package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/effect"
)

// The effectful branch of the loop (§6.7, §7.1).
//
// This is the only path in Factory that reaches outside the repository and
// spends money, and it is deliberately shaped nothing like the attempt path
// beside it. There is no speculation, no scoring, no promotion and no retry: one
// action happens, or none does, and either way the record says which.
//
// The lifecycle is the spec's, in its order:
//
//	quote → reserve → authorize → execute → settle
//
// Authorization is checked before the reservation rather than between it and
// execution, because a refusal that arrives after the key is claimed has already
// consumed the cell's one chance to act on this ticket.

// EffectResult is what one effectful action did. It is reported whether or not
// the action happened — a refusal and an unknown outcome are both results, and a
// pass that silently produced neither an order nor an explanation would be the
// worst outcome available.
type EffectResult struct {
	Task       string
	Capability string
	// Key is the idempotency key, present even on a refusal, so an operator can
	// look up whether this action ever happened.
	Key string
	// Done reports that the effect is confirmed to have occurred.
	Done bool
	// ExternalRef is the far end's identifier, when there is one.
	ExternalRef string
	// Unresolved reports the one state a cell cannot settle alone: the action
	// may have happened and nobody knows. It escalates.
	Unresolved bool
	// Refused, with Reason, means nothing happened and why.
	Refused bool
	Reason  string
	Amount  float64
	Unit    string
}

func (r EffectResult) String() string {
	switch {
	case r.Done:
		return fmt.Sprintf("%s %s: done ref=%s (%.2f %s)", shortID(r.Task), r.Capability, r.ExternalRef, r.Amount, r.Unit)
	case r.Unresolved:
		return fmt.Sprintf("%s %s: UNRESOLVED — may have happened, escalating: %s", shortID(r.Task), r.Capability, r.Reason)
	default:
		return fmt.Sprintf("%s %s: refused: %s", shortID(r.Task), r.Capability, r.Reason)
	}
}

// effectGrants is what this cell can actually act on: for each configured
// capability, a lease it holds and an executor that supports it.
//
// Both halves are required, and the claim policy is given the intersection
// rather than either half, because a lease with nothing to execute it and an
// executor with no lease are equally unable to place an order — and a cell that
// claimed a ticket on the strength of one of them would find out only after
// taking the claim.
func (c *Cell) effectGrants() []claim.EffectGrant {
	var out []claim.EffectGrant
	for _, cap := range c.Capabilities.Effects {
		capability := effect.Capability{ID: cap.ID, Interface: cap.Interface, Effectful: true}
		if _, err := c.Executors.For(capability); err != nil {
			continue
		}
		if _, _, err := authority.LoadLease(c.V, c.Capabilities.CellID, cap.ID); err != nil {
			continue
		}
		out = append(out, claim.EffectGrant{Capability: cap.ID, Interface: cap.Interface})
	}
	return out
}

// performEffect runs the lifecycle for one ticket.
//
// It returns a result rather than an error for anything the operator needs to
// see, and an error only for a failure to record — because a failure to write
// the record of an action that may have happened is the one thing that must stop
// the pass rather than be counted.
func (c *Cell) performEffect(ctx context.Context, t claim.Ticket) (EffectResult, error) {
	req := t.Requirements()
	e := *req.Effect
	res := EffectResult{Task: t.ID, Capability: e.Capability}

	capability := effect.Capability{ID: e.Capability, Interface: e.Interface, Effectful: true}
	executor, err := c.Executors.For(capability)
	if err != nil {
		return refused(res, err.Error()), nil
	}

	var payload any
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		return refused(res, fmt.Sprintf("the ticket's parameters are not JSON: %v", err)), nil
	}

	lease, leaseHash, err := authority.LoadLease(c.V, c.Capabilities.CellID, e.Capability)
	if err != nil {
		return refused(res, fmt.Sprintf("no lease for %s: %v", e.Capability, err)), nil
	}
	envelope, _, err := authority.LoadEnvelope(c.V, lease.Overseer)
	if err != nil {
		// No readable envelope means nothing establishes that the overseer still
		// stands behind this spend. Refusing is the only safe reading.
		return refused(res, fmt.Sprintf("cannot read overseer %s's envelope: %v", lease.Overseer, err)), nil
	}
	grant := authority.Grant{Envelope: envelope, Lease: &lease, LeaseHash: leaseHash}

	action := effect.Request{
		Capability:   capability,
		Task:         t.ID,
		Attempts:     1,
		Payload:      payload,
		AuthorizedBy: c.EffectAuthorizedBy,
	}

	// Quote. It must not cause an effect, so it runs before anything is held —
	// and for a fixed-price capability it is still where the amount comes from,
	// which keeps one path rather than two.
	quote, err := executor.Quote(ctx, action)
	if err != nil {
		return refused(res, fmt.Sprintf("could not price %s: %v", e.Capability, err)), nil
	}
	action.Amount, action.Quantity, action.Unit = quote.Amount, quote.Quantity, quote.Unit
	res.Amount, res.Unit = quote.Amount, quote.Unit

	// Authorize, against the quoted amount and the envelope-bounded lease. Every
	// unmet rule is reported at once: an operator about to spend money should see
	// the whole list rather than one round trip per broken rule.
	decision := effect.Check(action, c.Capabilities.CellID, grant, c.sync, c.now, c.MaxTrustAge)
	res.Key = decision.Key
	if !decision.Allowed {
		return refused(res, decision.Error()), nil
	}

	// Reserve. This claims the key and holds the headroom, and from here the
	// cell owns the outcome.
	claimed, err := effect.Reserve(c.V, action, c.Capabilities.CellID, grant, c.now().Unix(), c.EffectTTL)
	if err != nil {
		if errors.Is(err, effect.ErrAlreadyReserved) {
			// Not a failure: somebody already did this, possibly this cell before
			// a restart. Report what happened rather than doing it again.
			res.Key = claimed.Reservation.Key
			res.Done = claimed.Reservation.State == effect.StateDone
			res.ExternalRef = claimed.Reservation.ExternalRef
			res.Unresolved = claimed.Reservation.Unresolved()
			res.Refused = !res.Done
			res.Reason = err.Error()
			return res, nil
		}
		return refused(res, err.Error()), nil
	}
	res.Key = claimed.Reservation.Key

	// Execute. Everything after this point is about recording what happened,
	// because the money may already be gone.
	outcome, execErr := executor.Execute(ctx, action, claimed.Reservation.Key)

	switch {
	case execErr == nil:
		settled, err := effect.Settle(c.V, claimed, outcome.ExternalRef, outcome.Actual, c.now().Unix())
		if err != nil {
			// The effect happened and the record did not. This is the failure
			// that must not be swallowed: the lease still holds rather than
			// spends, and the reservation still reads pending, so the next pass
			// and the operator both see an unresolved action rather than a clean
			// slate.
			return res, fmt.Errorf("%s: the action succeeded (%s) but settlement failed, so the record is now wrong: %w",
				shortID(t.ID), outcome.ExternalRef, err)
		}
		res.Done, res.ExternalRef = true, outcome.ExternalRef
		if settled.Reservation.Actual > 0 {
			res.Amount = settled.Reservation.Actual
		}
		return res, c.recordEffect(t, settled.Reservation)

	case errors.Is(execErr, effect.ErrRejected):
		// A definite rejection: no effect occurred, so the hold comes back. The
		// key stays claimed, because whether to try again is a decision for a
		// higher principal and not a loop behaviour.
		failed, err := effect.Fail(c.V, claimed, execErr.Error(), c.now().Unix())
		if err != nil {
			return res, fmt.Errorf("%s: recording a rejection: %w", shortID(t.ID), err)
		}
		return refused(res, execErr.Error()), c.recordEffect(t, failed.Reservation)

	default:
		// Anything else means the outcome is unknown — a timeout, a dropped
		// connection and a 500 are all consistent with the order having been
		// placed. The reservation stays pending, which is the honest record, and
		// this escalates.
		res.Unresolved, res.Reason = true, execErr.Error()
		c.logf("UNRESOLVED %s %s: %v — the action may have happened; a higher principal must check %s",
			shortID(t.ID), e.Capability, execErr, res.Key)
		return res, c.recordEffect(t, claimed.Reservation)
	}
}

func refused(res EffectResult, reason string) EffectResult {
	res.Refused, res.Reason = true, reason
	return res
}

// recordEffect attaches the reservation to the ticket as a note, so what a cell
// spent is visible where the work is, not only under a reservation ref an
// operator has to know to look for.
func (c *Cell) recordEffect(t claim.Ticket, r effect.Reservation) error {
	payload, err := cell.Canonical(r)
	if err != nil {
		return err
	}
	if err := c.V.AddNote(t.Object, cell.NoteEffect, payload); err != nil {
		return fmt.Errorf("recording the effect on %s: %w", shortID(t.ID), err)
	}
	return nil
}

// priorEffect reports whether this cell has already acted on an effectful
// ticket, by looking for the reservation its parameters would derive.
//
// Without this the claim policy has nothing to go on: an effectful action writes
// no attempt ref, so `OwnAttempts` stays zero and every pass re-claims the same
// ticket. The reservation ref would still refuse the second order — that is the
// guarantee — but only after the cell had re-quoted, which for a quoted
// capability means calling the vendor's pricing API on every pass, forever.
func (c *Cell) priorEffect(t claim.Ticket) int {
	req := t.Requirements()
	if !req.Effectful() || req.Effect.Malformed != "" {
		return 0
	}
	var payload any
	if err := json.Unmarshal([]byte(req.Effect.Payload), &payload); err != nil {
		return 0
	}
	key, err := effect.IdempotencyKey(t.ID, effect.Capability{
		ID: req.Effect.Capability, Interface: req.Effect.Interface, Effectful: true,
	}, payload)
	if err != nil {
		return 0
	}
	name, err := cell.ReservationRef(c.Capabilities.CellID, key)
	if err != nil {
		return 0
	}
	if _, err := c.V.ResolveRef(name); err != nil {
		// No reservation, or the repository cannot be read. Either way this is
		// only an optimisation: answering "no prior action" is safe, because the
		// create-only claim is what actually prevents the second order.
		return 0
	}
	return 1
}
