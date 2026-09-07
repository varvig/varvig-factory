package effect

import (
	"encoding/json"
	"errors"
	"fmt"

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

// State is a reservation's lifecycle.
type State string

// The states. There is no "retrying": a failed effectful action escalates, and
// retry is an authorized decision made by a higher principal (§6.7 rule 5).
const (
	// StatePending means reserved and possibly executed. The cell does not know
	// which, so nobody may act on the key until a principal resolves it.
	StatePending State = "pending"
	// StateDone means the external effect is confirmed to have happened.
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
	// ExternalRef is the external system's own identifier — the order number,
	// the contract id. It is what makes a pending reservation resolvable by a
	// human or an overseer agent looking the action up at the far end.
	ExternalRef string `json:"external_ref,omitempty"`
	// Detail explains a failure, or how a pending reservation was resolved.
	Detail     string `json:"detail,omitempty"`
	ReservedAt int64  `json:"reserved_at"`
	SettledAt  int64  `json:"settled_at,omitempty"`
}

// Unresolved reports whether this reservation blocks further action on its key.
func (r Reservation) Unresolved() bool { return r.State == StatePending }

func (r Reservation) String() string {
	s := fmt.Sprintf("%s %s/%s %s", short(r.Key), r.CellID, r.Capability, r.State)
	if r.Amount > 0 {
		s += fmt.Sprintf(" %g %s", r.Amount, r.Unit)
	}
	if r.ExternalRef != "" {
		s += " ref=" + r.ExternalRef
	}
	return s
}

// ErrAlreadyReserved is returned by Reserve when the key is already claimed.
//
// It is an error rather than a boolean because the only correct response is to
// stop: the action either already happened or is in an unknown state, and both
// readings forbid executing now.
var ErrAlreadyReserved = errors.New("effect: this action is already reserved")

// Reserve claims a request's idempotency key before the effect is attempted.
//
// On success the caller — and only the caller — may execute, then must call
// Settle or Fail. If the key is already claimed, Reserve returns the existing
// reservation with ErrAlreadyReserved and the caller must not execute.
//
// The claim is create-only, so two cells racing on the same key produce one
// winner and one ErrAlreadyReserved rather than two orders. That is varvig's
// ordinary ref CAS doing the work; nothing here needs a lock or a lease of its
// own.
func Reserve(v varvigcli.Varvig, req Request, executingCell string, at int64) (Reservation, string, error) {
	key, err := IdempotencyKey(req.Task, req.Capability, req.Payload)
	if err != nil {
		return Reservation{}, "", err
	}
	name, err := cell.ReservationRef(executingCell, key)
	if err != nil {
		return Reservation{}, "", err
	}

	// Look first, so the common "already done" case reports what happened rather
	// than only that the swap was refused.
	if existing, hash, err := loadReservation(v, name); err == nil {
		return existing, hash, fmt.Errorf("%w: %s", ErrAlreadyReserved, existing)
	} else if !errors.Is(err, varvigcli.ErrNoRef) {
		return Reservation{}, "", err
	}

	r := Reservation{
		Key: key, CellID: executingCell, Task: req.Task,
		Capability: req.Capability.ID, Interface: req.Capability.Interface,
		Amount: req.Amount, Unit: req.Unit, Quantity: req.Quantity,
		AuthorizedBy: req.AuthorizedBy,
		State:        StatePending, ReservedAt: at,
	}
	hash, err := writeReservation(v, name, r, "")
	if err != nil {
		if errors.Is(err, varvigcli.ErrCAS) {
			// Somebody claimed it between the read and the write. Re-read, so
			// the caller is told what the winner is doing rather than being
			// handed a bare CAS failure.
			if existing, ehash, lerr := loadReservation(v, name); lerr == nil {
				return existing, ehash, fmt.Errorf("%w: %s", ErrAlreadyReserved, existing)
			}
			return Reservation{}, "", fmt.Errorf("%w: %v", ErrAlreadyReserved, err)
		}
		return Reservation{}, "", err
	}
	return r, hash, nil
}

// Settle records that the external effect happened, with the far end's own
// identifier for it.
//
// externalRef is required. A settled reservation with nothing to look up is
// almost as bad as no reservation at all: the next question about this spend is
// "which order was it", and the answer has to be in the record.
func Settle(v varvigcli.Varvig, r Reservation, hash, externalRef string, at int64) (string, error) {
	if externalRef == "" {
		return "", fmt.Errorf("effect: settling %s needs the external reference; a spend nobody can look up is not a settled one", short(r.Key))
	}
	r.State, r.ExternalRef, r.SettledAt = StateDone, externalRef, at
	return update(v, r, hash)
}

// Fail records that the external service definitely rejected the action, so no
// effect occurred.
//
// **A timeout is not a failure.** Only a definite rejection may be recorded
// here; anything else leaves the reservation pending, because "we never heard
// back" and "it did not happen" are different claims and only one of them is
// safe to act on. The reservation is not deleted either way: the key stays
// claimed, and whether to authorize a fresh attempt is a decision for a higher
// principal.
func Fail(v varvigcli.Varvig, r Reservation, hash, reason string, at int64) (string, error) {
	if reason == "" {
		return "", errors.New("effect: recording a failure needs the rejection it is based on; without one this is a timeout, which stays pending")
	}
	r.State, r.Detail, r.SettledAt = StateFailed, reason, at
	return update(v, r, hash)
}

// Resolve records how a higher principal settled a pending reservation after
// checking the external system by hand.
//
// This is the exit from the one state a cell cannot resolve alone. It is a
// distinct call from Settle and Fail so the record says a principal decided it,
// which is the difference between a confirmed outcome and an assumed one.
func Resolve(v varvigcli.Varvig, r Reservation, hash string, happened bool, principal, detail string, at int64) (string, error) {
	if principal == "" {
		return "", errors.New("effect: resolving a pending reservation needs the principal who checked; a cell cannot resolve its own unknown state")
	}
	if principal == r.CellID {
		return "", fmt.Errorf("%w: %s cannot resolve its own pending reservation", ErrSelfAuthorization, r.CellID)
	}
	r.State = StateFailed
	if happened {
		r.State = StateDone
	}
	r.Detail = fmt.Sprintf("resolved by %s: %s", principal, detail)
	r.SettledAt = at
	return update(v, r, hash)
}

// Pending lists a cell's unresolved reservations.
//
// This is the report an overseer needs: every action whose outcome is unknown,
// each one a possible order that was placed and not recorded. A cell coming back
// from a crash calls it before doing anything effectful.
func Pending(v varvigcli.Varvig, cellID string) ([]Reservation, error) {
	if err := cell.CheckID(cellID); err != nil {
		return nil, err
	}
	refs, err := v.Refs()
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
		r, _, err := loadReservation(v, ref.Name)
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

func update(v varvigcli.Varvig, r Reservation, hash string) (string, error) {
	name, err := cell.ReservationRef(r.CellID, r.Key)
	if err != nil {
		return "", err
	}
	return writeReservation(v, name, r, hash)
}

func writeReservation(v varvigcli.Varvig, name string, r Reservation, oldHash string) (string, error) {
	body, err := cell.Canonical(r)
	if err != nil {
		return "", err
	}
	id, err := v.PutBlob(body)
	if err != nil {
		return "", err
	}
	if err := v.UpdateRef(name, id, oldHash); err != nil {
		return "", err
	}
	return id, nil
}

func loadReservation(v varvigcli.Varvig, name string) (Reservation, string, error) {
	hash, err := v.ResolveRef(name)
	if err != nil {
		return Reservation{}, "", err
	}
	body, err := v.ReadBlob(hash)
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
