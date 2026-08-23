package cell

import (
	"fmt"
	"time"
)

// Claim is a cell's advisory statement that it intends to attempt a task
// (CELL.md §5). It lives at ClaimRef and it is exactly three things: who, what,
// and until when.
//
// Claims are advisory and cannot be exclusive across a partition
// (varvig-design.md §4b.3). Two cells may each compare-and-swap successfully
// against their own view of the repository, and both are correct. Duplicate
// attempts are normal and are the point — branching is search — so a cell must
// not add consensus, leader election, or a lock service to prevent them.
type Claim struct {
	CellID string `json:"cell_id"`
	Task   string `json:"task"`
	// NotAfter is mandatory. A claim without an expiry is a lock, and a lock
	// held by a partitioned cell is a task nobody may ever attempt again.
	NotAfter int64 `json:"not_after"`
	// Attempt is the attempt number this claim covers, so a claim and the
	// attempt it precedes can be matched up after a crash.
	Attempt int `json:"attempt"`
	// Offline records that the claim was made with upstream unreachable. It is
	// carried so that a cell reconciling later can tell speculative work from
	// coordinated work, and so the tighter offline budget can be attributed
	// (CELL.md §8).
	Offline bool `json:"offline,omitempty"`
}

// NewClaim builds a claim expiring ttl from now.
func NewClaim(cellID, taskID string, attempt int, ttl time.Duration, now time.Time, offline bool) (Claim, error) {
	c := Claim{
		CellID:   cellID,
		Task:     taskID,
		NotAfter: now.Add(ttl).Unix(),
		Attempt:  attempt,
		Offline:  offline,
	}
	if err := c.Validate(); err != nil {
		return Claim{}, err
	}
	if ttl <= 0 {
		return Claim{}, fmt.Errorf("cell: claim ttl must be positive, got %s", ttl)
	}
	return c, nil
}

// Validate checks a claim's shape.
func (c Claim) Validate() error {
	if err := CheckID(c.CellID); err != nil {
		return fmt.Errorf("cell: claim: %w", err)
	}
	if !validTaskID(c.Task) {
		return fmt.Errorf("cell: claim: invalid task id %q", c.Task)
	}
	if c.NotAfter == 0 {
		return fmt.Errorf("cell: claim on %s has no not_after; an unexpiring claim is a lock", short(c.Task))
	}
	if c.Attempt < 0 {
		return fmt.Errorf("cell: claim: negative attempt number %d", c.Attempt)
	}
	return nil
}

// Stale reports whether the claim has passed its expiry. A stale claim stops
// being a reason for another cell to skip, and the cell that wrote it has no
// further standing from it.
func (c Claim) Stale(now time.Time) bool { return now.Unix() > c.NotAfter }

// Remaining is how long the claim still stands, zero once stale.
func (c Claim) Remaining(now time.Time) time.Duration {
	d := time.Unix(c.NotAfter, 0).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}
