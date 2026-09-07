package authority

import (
	"fmt"
	"time"
)

// Act is a class of thing a cell might do, grouped by what makes it safe rather
// than by what it looks like (varvig-auth-and-api.md §4.3b).
type Act int

// The three classes. The grouping is by *reversibility*, which is why an
// effectful action sits apart from a promotion even though both are "writes":
// a promotion moves a ref and can be reasoned about from repository state; an
// order for physical goods cannot be un-ordered.
const (
	// ActPropose appends. Nobody promotes it, so a revoked principal that has
	// not heard yet wastes compute and nothing more.
	ActPropose Act = iota
	// ActPromote moves a ref. Irreversible in the sense that matters: it
	// changes what everyone else builds on.
	ActPromote
	// ActEffectful has real-world side effects that cannot be re-run,
	// discarded, or regenerated.
	ActEffectful
)

func (a Act) String() string {
	switch a {
	case ActPromote:
		return "promote"
	case ActEffectful:
		return "effectful action"
	default:
		return "propose"
	}
}

// Sync is what a cell knows about how current its view of trust state is.
//
// Only the component that ran the sync can fill this in, which is why freshness
// is Factory's to enforce and not varvig's: varvig's ref-update verification
// checks the signer against the trust file *as this peer holds it*, and a
// partitioned peer holds a stale file it has every reason to believe is current.
// Nothing inside varvig can tell the difference. The loop can.
type Sync struct {
	// Reachable reports whether the most recent sync attempt succeeded.
	Reachable bool
	// At is when trust state was last successfully confirmed against a peer.
	// The zero time means never.
	At time.Time
	// Configured reports whether this cell has an upstream at all. A
	// single-cell deployment with no peer is not stale — there is nothing it
	// could be behind — and treating it as stale would make promotion
	// impossible for the simplest working configuration.
	Configured bool
}

// MaxAge bounds how old a successful sync may be and still count as fresh.
//
// The default is **zero, meaning no age bound** — exactly what the spec asks
// for and nothing more. §4.3b requires promotion to "read current trust state";
// a successful sync is what establishes that, and inventing an age threshold
// here would be inventing policy. An operator who wants a stricter rule (a cell
// whose loop has stalled without failing, say) sets one explicitly.
type MaxAge time.Duration

// Fresh reports whether trust state is current enough to act on a shared
// ceiling.
func (s Sync) Fresh(now time.Time, maxAge MaxAge) bool {
	if !s.Configured {
		// No upstream: there is no peer whose state this cell could be behind.
		return true
	}
	if !s.Reachable || s.At.IsZero() {
		return false
	}
	if maxAge <= 0 {
		return true
	}
	return now.Sub(s.At) <= time.Duration(maxAge)
}

// Refusal explains why an act is not permitted. It is a distinct type rather
// than an error string because a caller needs to distinguish "not now, sync
// first" from "not ever, escalate" — those lead to different next steps.
type Refusal struct {
	Act Act
	// Reason is operator-facing and names what would have to change.
	Reason string
	// Escalate reports that a higher principal must decide, rather than the
	// cell retrying after a sync.
	Escalate bool
}

func (r Refusal) Error() string { return fmt.Sprintf("%s refused: %s", r.Act, r.Reason) }

// Permit decides whether a cell may take an act given its sync state and, for
// an effectful act, its lease.
//
// The table this implements is §4.3b's, and the asymmetry is the point:
//
//	Propose    stale is fine        — append-only bounds the damage
//	Promote    needs fresh state    — a shared ceiling cannot be enforced locally
//	Effectful  needs a lease, and then stale is fine, indefinitely
//
// A cell can therefore keep working while disconnected, and can spend what was
// already exclusively allocated to it, but cannot promote. That is the correct
// trade rather than a limitation: it uses the append-only write path as the
// containment mechanism instead of adding a renewal protocol the network cannot
// support.
func Permit(act Act, s Sync, now time.Time, maxAge MaxAge, lease *Lease) error {
	switch act {
	case ActPropose:
		// Long-lived and lag-tolerant, by design.
		return nil

	case ActPromote:
		if s.Fresh(now, maxAge) {
			return nil
		}
		reason := "trust state is not current; promotion moves a ref, and a shared ceiling cannot be enforced from a stale view"
		if s.Configured && !s.Reachable {
			reason = "upstream is unreachable, so trust state cannot be confirmed current; the cell may keep proposing but not promote"
		} else if !s.At.IsZero() && maxAge > 0 {
			reason = fmt.Sprintf("trust state was last confirmed %s ago, beyond the configured maximum of %s",
				now.Sub(s.At).Round(time.Second), time.Duration(maxAge))
		}
		return Refusal{Act: act, Reason: reason}

	case ActEffectful:
		// Note what is *not* checked here: freshness. An effectful action
		// within an outstanding lease is allowed offline and indefinitely,
		// because the amount was already committed when the lease was issued.
		// Requiring connectivity in this path would be the wrong guard on the
		// wrong problem.
		if lease == nil {
			return Refusal{Act: act, Escalate: true,
				Reason: "no lease is held for this capability; an effectful action spends from an exclusive allocation, never from a shared envelope"}
		}
		if err := lease.Validate(); err != nil {
			return Refusal{Act: act, Escalate: true, Reason: err.Error()}
		}
		if lease.Exhausted() {
			return Refusal{Act: act, Escalate: true, Reason: fmt.Sprintf(
				"the lease for %s is exhausted (%g of %g %s spent); the cell stops and says so rather than borrowing against the envelope",
				lease.Capability, lease.Spent, lease.Amount, lease.Unit)}
		}
		return nil
	}
	return Refusal{Act: act, Reason: "unknown act class"}
}

// PermitSpend is Permit for an effectful act of a known size, so the headroom
// check names the actual shortfall.
//
// Beyond the lease is refused and escalates — it does not fall back to the
// envelope. Falling back would make the lease advisory, and an advisory
// exclusive allocation is a shared one.
func PermitSpend(s Sync, now time.Time, maxAge MaxAge, lease *Lease, amount float64, quantity int64) error {
	if err := Permit(ActEffectful, s, now, maxAge, lease); err != nil {
		return err
	}
	if amount > 0 && lease.Amount > 0 && amount > lease.Headroom() {
		return Refusal{Act: ActEffectful, Escalate: true, Reason: fmt.Sprintf(
			"this action costs %g %s but the lease for %s has %g %s left; beyond the lease escalates rather than drawing on the envelope",
			amount, lease.Unit, lease.Capability, lease.Headroom(), lease.Unit)}
	}
	if quantity > 0 {
		if headroom := lease.QuantityHeadroom(); headroom >= 0 && quantity > headroom {
			return Refusal{Act: ActEffectful, Escalate: true, Reason: fmt.Sprintf(
				"this action orders %d units but the lease for %s has %d left",
				quantity, lease.Capability, headroom)}
		}
	}
	return nil
}
