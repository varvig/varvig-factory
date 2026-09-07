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
	Spent   float64 `json:"spent,omitempty"`
	Ordered int64   `json:"ordered,omitempty"`
	// Reserved and ReservedUnits are headroom held by reservations that have not
	// settled yet (§7.1). They are what stops two pending orders from each
	// passing the headroom check on their own and together exceeding the lease.
	//
	// Held is not the same as spent: a hold is released if the action is
	// definitely rejected, or if the reservation expires without an answer. Only
	// settlement converts a hold into spend.
	Reserved      float64 `json:"reserved,omitempty"`
	ReservedUnits int64   `json:"reserved_units,omitempty"`
	IssuedAt      int64   `json:"issued_at"`
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
	if l.Amount < 0 || l.Quantity < 0 || l.Spent < 0 || l.Ordered < 0 || l.Reserved < 0 || l.ReservedUnits < 0 {
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
	if l.Spent+l.Reserved > l.Amount && l.Amount > 0 {
		// Over-commitment: settled spend plus held headroom exceeds the lease.
		// It means a hold was taken without checking, or a settlement recorded
		// an actual larger than its hold without the hold being adjusted.
		return fmt.Errorf("authority: lease for %s/%s has committed %g of %g (%g spent, %g held by reservations)",
			l.CellID, l.Capability, l.Spent+l.Reserved, l.Amount, l.Spent, l.Reserved)
	}
	if l.Quantity > 0 && l.Ordered+l.ReservedUnits > l.Quantity {
		return fmt.Errorf("authority: lease for %s/%s has committed %d of %d units (%d ordered, %d held)",
			l.CellID, l.Capability, l.Ordered+l.ReservedUnits, l.Quantity, l.Ordered, l.ReservedUnits)
	}
	return nil
}

// Headroom is what remains spendable on this lease: the allocation less what has
// settled and less what pending reservations are holding.
//
// Held headroom is subtracted because a pending reservation may already have
// become a real order at the far end. Treating it as still available is exactly
// the double-spend the reservation exists to prevent.
func (l Lease) Headroom() float64 {
	if committed := l.Spent + l.Reserved; committed < l.Amount {
		return l.Amount - committed
	}
	return 0
}

// QuantityHeadroom is the remaining unit count, or -1 when no quantity ceiling
// was allocated. -1 rather than zero, because "no quantity limit on this lease"
// and "no units left" are opposite answers and must not share a value.
func (l Lease) QuantityHeadroom() int64 {
	if l.Quantity <= 0 {
		return -1
	}
	if committed := l.Ordered + l.ReservedUnits; committed < l.Quantity {
		return l.Quantity - committed
	}
	return 0
}

// Reclaimable reports whether an unspent lease has passed its stated timeout.
//
// Reclaim is **provisional until the cell confirms** (§6.6). A unilateral
// reclaim risks a double-spend: the cell may already have placed an order it has
// not yet reported. So this answers "may an overseer begin reclaiming?", never
// "is this lease void?".
func (l Lease) Reclaimable(now time.Time) bool {
	// Held headroom counts against reclaim for the same reason spend does, and
	// more sharply: a hold means an action may be in flight right now.
	return l.ReclaimAfter > 0 && now.Unix() > l.ReclaimAfter &&
		l.Spent == 0 && l.Ordered == 0 && l.Reserved == 0 && l.ReservedUnits == 0
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

// Hold reserves headroom on a lease ahead of an effectful action, returning the
// updated lease. It refuses when the hold would exceed what is left.
//
// Holding before acting is what makes two pending actions safe. Without it each
// would check the same headroom, pass, and together exceed the lease — and by
// the time the second invoice arrives the money is gone.
func (l Lease) Hold(amount float64, quantity int64) (Lease, error) {
	if amount < 0 || quantity < 0 {
		return l, fmt.Errorf("authority: cannot hold a negative amount on %s/%s", l.CellID, l.Capability)
	}
	if amount > 0 && amount > l.Headroom() {
		return l, fmt.Errorf("authority: holding %g %s on the lease for %s/%s leaves %g; only %g is available",
			amount, l.Unit, l.CellID, l.Capability, l.Headroom()-amount, l.Headroom())
	}
	if quantity > 0 {
		if headroom := l.QuantityHeadroom(); headroom >= 0 && quantity > headroom {
			return l, fmt.Errorf("authority: holding %d units on the lease for %s/%s leaves only %d",
				quantity, l.CellID, l.Capability, headroom)
		}
	}
	l.Reserved += amount
	l.ReservedUnits += quantity
	return l, nil
}

// Release returns held headroom to the lease without spending it. It is what a
// confirmed rejection and an expired reservation both do.
//
// Releasing more than is held is refused rather than clamped: it means two
// releases for one hold, and silently flooring at zero would hand back headroom
// that a still-pending action might yet consume.
func (l Lease) Release(amount float64, quantity int64) (Lease, error) {
	if amount < 0 || quantity < 0 {
		return l, fmt.Errorf("authority: cannot release a negative amount on %s/%s", l.CellID, l.Capability)
	}
	if amount > l.Reserved || quantity > l.ReservedUnits {
		return l, fmt.Errorf("authority: releasing %g %s and %d units on %s/%s, which holds only %g and %d; this is a double release",
			amount, l.Unit, quantity, l.CellID, l.Capability, l.Reserved, l.ReservedUnits)
	}
	l.Reserved -= amount
	l.ReservedUnits -= quantity
	return l, nil
}

// Convert turns a hold into settled spend, using the actual amount rather than
// the held one.
//
// The two differ whenever a quote was not exact, which is normal. An actual
// above the hold is still applied — the money is already gone, and refusing to
// record it would leave the lease claiming headroom that no longer exists — but
// it is reported, because a quote that is persistently wrong in one direction is
// a signal worth surfacing rather than absorbing (§7.1).
func (l Lease) Convert(held, actual float64, heldUnits, actualUnits int64) (Lease, error) {
	released, err := l.Release(held, heldUnits)
	if err != nil {
		return l, err
	}
	if actual < 0 || actualUnits < 0 {
		return l, fmt.Errorf("authority: cannot settle a negative amount on %s/%s", l.CellID, l.Capability)
	}
	released.Spent += actual
	released.Ordered += actualUnits
	if err := released.Validate(); err != nil {
		return l, fmt.Errorf("settling %g %s against a hold of %g: %w", actual, l.Unit, held, err)
	}
	return released, nil
}
