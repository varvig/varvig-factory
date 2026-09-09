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

	"github.com/varvig/varvig-factory/iface"
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
	// Offered means the reservation is waiting for a connector to take it. The
	// cell has committed the headroom and done everything it can; the effect has
	// not happened yet.
	Offered bool
	Amount  cell.Money
	Unit    string
}

func (r EffectResult) String() string {
	switch {
	case r.Done:
		return fmt.Sprintf("%s %s: done ref=%s (%s %s)", shortID(r.Task), r.Capability, r.ExternalRef, r.Amount.In(r.Unit), r.Unit)
	case r.Offered:
		return fmt.Sprintf("%s %s: offered to a connector (%s %s held)", shortID(r.Task), r.Capability, r.Amount.In(r.Unit), r.Unit)
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
		capability := effect.Capability{ID: cap.ID, Interface: cap.Interface, Effectful: true, CostModel: cap.CostModel}
		if _, err := c.Executors.For(capability); err != nil {
			continue
		}
		// A lease is required only for a capability that declares a cost model
		// (§7.0). Requiring one for a free effect would make an overseer and a
		// budget the price of turning on a light, and would leave a factory
		// that spends nothing unable to act at all.
		if capability.Priced() {
			if _, _, err := authority.LoadLease(c.Factory, c.Capabilities.CellID, cap.ID); err != nil {
				continue
			}
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

	// The capability's terms come from **this cell's configuration**, and only
	// its identity from the ticket. A ticket says what it wants done; an
	// operator says what doing it costs. Reading the cost model off the request
	// would let a ticket declare a priced capability free and walk past the
	// lease check entirely.
	configured, ok := c.Capabilities.Effect(e.Capability)
	if !ok {
		return refused(res, fmt.Sprintf("this cell declares no effectful capability %q", e.Capability)), nil
	}
	capability := effect.Capability{
		ID: e.Capability, Interface: e.Interface, Effectful: true,
		CostModel: configured.CostModel,
	}

	// The interface must resolve in the registry before anything else happens.
	//
	// A hash is enough to tell two interfaces apart, which is what the matching
	// rules need, and not enough to know what the action requires. Acting on a
	// hash nothing in the factory has ever published means placing an order
	// whose shape no one here can describe — and the moment to find that out is
	// before the money, not in the invoice.
	if !iface.Known(c.Factory, capability.Interface) {
		return refused(res, fmt.Sprintf(
			"interface %s is not in this factory's registry; a capability whose interface nobody published is one nothing here can describe",
			shortID(capability.Interface))), nil
	}

	executor, err := c.Executors.For(capability)
	if err != nil {
		return refused(res, err.Error()), nil
	}

	var payload any
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		return refused(res, fmt.Sprintf("the ticket's parameters are not JSON: %v", err)), nil
	}

	// A priced capability needs its lease and the envelope that bounds it. A
	// free one needs neither, and where an overseer is configured its ceilings
	// still apply — through the envelope named by c.EffectOverseer, since with
	// no lease there is nothing else to name one.
	var grant authority.Grant
	if capability.Priced() {
		lease, leaseHash, err := authority.LoadLease(c.Factory, c.Capabilities.CellID, e.Capability)
		if err != nil {
			return refused(res, fmt.Sprintf("no lease for %s: %v", e.Capability, err)), nil
		}
		envelope, _, err := authority.LoadEnvelope(c.Factory, lease.Overseer)
		if err != nil {
			// No readable envelope means nothing establishes that the overseer
			// still stands behind this spend. Refusing is the only safe reading.
			return refused(res, fmt.Sprintf("cannot read overseer %s's envelope: %v", lease.Overseer, err)), nil
		}
		grant = authority.Grant{Envelope: envelope, Lease: &lease, LeaseHash: leaseHash}
	} else if c.EffectOverseer != "" {
		envelope, _, err := authority.LoadEnvelope(c.Factory, c.EffectOverseer)
		if err != nil {
			return refused(res, fmt.Sprintf("cannot read overseer %s's envelope: %v", c.EffectOverseer, err)), nil
		}
		grant = authority.Grant{Envelope: envelope}
	}

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

	// Measure the rate history before deciding, because a rate ceiling is the
	// one bound that cannot be answered from the request alone — and for a free
	// capability it is very likely the only bound there is. An unreadable
	// reservation makes the count untrustworthy rather than low, so the error
	// refuses instead of passing a short count off as a measurement.
	hist := authority.History{}
	if grant.Envelope.Configured() {
		taken, err := effect.ActionsToday(c.Project, c.Capabilities.CellID, e.Capability, c.now().Unix())
		if err != nil {
			return refused(res, fmt.Sprintf("cannot measure today's %s actions, so a rate ceiling cannot be honoured: %v", e.Capability, err)), nil
		}
		hist = authority.History{ActionsToday: taken, Measured: true}
	}

	// Authorize, against the quoted amount and the envelope-bounded lease. Every
	// unmet rule is reported at once: an operator about to spend money should see
	// the whole list rather than one round trip per broken rule.
	decision := effect.Check(action, c.Capabilities.CellID, grant, c.sync, c.now, c.MaxTrustAge, hist)
	res.Key = decision.Key
	if !decision.Allowed {
		return refused(res, decision.Error()), nil
	}

	// Reserve. This claims the key and holds the headroom, and from here the
	// cell owns the outcome.
	claimed, err := effect.Reserve(c.Factory, c.Project, action, c.Capabilities.CellID, grant, c.now().Unix(), c.EffectTTL)
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

	// If a connector serves this capability, the cell's part is done: the offer
	// stands in the repository and whichever connector holds credentials for the
	// vendor takes it. The cell settles the report on a later pass.
	if c.Connectors[capability.Interface] {
		offered, err := effect.Offer(c.Project, claimed, c.now().Unix()+c.EffectTTL)
		if err != nil {
			return res, fmt.Errorf("%s: offering to a connector: %w", shortID(t.ID), err)
		}
		res.Offered = true
		res.Reason = fmt.Sprintf("offered to a connector for %s", e.Capability)
		return res, c.recordEffect(t, offered.Reservation)
	}

	// In-process: the cell takes its own reservation before acting, so there is
	// one answer to "who holds this" whichever path produced it.
	claimed, err = effect.TakeSelf(c.Project, claimed, c.now().Unix())
	if err != nil {
		return res, fmt.Errorf("%s: taking its own reservation: %w", shortID(t.ID), err)
	}

	// Execute. Everything after this point is about recording what happened,
	// because the money may already be gone.
	outcome, execErr := executor.Execute(ctx, action, claimed.Reservation.Key)

	switch {
	case execErr == nil:
		settled, err := effect.Settle(c.Factory, c.Project, claimed, outcome.ExternalRef, outcome.Actual, c.now().Unix())
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
		failed, err := effect.Fail(c.Factory, c.Project, claimed, execErr.Error(), c.now().Unix())
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
	if err := c.Project.AddNote(t.Object, cell.NoteEffect, payload); err != nil {
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
	if _, err := c.Project.ResolveRef(name); err != nil {
		// No reservation, or the repository cannot be read. Either way this is
		// only an optimisation: answering "no prior action" is safe, because the
		// create-only claim is what actually prevents the second order.
		return 0
	}
	return 1
}

// settleReports applies connector reports to the leases they draw on.
//
// This is the cell's half of the connector exchange, and it runs every pass
// rather than only after offering: a report may land long after the pass that
// offered it, and it may land while the cell is restarting. The state is in the
// repository, so picking it up later is the normal case rather than recovery.
//
// A report is a claim about the world made by an untrusted peer. What bounds it
// is the lease — Convert refuses an actual beyond the allocation — so a
// connector reporting a wild figure costs at most an amount the overseer chose.
func (c *Cell) settleReports() ([]EffectResult, []string) {
	reported, err := effect.Reported(c.Project, c.Capabilities.CellID)
	if err != nil {
		return nil, []string{"reading connector reports: " + err.Error()}
	}

	var out []EffectResult
	var errs []string
	for _, r := range reported {
		res := EffectResult{
			Task: r.Task, Capability: r.Capability, Key: r.Key,
			Amount: r.Amount, Unit: r.Unit,
		}
		lease, leaseHash, err := authority.LoadLease(c.Factory, c.Capabilities.CellID, r.Capability)
		if err != nil {
			// The report stands and the money is still held; the next pass tries
			// again. Not settling is safe, and inventing a lease would not be.
			errs = append(errs, fmt.Sprintf("settling %s: no lease: %v", short(r.Key), err))
			continue
		}
		claim, err := effect.LoadClaim(c.Project, c.Capabilities.CellID, r.Key, lease, leaseHash)
		if err != nil {
			errs = append(errs, fmt.Sprintf("settling %s: %v", short(r.Key), err))
			continue
		}
		settled, err := effect.SettleReported(c.Factory, c.Project, claim, c.now().Unix())
		if err != nil {
			errs = append(errs, fmt.Sprintf("settling %s: %v", short(r.Key), err))
			continue
		}
		res.Done = settled.Reservation.State == effect.StateDone
		res.ExternalRef = settled.Reservation.ExternalRef
		res.Refused = !res.Done
		res.Reason = settled.Reservation.Detail
		if settled.Reservation.Actual > 0 {
			res.Amount = settled.Reservation.Actual
		}
		out = append(out, res)
		c.logf("settled a connector report: %s", res)
	}
	return out, errs
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}
