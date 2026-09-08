package effect

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// A reservation is the idempotency key made durable.
//
// Deriving a key (see IdempotencyKey) says what "the same action" means. It does
// not by itself stop the action happening twice — for that, the key has to be
// claimed in a place that survives the process, *before* the effect is
// attempted. That place is a ref, and the claim is create-only: whoever creates
// it executes, and everyone else finds it already there.
//
// The ordering is the whole mechanism, and it is deliberately the pessimistic
// one. Reserve, then execute, then settle. A crash between reserve and settle
// leaves a **pending** reservation, which is the honest record of the one state
// that matters: the cell does not know whether the order was placed. That case
// escalates. It is not retried, and the reservation is not deleted to clear the
// way — deleting it is precisely how the second invoice arrives.
//
// A reservation also *holds lease headroom* for as long as it is outstanding
// (§7.1). Without that, two pending actions would each check the same headroom,
// each pass, and together exceed the lease. The hold is taken **before** the key
// is claimed, and released if the claim then fails: leaking headroom is
// recoverable — an expiry returns it — and a double-spend is not, so the order
// of those two writes is chosen to fail in the recoverable direction.
//
// Expiry releases the held headroom but **never the key** (§9.14). A lost
// external response must not permanently consume budget; it also must not let
// the same action be submitted again, because it may well have happened. Those
// are two different resources and they are released on two different rules.

// State is a reservation's lifecycle.
type State string

// The states. There is no "retrying": a failed effectful action escalates, and
// retry is an authorized decision made by a higher principal (§6.7 rule 5).
//
// The line that matters runs between StateOffered and StatePending. Before
// anything takes the reservation, nothing has happened and the key is safe to
// release; from the moment something takes it, the outcome is unknown and the
// key is claimed for good. Collapsing those two into one state would make a
// reservation nobody has touched look like an order that may be in flight, or
// worse, the reverse.
const (
	// StateOffered means reserved and waiting for something to act on it.
	// **Nothing has happened yet**, so this is the one unresolved state that is
	// safe to abandon.
	StateOffered State = "offered"
	// StatePending means taken and possibly executed. Whoever holds it may have
	// reached the external service already, so nobody may act on the key until a
	// principal resolves it.
	StatePending State = "pending"
	// StateReported means an executor has claimed an outcome that the lease has
	// not been settled from yet. It is a distinct state because the report and
	// the settlement are two writes that cannot be one: a connector reports, and
	// only the cell may move money on a lease.
	StateReported State = "reported"
	// StateDone means the external effect is confirmed to have happened and the
	// lease is settled.
	StateDone State = "done"
	// StateFailed means the external service is confirmed to have rejected it,
	// so no effect occurred. Only a definite rejection earns this — a timeout is
	// pending, not failed.
	StateFailed State = "failed"
)

