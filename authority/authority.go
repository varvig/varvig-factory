// Package authority implements the allocation model: who may authorize what,
// and with how much (FACTORY.md §6.6, varvig-auth-and-api.md §4.3b).
//
// # The one idea
//
// Everything here follows from a single distinction, and the spec states it
// plainly: **the distinction that matters is shared versus exclusive authority,
// not fresh versus stale.**
//
//	An envelope is a shared ceiling.  It cannot be enforced locally, so acting
//	on it requires current state. If the envelope says €5,000/month and three
//	cells each hold it, a partition lets each spend €5,000 — and every one of
//	them is "within an agreed budget".
//
//	A lease is an exclusive allocation drawn from an envelope. No other cell can
//	spend it, so the amount was already committed when it was issued. Acting on
//	a stale lease is therefore safe by construction, offline, indefinitely.
//
// Freshness is a consequence of sharing, not a value in itself. That is why
// this package has no TTL on membership and no renewal protocol: expiry assumes
// renewal is cheap, which contradicts supporting cells that are offline for
// weeks.
//
// # Why none of this needs varvig to change
//
// Envelopes, leases and reservations are all **refs**. varvig already moves a ref
// only by signed compare-and-swap, logs every move in the reflog, and replicates
// refs between peers. So "the owner sets the ceiling" is an ordinary signed ref
// update, and "what was the ceiling when this decision was made" is answered from
// ref history. No new primitive, no expiry ceremony.
//
// The one thing a cell cannot do is widen its own envelope. That is not enforced
// here — it is enforced by the trust store, because only the owner's key may move
// that ref. Factory checks it anyway (§6.7 rule 3) so a misconfiguration fails
// locally with a clear reason rather than as a confusing rejection from a peer.
package authority

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

// Envelope is an overseer's ceiling, per capability. It lives at EnvelopeRef and
// only the owner key may move that ref.
//
// Contents are **ceilings, not permissions to spend** — limits on spending. The
// difference matters when reading the numbers: an envelope of €5,000 is not a
// budget that has been granted, it is the most that may ever be allocated out of
// it.
type Envelope struct {
	// Overseer is the principal this envelope bounds.
	Overseer string `json:"overseer"`
	// Ceilings are the per-capability limits.
	Ceilings []Ceiling `json:"ceilings"`
	// SetAt is when the owner last moved this ref.
	SetAt int64 `json:"set_at"`
}

// Ceiling is one capability's limit.
//
// Three dimensions, because they fail differently: a spend cap stops a single
// expensive mistake, a quantity cap stops a units-confusion mistake (ordering
// 1,000 boards instead of 10 costs the same per board), and a rate cap stops a
// loop that is individually within both.
type Ceiling struct {
	// Capability is the interface this bounds, e.g. "pcb-fabrication@1".
	Capability string `json:"capability"`
	// Spend is the maximum total allocatable, in Unit.
	Spend float64 `json:"spend,omitempty"`
	Unit  string  `json:"unit,omitempty"`
	// Quantity is the maximum number of units orderable.
	Quantity int64 `json:"quantity,omitempty"`
	// RatePerDay caps actions per day.
	RatePerDay int64 `json:"rate_per_day,omitempty"`
}

// Validate rejects an envelope that cannot bound anything.
func (e Envelope) Validate() error {
	if err := cell.CheckID(e.Overseer); err != nil {
		return fmt.Errorf("authority: envelope: %w", err)
	}
	if len(e.Ceilings) == 0 {
		// An envelope with no ceilings is not an unlimited envelope, it is a
		// malformed one. Reading it as unlimited is the single worst default
		// available here.
		return fmt.Errorf("authority: envelope for %s declares no ceilings; an empty envelope is not an unlimited one", e.Overseer)
	}
	seen := map[string]bool{}
	for _, c := range e.Ceilings {
		if c.Capability == "" {
			return fmt.Errorf("authority: envelope for %s has a ceiling with no capability", e.Overseer)
		}
		if seen[c.Capability] {
			// Two ceilings for one capability leaves "which applies?" to
			// whichever code reads it first.
			return fmt.Errorf("authority: envelope for %s declares %q twice", e.Overseer, c.Capability)
		}
		seen[c.Capability] = true
		if c.Spend < 0 || c.Quantity < 0 || c.RatePerDay < 0 {
			return fmt.Errorf("authority: envelope for %s has a negative ceiling on %q", e.Overseer, c.Capability)
		}
		if c.Spend > 0 && c.Unit == "" {
			return fmt.Errorf("authority: ceiling on %q names a spend of %g with no unit", c.Capability, c.Spend)
		}
	}
	return nil
}

