package effect

import (
	"errors"
	"fmt"
	"strings"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// The connector protocol: how something outside this binary performs an
// effectful action.
//
// # Why this is not a plugin interface
//
// It is modelled on how core does tracker bridges, and for the same reasons. A
// bridge connector is a separate peer holding a bridge-kind key; core "never
// learns a vendor's name", the seam speaks only in an opaque system tag and a
// foreign id, and the connector is untrusted — its key kind caps what it can
// assert, so it can run anywhere.
//
// Everything that makes that work applies here, and one thing makes it matter
// more: an executor holds credentials to a service that charges money.
// Compiling vendors into the cell binary would mean a rebuild to add one and
// those credentials living in the cell's process. A connector holds its own
// credentials, runs wherever the operator puts it, and needs no rebuild.
//
// So the interface is **repository state**, not an ABI. There is nothing to
// load, nothing to link, and no version to keep in step.
//
// # The exchange
//
//	cell       offers    a reservation naming a capability
//	connector  takes     it, by compare-and-swap, recording who it is
//	connector  executes  against the vendor
//	connector  reports   the outcome — an order number and what it cost
//	cell       settles   the lease from that report
//
// The last line is the one that must not move. A connector reports; only the
// cell spends. That is the same containment core applies to a bridge, which may
// sign a weak attestation and never a strong one: the untrusted peer states a
// fact, and the trusted layer applies it under rules. Here the rule is the
// lease, so a connector that lies about cost is bounded by an amount the
// overseer chose deliberately.
//
// # What a connector cannot do
//
// It cannot create a reservation, so it cannot invent work for itself. It
// cannot raise a lease or widen an envelope. It cannot settle, so it cannot
// move money. It cannot take a reservation another connector holds. What it can
// do is claim an outcome for an action that a cell reserved and an overseer
// authorized — and be wrong about it, bounded by that lease.

// ErrNotOffered is returned when a reservation is not available to take.
var ErrNotOffered = errors.New("effect: reservation is not offered")

// ErrNotHolder is returned when a connector reports on a reservation it does
// not hold. Two connectors racing produce one holder and one of these.
var ErrNotHolder = errors.New("effect: reservation is held by someone else")

// Offer records a reservation as available for a connector to take.
//
// This is what Reserve produces when the capability is served by a connector
// rather than in-process. The reservation is real and the lease headroom is
// already held — the money is committed the moment the offer exists, because a
// connector may pick it up at any time and the cell cannot take that back.
func Offer(v varvigcli.Varvig, c Claim, deadline int64) (Claim, error) {
	if c.Reservation.State != StateOffered {
		return c, fmt.Errorf("%w: %s is %s", ErrNotOffered, short(c.Reservation.Key), c.Reservation.State)
	}
	r := c.Reservation
	r.TakeDeadline = deadline
	hash, err := update(v, r, c.Hash)
	if err != nil {
		return c, err
	}
	c.Reservation, c.Hash = r, hash
	return c, nil
}

// Awaiting lists the reservations offered for a capability and not yet taken.
//
// This is a connector's inbox, and it is derived rather than stored: there is no
// queue, only reservation refs whose state says nobody holds them. A connector
// that dies and restarts sees the same list, and two connectors see the same
// list and race for it — which the take resolves.
//
// Matching is on the interface **hash**, never the alias: a connector serving a
// different interface under the same name is serving a different capability
// (§2.1), and here that mistake is resolved by spending money.
func Awaiting(v varvigcli.Varvig, capability Capability) ([]Reservation, error) {
	if err := capability.Validate(); err != nil {
		return nil, err
	}
	all, err := allReservations(v)
	if err != nil {
		return nil, err
	}
	var out []Reservation
	for _, r := range all {
		if r.State == StateOffered && r.Interface == capability.Interface {
			out = append(out, r)
		}
	}
	return out, nil
}

// Take claims an offered reservation for one connector, by compare-and-swap.
//
// Whoever wins the swap acts; everyone else gets ErrNotOffered and must not
// touch the vendor. That is the whole exclusion mechanism, and it is varvig's
// ordinary ref CAS rather than a lock, a lease or a queue.
//
// deadline is when the connector's claim goes stale. Passing it does not release
// the reservation to anyone else: by then the connector may have reached the
// vendor, and handing the same action to a second connector is precisely the
// double-order this protocol exists to prevent. A stale claim escalates.
func Take(v varvigcli.Varvig, cellID, key, connectorID string, at, deadline int64) (Claim, error) {
	if connectorID == "" {
		return Claim{}, errors.New("effect: a connector must identify itself to take a reservation")
	}
	name, err := cell.ReservationRef(cellID, key)
	if err != nil {
		return Claim{}, err
	}
	r, hash, err := loadReservation(v, name)
	if err != nil {
		return Claim{}, err
	}
	if r.State != StateOffered {
		holder := r.TakenBy
		if holder == "" {
			holder = "nobody"
		}
		return Claim{Reservation: r, Hash: hash},
			fmt.Errorf("%w: %s is %s (held by %s)", ErrNotOffered, short(key), r.State, holder)
	}

	r.State, r.TakenBy, r.TakenAt = StatePending, connectorID, at
	if deadline > 0 {
		r.TakeDeadline = deadline
	}
	newHash, err := writeReservation(v, name, r, hash)
	if err != nil {
		if errors.Is(err, varvigcli.ErrCAS) {
			// Another connector took it between the read and the write. Report
			// what it is now, so the loser knows who is acting rather than only
			// that it lost.
			if now, nowHash, lerr := loadReservation(v, name); lerr == nil {
				return Claim{Reservation: now, Hash: nowHash},
					fmt.Errorf("%w: %s was taken by %s", ErrNotOffered, short(key), now.TakenBy)
			}
		}
		return Claim{}, err
	}
	return Claim{Reservation: r, Hash: newHash}, nil
}

// Report records a connector's outcome. It does **not** settle the lease.
//
// happened=true means the effect occurred and externalRef names it at the far
// end; happened=false means the service definitely refused, so no effect
// occurred. A connector that does not know must report nothing at all and let
// the reservation stand as pending — "we never heard back" is not a report, and
// a connector that guesses here is worse than one that goes quiet.
//
// actual is what it really cost; zero means as quoted.
func Report(v varvigcli.Varvig, c Claim, connectorID string, happened bool, externalRef string, actual float64, detail string, at int64) (Claim, error) {
	r := c.Reservation
	if r.TakenBy == "" || r.TakenBy != connectorID {
		return c, fmt.Errorf("%w: %s holds %s, not %q", ErrNotHolder, r.TakenBy, short(r.Key), connectorID)
	}
	if r.State != StatePending {
		return c, fmt.Errorf("effect: %s is %s and cannot be reported on", short(r.Key), r.State)
	}
	if happened && externalRef == "" {
		// The same rule Settle applies, enforced at the point the claim is made:
		// a spend nobody can look up is not a settled one, and letting the
		// reference go missing here would only surface later as a settlement
		// that cannot be completed.
		return c, fmt.Errorf("effect: reporting %s as done needs the external reference", short(r.Key))
	}
	if !happened && detail == "" {
		return c, errors.New("effect: reporting a rejection needs the refusal it is based on; without one this is a timeout, which stays pending")
	}

	r.State = StateReported
	r.ExternalRef, r.Actual, r.Detail, r.SettledAt = externalRef, actual, detail, at
	// Happened is carried in the record rather than inferred from ExternalRef,
	// so a rejection and a success are distinguishable without reading a string.
	r.Happened = happened
	hash, err := update(v, r, c.Hash)
	if err != nil {
		return c, err
	}
	c.Reservation, c.Hash = r, hash
	return c, nil
}

// Reported lists this cell's reservations awaiting settlement — the cell's half
// of the exchange, and the mirror of Awaiting.
func Reported(v varvigcli.Varvig, cellID string) ([]Reservation, error) {
	if err := cell.CheckID(cellID); err != nil {
		return nil, err
	}
	all, err := allReservations(v)
	if err != nil {
		return nil, err
	}
	var out []Reservation
	for _, r := range all {
		if r.State == StateReported && r.CellID == cellID {
			out = append(out, r)
		}
	}
	return out, nil
}

// SettleReported applies a connector's report to the lease.
//
// This is where the untrusted half meets the trusted one. The connector said
// what happened; the cell decides what that costs, and the lease bounds it —
// Convert refuses an actual beyond the allocation, so a connector reporting a
// wild figure is contained by an amount the overseer chose.
func SettleReported(v varvigcli.Varvig, c Claim, at int64) (Claim, error) {
	if c.Reservation.State != StateReported {
		return c, fmt.Errorf("effect: %s is %s, not a report awaiting settlement", short(c.Reservation.Key), c.Reservation.State)
	}
	if c.Reservation.Happened {
		return Settle(v, withState(c, StatePending), c.Reservation.ExternalRef, c.Reservation.Actual, at)
	}
	return Fail(v, withState(c, StatePending), c.Reservation.Detail, at)
}

// withState returns the claim with its reservation moved to s, so the settle
// helpers — which are written for the in-process path — see the state they
// expect. The stored value is not touched: only the copy handed onward.
func withState(c Claim, s State) Claim {
	c.Reservation.State = s
	return c
}

// allReservations reads every reservation in the repository.
//
// A connector serves a capability across whichever cells hold leases for it, so
// its inbox cannot be scoped to one cell's prefix the way Pending is.
func allReservations(v varvigcli.Varvig) ([]Reservation, error) {
	refs, err := v.Refs()
	if err != nil {
		return nil, err
	}
	var out []Reservation
	var bad []string
	for _, ref := range refs {
		if !strings.HasPrefix(ref.Name, cell.ReservationPrefix) {
			continue
		}
		r, _, err := loadReservation(v, ref.Name)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", ref.Name, err))
			continue
		}
		out = append(out, r)
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("effect: %d unreadable reservation refs: %v", len(bad), bad)
	}
	return out, nil
}