// Reservation is the durable record of one effectful action, keyed by its
// idempotency key.
type Reservation struct {
	Key    string `json:"key"`
	CellID string `json:"cell_id"`
	Task   string `json:"task"`
	// Capability and Interface record what was ordered, the interface by hash,
	// so a reservation read back years later still names an unambiguous contract
	// (§2.1) even if the alias has since been re-pointed.
	Capability string `json:"capability"`
	Interface  string `json:"interface"`

	Amount   float64 `json:"amount,omitempty"`
	Unit     string  `json:"unit,omitempty"`
	Quantity int64   `json:"quantity,omitempty"`
	// AuthorizedBy is the higher principal that authorized this. It is recorded
	// rather than merely checked, because "who authorized this spend" is the
	// first question asked about an invoice nobody expected.
	AuthorizedBy string `json:"authorized_by"`

	State State `json:"state"`
	// ExpiresAt is when the *hold* lapses, releasing lease headroom. It is not
	// when the reservation lapses: the key stays claimed for good, because an
	// action whose outcome was never learned may have happened.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// TakenBy is the executor that holds this reservation, and TakenAt when it
	// took it. Empty while the reservation is merely offered.
	//
	// Recording the holder is what makes an offer safe to publish: whoever wins
	// the compare-and-swap acts, and everyone else sees a name that is not
	// theirs. It is also the first thing an operator wants when a reservation
	// has gone quiet — "which process was supposed to be doing this".
	TakenBy string `json:"taken_by,omitempty"`
	TakenAt int64  `json:"taken_at,omitempty"`
	// TakeDeadline is when the holder's claim goes stale. Passing it does **not**
	// release the reservation — the holder may have reached the vendor — it only
	// says the holder has stopped reporting, which is a thing to escalate rather
	// than a thing to retry.
	TakeDeadline int64 `json:"take_deadline,omitempty"`
	// HoldReleased records that the lease headroom for this reservation is no
	// longer held — because it settled, was rejected, or expired. It is tracked
	// on the reservation so a release cannot be applied twice to the lease.
	HoldReleased bool `json:"hold_released,omitempty"`
	// Actual is the settled cost when it differed from the quoted Amount.
	Actual float64 `json:"actual,omitempty"`
	// Happened records a connector's claim about the world: true when the effect
	// occurred, false when the service definitely refused. It is stored rather
	// than inferred from ExternalRef, so a rejection and a success stay
	// distinguishable without reading a string for meaning.
	Happened bool `json:"happened,omitempty"`
	// ExternalRef is the external system's own identifier — the order number,
	// the contract id. It is what makes a pending reservation resolvable by a
	// human or an overseer agent looking the action up at the far end.
	ExternalRef string `json:"external_ref,omitempty"`
	// Detail explains a failure, or how a pending reservation was resolved.
	Detail     string `json:"detail,omitempty"`
	ReservedAt int64  `json:"reserved_at"`
	SettledAt  int64  `json:"settled_at,omitempty"`
}

// Unresolved reports whether this reservation's outcome is unknown — the state
// that escalates, because the action may have happened and nobody can say.
//
// StateOffered is deliberately not unresolved: nothing has taken it, so nothing
// has happened. StateReported is not either: the outcome is known and only the
// bookkeeping is outstanding.
func (r Reservation) Unresolved() bool { return r.State == StatePending }

// Open reports whether this reservation still needs something to happen —
// whether that is an executor taking it, or the cell settling a report.
func (r Reservation) Open() bool {
	switch r.State {
	case StateOffered, StatePending, StateReported:
		return true
	}
	return false
}

func (r Reservation) String() string {
	s := fmt.Sprintf("%s %s/%s %s", short(r.Key), r.CellID, r.Capability, r.State)
	if r.Amount > 0 {
		s += fmt.Sprintf(" %.2f %s", r.Amount, r.Unit)
	}
	if r.ExternalRef != "" {
		s += " ref=" + r.ExternalRef
	}
	if r.State == StatePending && r.HoldReleased {
		// Worth surfacing: the money came back but the outcome is still unknown,
		// which is a different situation from a hold that is still standing.
		s += " (hold released, outcome still unknown)"
	}
	return s
}

// ErrAlreadyReserved is returned by Reserve when the key is already claimed.
//
// It is an error rather than a boolean because the only correct response is to
// stop: the action either already happened or is in an unknown state, and both
// readings forbid executing now.
var ErrAlreadyReserved = errors.New("effect: this action is already reserved")

// Claim is a successful reservation: the key, and the lease the hold was taken
// on, each with the object hash it lives at so the caller can settle without
// re-reading.
//
// The two hashes are carried together because the two writes are a pair. A
// caller holding only one of them cannot finish the job.
type Claim struct {
	Reservation Reservation
	Hash        string
	Lease       authority.Lease
	LeaseHash   string
}