// Ceiling returns the ceiling for a capability.
//
// A capability with no ceiling is **not allowed**, and the boolean says so
// rather than a zero Ceiling being returned as though it were a limit of zero.
// An unlisted capability is one the owner never considered; treating silence as
// permission is how an envelope stops bounding anything.
func (e Envelope) Ceiling(capability string) (Ceiling, bool) {
	for _, c := range e.Ceilings {
		if c.Capability == capability {
			return c, true
		}
	}
	return Ceiling{}, false
}

// Lease is a signed, non-overlapping allocation drawn from an envelope, held by
// exactly one cell for exactly one capability. It lives at LeaseRef.
//
// Because no other cell can spend it, a stale lease is entirely safe to act on —
// which is the whole point. A cell spends its lease down offline for as long as
// it takes, settles actuals on reconnect, and is replenished, or is not.
type Lease struct {
	CellID     string `json:"cell_id"`
	Capability string `json:"capability"`
	// Overseer is who issued it, and Envelope is the envelope ref's value at
	// issue time — so an audit can name the ceiling that was in force without
	// trusting a later read of a ref that has since moved.
	Overseer string `json:"overseer"`
	Envelope string `json:"envelope"`
	// Amount is the exclusive allocation in Unit; Quantity, when set, is the
	// unit count allowed.
	Amount   float64 `json:"amount"`
	Unit     string  `json:"unit,omitempty"`
	Quantity int64   `json:"quantity,omitempty"`
	// Spent and Ordered are settled actuals, updated when the cell settles.
	Spent    float64 `json:"spent,omitempty"`
	Ordered  int64   `json:"ordered,omitempty"`
	IssuedAt int64   `json:"issued_at"`
	// ReclaimAfter is the stated timeout after which an unspent lease may be
	// *provisionally* reclaimed (§6.6, stranded leases). It is not an expiry:
	// the lease stays spendable by its holder past this point, because a cell
	// that placed an order it has not yet reported must not have the budget for
	// it pulled out from under it.
	ReclaimAfter int64 `json:"reclaim_after,omitempty"`
}

// Validate rejects a lease that cannot bound anything.
func (l Lease) Validate() error {
	if err := cell.CheckID(l.CellID); err != nil {
		return fmt.Errorf("authority: lease: %w", err)
	}
	if l.Capability == "" {
		return fmt.Errorf("authority: lease for %s names no capability", l.CellID)
	}
	if err := cell.CheckID(l.Overseer); err != nil {
		return fmt.Errorf("authority: lease for %s: overseer: %w", l.CellID, err)
	}
	if l.Amount < 0 || l.Quantity < 0 || l.Spent < 0 || l.Ordered < 0 {
		return fmt.Errorf("authority: lease for %s/%s has a negative amount", l.CellID, l.Capability)
	}
	if l.Amount > 0 && l.Unit == "" {
		return fmt.Errorf("authority: lease for %s/%s allocates %g with no unit", l.CellID, l.Capability, l.Amount)
	}
	if l.Spent > l.Amount {
		// Overspend is not a state to tolerate quietly: it means either a
		// settlement bug or an action taken outside the lease.
		return fmt.Errorf("authority: lease for %s/%s has spent %g of %g", l.CellID, l.Capability, l.Spent, l.Amount)
	}
	if l.Ordered > l.Quantity && l.Quantity > 0 {
		return fmt.Errorf("authority: lease for %s/%s has ordered %d of %d", l.CellID, l.Capability, l.Ordered, l.Quantity)
	}
	return nil
}

// Headroom is what remains spendable on this lease.
func (l Lease) Headroom() float64 {
	if l.Spent >= l.Amount {
		return 0
	}
	return l.Amount - l.Spent
}

// QuantityHeadroom is the remaining unit count, or -1 when no quantity ceiling
// was allocated. -1 rather than zero, because "no quantity limit on this lease"
// and "no units left" are opposite answers and must not share a value.
func (l Lease) QuantityHeadroom() int64 {
	if l.Quantity <= 0 {
		return -1
	}
	if l.Ordered >= l.Quantity {
		return 0
	}
	return l.Quantity - l.Ordered
}