// Two repositories, and the order the two writes go in.
//
// A reservation lives in a project repository and the lease it spends from lives
// in the factory-coordination repository, so every state change here is *two*
// writes to two repos. They cannot be made atomic — not by care, and not in
// principle, because two repositories with independent CAS have no shared
// transaction — so what matters is which state a crash between them leaves
// behind.
//
// **The lease is written first, always.** Whether the lease write takes headroom
// (Reserve), converts it to spend (Settle) or gives it back (Fail, Resolve), it
// lands before the reservation record moves. The consequences differ by path and
// are worth naming:
//
//   - Taking a hold first, then failing to claim the key, leaks headroom until
//     the expiry returns it. Recoverable, and ReleaseExpired exists for it.
//   - Recording spend first, then failing to mark the reservation done,
//     over-reports spend against the lease. Wrong in the direction an overseer
//     can correct, and never in the direction that spends twice.
//   - Releasing a hold first, then failing to mark the reservation failed,
//     leaves a record that still reads open against a lease that no longer holds
//     for it. A retry then hits Lease.Release's double-release guard and errors
//     loudly rather than returning headroom twice, and Convert releases before
//     it spends so it is guarded by the same check. The reservation is stuck
//     until a principal resolves it — visibly stuck, which is the point.
//
// The one state never reachable is a reservation that says an irreversible
// action is settled against a lease that has not recorded paying for it. That is
// the only failure here nobody can undo, so it is the one the ordering buys.
//
// Reserve claims a request's idempotency key and holds the lease headroom for
// it, before the effect is attempted.
//
// On success the caller — and only the caller — may execute, then must call
// Settle or Fail. If the key is already claimed, Reserve returns the existing
// reservation with ErrAlreadyReserved and the caller must not execute.
//
// The claim is create-only, so two cells racing on the same key produce one
// winner and one ErrAlreadyReserved rather than two orders. That is varvig's
// ordinary ref CAS doing the work; nothing here needs a lock or a lease of its
// own.
//
// ttl is how long the hold lasts. Zero means no expiry, which is legitimate for
// a capability that always answers synchronously — but for anything asynchronous
// it means a lost response consumes the headroom for good, so set one.
func Reserve(f varvigcli.FactoryRepo, p varvigcli.ProjectRepo, req Request, executingCell string, grant authority.Grant, at, ttl int64) (Claim, error) {
	key, err := IdempotencyKey(req.Task, req.Capability, req.Payload)
	if err != nil {
		return Claim{}, err
	}
	name, err := cell.ReservationRef(executingCell, key)
	if err != nil {
		return Claim{}, err
	}
	if grant.Lease == nil {
		return Claim{}, errors.New("effect: no lease is held for this capability; an effectful action spends from an exclusive allocation, never from a shared envelope")
	}
	lease := *grant.Lease
	if lease.CellID != executingCell {
		return Claim{}, fmt.Errorf("effect: %s cannot reserve against a lease held by %q; spend comes from the acting cell's own lease",
			executingCell, lease.CellID)
	}
	if lease.Capability != req.Capability.ID {
		return Claim{}, fmt.Errorf("effect: the lease is for %s but this action is %s", lease.Capability, req.Capability.ID)
	}

	// Look first, so the common "already done" case reports what happened rather
	// than only that a swap was refused — and so an existing claim's hold is not
	// taken a second time.
	//
	// This precedes the envelope check deliberately. "Did my order go through?"
	// is the more urgent answer, and a tightening that arrived afterwards must
	// not obscure an action that already happened — the envelope bounds what
	// happens next, not what is already done.
	if existing, hash, err := loadReservation(p, name); err == nil {
		return Claim{Reservation: existing, Hash: hash, Lease: lease, LeaseHash: grant.LeaseHash},
			fmt.Errorf("%w: %s", ErrAlreadyReserved, existing)
	} else if !errors.Is(err, varvigcli.ErrNoRef) {
		return Claim{}, err
	}

	// The hold is taken against the envelope-bounded view, so a tightened
	// envelope refuses the reservation here rather than at settlement — before
	// the effect happens, which is the only point at which refusing helps
	// (§9.12).
	bounded, err := grant.Bounded()
	if err != nil {
		return Claim{}, err
	}

	// Hold the headroom first. If the key claim below then fails, the hold is
	// released; if *that* release fails, headroom leaks until the expiry returns
	// it. Leaking headroom is recoverable and a double-spend is not, so the
	// writes go in this order.
	//
	// The check runs against the bounded view and the write against the lease as
	// issued: storing the bounded lease would rewrite the record of what the
	// overseer actually committed to, and that record is the evidence for every
	// later question about this spend.
	if _, err := bounded.Hold(req.Amount, req.Quantity); err != nil {
		return Claim{}, err
	}
	held, err := lease.Hold(req.Amount, req.Quantity)
	if err != nil {
		return Claim{}, err
	}
	heldHash, err := authority.PublishLease(f, held, grant.LeaseHash)
	if err != nil {
		return Claim{}, err
	}

	var expires int64
	if ttl > 0 {
		expires = at + ttl
	}
	// A fresh reservation is *offered*: the headroom is held, and nothing has
	// touched the vendor. The caller decides what happens next — an in-process
	// executor takes it immediately, a connector-served one is left for whichever
	// connector picks it up.
	r := Reservation{
		Key: key, CellID: executingCell, Task: req.Task,
		Capability: req.Capability.ID, Interface: req.Capability.Interface,
		Amount: req.Amount, Unit: req.Unit, Quantity: req.Quantity,
		AuthorizedBy: req.AuthorizedBy,
		State:        StateOffered, ReservedAt: at, ExpiresAt: expires,
	}
	hash, err := writeReservation(p, name, r, "")
	if err != nil {
		// The key was claimed between the read and the write. Give the hold
		// back, because the winner has taken its own.
		if back, rerr := held.Release(req.Amount, req.Quantity); rerr == nil {
			if backHash, perr := authority.PublishLease(f, back, heldHash); perr == nil {
				held, heldHash = back, backHash
			}
		}
		if errors.Is(err, varvigcli.ErrCAS) {
			// Re-read, so the caller is told what the winner is doing rather
			// than being handed a bare CAS failure.
			if existing, ehash, lerr := loadReservation(p, name); lerr == nil {
				return Claim{Reservation: existing, Hash: ehash, Lease: held, LeaseHash: heldHash},
					fmt.Errorf("%w: %s", ErrAlreadyReserved, existing)
			}
			return Claim{Lease: held, LeaseHash: heldHash}, fmt.Errorf("%w: %v", ErrAlreadyReserved, err)
		}
		return Claim{Lease: held, LeaseHash: heldHash}, err
	}
	return Claim{Reservation: r, Hash: hash, Lease: held, LeaseHash: heldHash}, nil
}