// Reclaimable reports whether an unspent lease has passed its stated timeout.
//
// Reclaim is **provisional until the cell confirms** (§6.6). A unilateral
// reclaim risks a double-spend: the cell may already have placed an order it has
// not yet reported. So this answers "may an overseer begin reclaiming?", never
// "is this lease void?".
func (l Lease) Reclaimable(now time.Time) bool {
	return l.ReclaimAfter > 0 && now.Unix() > l.ReclaimAfter && l.Spent == 0
}

// Exhausted reports whether the lease has nothing left to spend.
func (l Lease) Exhausted() bool {
	if l.Amount > 0 && l.Headroom() <= 0 {
		return true
	}
	return l.Quantity > 0 && l.QuantityHeadroom() <= 0
}

// CheckExclusive verifies the two invariants that make leases safe to spend
// offline (FACTORY.md §9.13b): no two cells hold a lease for the same
// capability, and the sum of outstanding leases never exceeds the envelope.
//
// Both are checked together because either alone is insufficient. Non-overlap
// without a sum check lets one cell hold more than the whole envelope; a sum
// check without non-overlap lets two cells each hold half and both believe they
// hold it exclusively.
func CheckExclusive(env Envelope, leases []Lease) error {
	if err := env.Validate(); err != nil {
		return err
	}
	byCapability := map[string][]Lease{}
	for _, l := range leases {
		if err := l.Validate(); err != nil {
			return err
		}
		if l.Overseer != env.Overseer {
			return fmt.Errorf("authority: lease for %s/%s was issued by %s, not by this envelope's overseer %s",
				l.CellID, l.Capability, l.Overseer, env.Overseer)
		}
		byCapability[l.Capability] = append(byCapability[l.Capability], l)
	}

	capabilities := make([]string, 0, len(byCapability))
	for c := range byCapability {
		capabilities = append(capabilities, c)
	}
	sort.Strings(capabilities)

	for _, capability := range capabilities {
		held := byCapability[capability]
		ceiling, ok := env.Ceiling(capability)
		if !ok {
			return fmt.Errorf("authority: %d lease(s) allocate %q, which this envelope does not bound",
				len(held), capability)
		}

		cells := map[string]bool{}
		var totalSpend float64
		var totalQuantity int64
		for _, l := range held {
			if cells[l.CellID] {
				return fmt.Errorf("authority: cell %s holds two leases for %q; a lease is exclusive, so a second one is not an increase but an ambiguity",
					l.CellID, capability)
			}
			cells[l.CellID] = true
			totalSpend += l.Amount
			totalQuantity += l.Quantity
			if l.Unit != "" && ceiling.Unit != "" && l.Unit != ceiling.Unit {
				return fmt.Errorf("authority: lease for %s/%s is in %q but the ceiling is in %q; comparing them would be arithmetic on unlike units",
					l.CellID, capability, l.Unit, ceiling.Unit)
			}
		}
		if ceiling.Spend > 0 && totalSpend > ceiling.Spend {
			return fmt.Errorf("authority: outstanding leases for %q total %g %s against a ceiling of %g %s; the owner's exposure would exceed what they set",
				capability, totalSpend, ceiling.Unit, ceiling.Spend, ceiling.Unit)
		}
		if ceiling.Quantity > 0 && totalQuantity > ceiling.Quantity {
			return fmt.Errorf("authority: outstanding leases for %q total %d units against a ceiling of %d",
				capability, totalQuantity, ceiling.Quantity)
		}
	}
	return nil
}

// Exposure is the owner's maximum exposure: the sum of outstanding leases, per
// capability.
//
// This — not the envelope — is the number to reason about (§6.6). The envelope
// is what *could* be allocated; the sum of leases is what has been, and what
// cannot be clawed back without reaching each cell.
func Exposure(leases []Lease) map[string]float64 {
	out := map[string]float64{}
	for _, l := range leases {
		out[l.Capability] += l.Headroom()
	}
	return out
}

// String renders a lease for an operator.
func (l Lease) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s: %g of %g %s", l.CellID, l.Capability, l.Spent, l.Amount, l.Unit)
	if l.Quantity > 0 {
		fmt.Fprintf(&b, ", %d of %d units", l.Ordered, l.Quantity)
	}
	if l.Exhausted() {
		b.WriteString(" (exhausted)")
	}
	return b.String()
}