// Settle records that the external effect happened, converts the hold into
// settled spend, and writes both refs.
//
// externalRef is required. A settled reservation with nothing to look up is
// almost as bad as no reservation at all: the next question about this spend is
// "which order was it", and the answer has to be in the record.
//
// actual is what it really cost. Pass 0 to mean "as quoted". A divergence from
// the quote is recorded rather than absorbed (§7.1): one is noise, a pattern of
// them is a capability whose quotes cannot be trusted.
func Settle(f varvigcli.FactoryRepo, p varvigcli.ProjectRepo, c Claim, externalRef string, actual float64, at int64) (Claim, error) {
	if externalRef == "" {
		return c, fmt.Errorf("effect: settling %s needs the external reference; a spend nobody can look up is not a settled one", short(c.Reservation.Key))
	}
	if c.Reservation.HoldReleased {
		return c, fmt.Errorf("effect: the hold for %s is already released; settling again would spend the lease twice", short(c.Reservation.Key))
	}
	if err := c.hasLease(); err != nil {
		return c, err
	}
	if actual == 0 {
		actual = c.Reservation.Amount
	}
	lease, err := c.Lease.Convert(c.Reservation.Amount, actual, c.Reservation.Quantity, c.Reservation.Quantity)
	if err != nil {
		return c, err
	}
	// The lease first, in the factory repo: a crash after this and before the
	// reservation write over-reports spend, which an overseer can correct. The
	// reverse order would let a reservation say an irreversible action was paid
	// for against a lease with no record of paying, which nobody can.
	leaseHash, err := authority.PublishLease(f, lease, c.LeaseHash)
	if err != nil {
		return c, err
	}
	c.Lease, c.LeaseHash = lease, leaseHash

	r := c.Reservation
	r.State, r.ExternalRef, r.SettledAt, r.HoldReleased = StateDone, externalRef, at, true
	if actual != r.Amount {
		r.Actual = actual
		r.Detail = fmt.Sprintf("quoted %.2f %s, actual %.2f %s", r.Amount, r.Unit, actual, r.Unit)
	}
	hash, err := update(p, r, c.Hash)
	if err != nil {
		return c, err
	}
	c.Reservation, c.Hash = r, hash
	return c, nil
}

// Fail records that the external service definitely rejected the action, so no
// effect occurred, and releases the hold.
//
// **A timeout is not a failure.** Only a definite rejection may be recorded
// here; anything else leaves the reservation pending, because "we never heard
// back" and "it did not happen" are different claims and only one of them is
// safe to act on. The reservation is not deleted either way: the key stays
// claimed, and whether to authorize a fresh attempt is a decision for a higher
// principal.
func Fail(f varvigcli.FactoryRepo, p varvigcli.ProjectRepo, c Claim, reason string, at int64) (Claim, error) {
	if reason == "" {
		return c, errors.New("effect: recording a failure needs the rejection it is based on; without one this is a timeout, which stays pending")
	}
	r := c.Reservation
	if err := c.hasLease(); err != nil {
		return c, err
	}
	r.State, r.Detail, r.SettledAt = StateFailed, reason, at
	if !r.HoldReleased {
		lease, err := c.Lease.Release(r.Amount, r.Quantity)
		if err != nil {
			return c, err
		}
		leaseHash, err := authority.PublishLease(f, lease, c.LeaseHash)
		if err != nil {
			return c, err
		}
		c.Lease, c.LeaseHash, r.HoldReleased = lease, leaseHash, true
	}
	hash, err := update(p, r, c.Hash)
	if err != nil {
		return c, err
	}
	c.Reservation, c.Hash = r, hash
	return c, nil
}

// Resolve records how a higher principal settled a pending reservation after
// checking the external system by hand.
//
// This is the exit from the one state a cell cannot resolve alone. It is a
// distinct call from Settle and Fail so the record says a principal decided it,
// which is the difference between a confirmed outcome and an assumed one.
func Resolve(f varvigcli.FactoryRepo, p varvigcli.ProjectRepo, c Claim, happened bool, principal, detail string, at int64) (Claim, error) {
	r := c.Reservation
	if principal == "" {
		return c, errors.New("effect: resolving a pending reservation needs the principal who checked; a cell cannot resolve its own unknown state")
	}
	if principal == r.CellID {
		return c, fmt.Errorf("%w: %s cannot resolve its own pending reservation", ErrSelfAuthorization, r.CellID)
	}
	if happened {
		// It happened: the lease owes the money. If the hold was already
		// released by an expiry, the spend is applied without a hold to convert
		// — which is exactly the case expiry-without-release-of-the-key exists
		// to keep survivable.
		lease := c.Lease
		var err error
		if r.HoldReleased {
			lease.Spent += r.Amount
			lease.Ordered += r.Quantity
			err = lease.Validate()
		} else {
			lease, err = c.Lease.Convert(r.Amount, r.Amount, r.Quantity, r.Quantity)
		}
		if err != nil {
			return c, err
		}
		leaseHash, err := authority.PublishLease(f, lease, c.LeaseHash)
		if err != nil {
			return c, err
		}
		c.Lease, c.LeaseHash = lease, leaseHash
		r.State, r.HoldReleased = StateDone, true
	} else {
		if !r.HoldReleased {
			lease, err := c.Lease.Release(r.Amount, r.Quantity)
			if err != nil {
				return c, err
			}
			leaseHash, err := authority.PublishLease(f, lease, c.LeaseHash)
			if err != nil {
				return c, err
			}
			c.Lease, c.LeaseHash, r.HoldReleased = lease, leaseHash, true
		}
		r.State = StateFailed
	}
	r.Detail = fmt.Sprintf("resolved by %s: %s", principal, detail)
	r.SettledAt = at
	hash, err := update(p, r, c.Hash)
	if err != nil {
		return c, err
	}
	c.Reservation, c.Hash = r, hash
	return c, nil
}

// Expired reports whether this reservation's hold has lapsed.
//
// It applies to an offered reservation as much as a taken one — more clearly, in
// fact: nothing took the offer, so nothing happened, and holding lease headroom
// for an action no executor ever picked up is exactly the budget leak §9.14 is
// about. What it never does is release the *key*, in either state, because
// distinguishing "nobody took it" from "somebody took it and went quiet" is the
// state machine's job and not the expiry's.
func (r Reservation) Expired(now int64) bool {
	return r.ExpiresAt > 0 && now > r.ExpiresAt && !r.HoldReleased && r.Open()
}

// ReleaseExpired returns the lease headroom held by expired reservations, so a
// lost external response cannot permanently consume budget (§9.14).
//
// It releases the **headroom only**. The reservation stays pending and the key
// stays claimed for good: the action may have happened, and letting the key go
// would let the same order be placed again. Two different resources, two
// different rules — money comes back on a timer, the right to act does not come
// back at all.
//
// Returns the updated lease and the reservations whose holds were released, so
// the caller can report them: each one is still an action of unknown outcome.
func ReleaseExpired(f varvigcli.FactoryRepo, p varvigcli.ProjectRepo, lease authority.Lease, leaseHash string, now int64) (authority.Lease, string, []Reservation, error) {
	open, err := openReservations(p, lease.CellID)
	if err != nil {
		return lease, leaseHash, nil, err
	}
	var released []Reservation
	for _, r := range open {
		if r.Capability != lease.Capability || !r.Expired(now) {
			continue
		}
		next, err := lease.Release(r.Amount, r.Quantity)
		if err != nil {
			return lease, leaseHash, released, err
		}
		nextHash, err := authority.PublishLease(f, next, leaseHash)
		if err != nil {
			return lease, leaseHash, released, err
		}
		lease, leaseHash = next, nextHash

		name, err := cell.ReservationRef(r.CellID, r.Key)
		if err != nil {
			return lease, leaseHash, released, err
		}
		current, err := p.ResolveRef(name)
		if err != nil {
			return lease, leaseHash, released, err
		}
		r.HoldReleased = true
		r.Detail = fmt.Sprintf("hold released at %d without an answer; the key stays claimed because the action may have happened", now)
		if _, err := writeReservation(p, name, r, current); err != nil {
			// The lease is already correct, which is the part that matters; the
			// caller re-runs to finish marking the record.
			return lease, leaseHash, released, err
		}
		released = append(released, r)
	}
	return lease, leaseHash, released, nil
}

// Pending lists a cell's unresolved reservations.
//
// This is the report an overseer needs: every action whose outcome is unknown,
// each one a possible order that was placed and not recorded. A cell coming back
// from a crash calls it before doing anything effectful.
func Pending(p varvigcli.ProjectRepo, cellID string) ([]Reservation, error) {
	if err := cell.CheckID(cellID); err != nil {
		return nil, err
	}
	refs, err := p.Refs()
	if err != nil {
		return nil, err
	}
	prefix := cell.ReservationPrefix + cellID + "/"
	var out []Reservation
	var bad []string
	for _, ref := range refs {
		if len(ref.Name) <= len(prefix) || ref.Name[:len(prefix)] != prefix {
			continue
		}
		r, _, err := loadReservation(p, ref.Name)
		if err != nil {
			// Reported, never skipped: an unreadable reservation is an action of
			// unknown outcome, which is the very thing this call exists to find.
			bad = append(bad, fmt.Sprintf("%s: %v", ref.Name, err))
			continue
		}
		if r.Unresolved() {
			out = append(out, r)
		}
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("effect: %d unreadable reservation refs, each an action of unknown outcome: %v", len(bad), bad)
	}
	return out, nil
}

// LoadClaim rebuilds a Claim for a reservation a principal is about to resolve.
//
// Pending reports *what* is unresolved; this fetches the handles needed to act
// on one. They are separate calls because listing happens once and resolving
// happens per reservation, each against a lease that may have moved in between.
func LoadClaim(p varvigcli.ProjectRepo, cellID, key string, lease authority.Lease, leaseHash string) (Claim, error) {
	name, err := cell.ReservationRef(cellID, key)
	if err != nil {
		return Claim{}, err
	}
	r, hash, err := loadReservation(p, name)
	if err != nil {
		return Claim{}, err
	}
	if r.Capability != lease.Capability || r.CellID != lease.CellID {
		return Claim{}, fmt.Errorf("effect: reservation %s is %s/%s but the lease given is %s/%s",
			short(key), r.CellID, r.Capability, lease.CellID, lease.Capability)
	}
	return Claim{Reservation: r, Hash: hash, Lease: lease, LeaseHash: leaseHash}, nil
}

func update(p varvigcli.ProjectRepo, r Reservation, hash string) (string, error) {
	name, err := cell.ReservationRef(r.CellID, r.Key)
	if err != nil {
		return "", err
	}
	return writeReservation(p, name, r, hash)
}

func writeReservation(p varvigcli.ProjectRepo, name string, r Reservation, oldHash string) (string, error) {
	body, err := cell.Canonical(r)
	if err != nil {
		return "", err
	}
	id, err := p.PutBlob(body)
	if err != nil {
		return "", err
	}
	if err := p.UpdateRef(name, id, oldHash); err != nil {
		return "", err
	}
	return id, nil
}

func loadReservation(p varvigcli.ProjectRepo, name string) (Reservation, string, error) {
	hash, err := p.ResolveRef(name)
	if err != nil {
		return Reservation{}, "", err
	}
	body, err := p.ReadBlob(hash)
	if err != nil {
		return Reservation{}, hash, err
	}
	var r Reservation
	if err := json.Unmarshal(body, &r); err != nil {
		return Reservation{}, hash, fmt.Errorf("effect: %s does not hold a well-formed reservation: %w", name, err)
	}
	return r, hash, nil
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}

// TakeSelf marks a reservation as held by the cell itself, for the in-process
// path.
//
// The cell goes through the same take as a connector rather than skipping
// straight to pending, so there is one answer to "who holds this" and one state
// machine to reason about. An operator looking at a stuck reservation should not
// have to know which path produced it.
func TakeSelf(p varvigcli.ProjectRepo, c Claim, at int64) (Claim, error) {
	taken, err := Take(p, c.Reservation.CellID, c.Reservation.Key, c.Reservation.CellID, at, c.Reservation.TakeDeadline)
	if err != nil {
		return c, err
	}
	// Take is written for a connector, which never touches a lease and so gets
	// none back. The cell does spend, so it carries its own lease across —
	// without this the settlement that follows would operate on a zero lease.
	taken.Lease, taken.LeaseHash = c.Lease, c.LeaseHash
	return taken, nil
}

// openReservations lists a cell's reservations that still hold lease headroom —
// offered, taken, or reported but not yet settled.
//
// Pending answers a narrower question (which outcomes are unknown) because that
// is the one an operator escalates on. This one answers "what is still holding
// money", which is what the expiry sweep needs.
func openReservations(p varvigcli.ProjectRepo, cellID string) ([]Reservation, error) {
	if err := cell.CheckID(cellID); err != nil {
		return nil, err
	}
	refs, err := p.Refs()
	if err != nil {
		return nil, err
	}
	prefix := cell.ReservationPrefix + cellID + "/"
	var out []Reservation
	var bad []string
	for _, ref := range refs {
		if !strings.HasPrefix(ref.Name, prefix) {
			continue
		}
		r, _, err := loadReservation(p, ref.Name)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", ref.Name, err))
			continue
		}
		if r.Open() {
			out = append(out, r)
		}
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("effect: %d unreadable reservation refs: %v", len(bad), bad)
	}
	return out, nil
}

// hasLease refuses a claim whose lease was not carried through.
//
// A zero lease is not a small mistake here: Release and Convert would compute
// against an allocation of nothing and produce a double-release error naming a
// cell called "/", which says nothing about what actually went wrong. This
// turns a confusing arithmetic failure into the plumbing bug it is.
func (c Claim) hasLease() error {
	if c.Lease.CellID == "" {
		return fmt.Errorf("effect: settling %s has no lease attached; the claim lost it between taking and settling",
			short(c.Reservation.Key))
	}
	if c.Lease.CellID != c.Reservation.CellID {
		return fmt.Errorf("effect: settling %s carries %s's lease, not %s's",
			short(c.Reservation.Key), c.Lease.CellID, c.Reservation.CellID)
	}
	return nil
}
