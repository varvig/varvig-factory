package cell

import (
	"fmt"
	"sort"
)

// Status is one check's outcome.
type Status string

// The four statuses. skip and error are deliberately distinct from both pass and
// fail: a check that did not run has not passed, and a harness that crashed has
// not failed the code. Collapsing either into pass is how a green wall stops
// meaning anything (CELL.md §4.1).
const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusSkip  Status = "skip"
	StatusError Status = "error"
)

// Check is one measurement.
type Check struct {
	Name       string `json:"name"`
	Status     Status `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// Detail is a short human-readable note (a failing test name, an exit
	// code). It is not parsed by anything and must never carry a verdict the
	// Status field does not already carry.
	Detail string `json:"detail,omitempty"`
}

// Evidence is a cell's assertion about an attempt: what was checked, on what
// ground, with what result (CELL.md §4.1).
//
// CellID records *who asserted the result, not that the run was honest*
// (varvig-design.md §4b.3). A signature establishes authorship and nothing
// stronger. That limit is exactly why autonomous promotion requires evidence
// from a cell other than the attempting one (FACTORY.md §6.3): independence is
// the only leverage available, so it is spent rather than assumed.
type Evidence struct {
	Attempt     string  `json:"attempt"`
	Task        string  `json:"task"`
	CellID      string  `json:"cell_id"`
	Environment string  `json:"environment"`
	Checks      []Check `json:"checks"`
	ProducedAt  int64   `json:"produced_at"`
}

// Normalize sorts checks by name so that two runs of the same check set encode
// identically. Two checks with the same name are kept — a suite may legitimately
// report the same name twice — and their relative order is then fixed by status
// so the encoding stays deterministic.
func (e *Evidence) Normalize() {
	sort.SliceStable(e.Checks, func(i, j int) bool {
		if e.Checks[i].Name != e.Checks[j].Name {
			return e.Checks[i].Name < e.Checks[j].Name
		}
		return e.Checks[i].Status < e.Checks[j].Status
	})
}

// Validate checks the shape. Evidence that arrived from a peer is exactly as
// untrusted as evidence this cell produced, so both go through here.
func (e Evidence) Validate() error {
	if e.Attempt == "" {
		return fmt.Errorf("cell: evidence names no attempt")
	}
	if err := CheckID(e.CellID); err != nil {
		return fmt.Errorf("cell: evidence: %w", err)
	}
	if len(e.Checks) == 0 {
		// Evidence with no checks is not weak evidence, it is no evidence. It
		// would satisfy a naive "has evidence?" test while asserting nothing.
		return fmt.Errorf("cell: evidence for %s records no checks", short(e.Attempt))
	}
	for _, c := range e.Checks {
		if c.Name == "" {
			return fmt.Errorf("cell: evidence has an unnamed check")
		}
		switch c.Status {
		case StatusPass, StatusFail, StatusSkip, StatusError:
		default:
			return fmt.Errorf("cell: check %q has unknown status %q", c.Name, c.Status)
		}
	}
	if e.Environment == "" {
		// Permitted by the shape, and unknown class forever after (§4.4). The
		// caller decides whether that is acceptable; Validate reports it as
		// valid-but-unknown rather than rejecting, because refusing it would
		// discard a legitimate hand-recorded observation.
		return nil
	}
	return nil
}

// Passed reports whether every check passed. skip and error are not pass, so a
// suite that silently skipped its integration tests does not report green.
func (e Evidence) Passed() bool {
	for _, c := range e.Checks {
		if c.Status != StatusPass {
			return false
		}
	}
	return len(e.Checks) > 0
}

// Failures returns the names of checks that did not pass, with their status, so
// a refusal can say what was wrong.
func (e Evidence) Failures() []string {
	var out []string
	for _, c := range e.Checks {
		if c.Status != StatusPass {
			out = append(out, c.Name+"="+string(c.Status))
		}
	}
	sort.Strings(out)
	return out
}

// Independent reports whether this evidence was produced by a cell other than
// the one that authored the attempt — the §6.3 condition 1 predicate, kept here
// next to the evidence shape so there is one definition of it.
func (e Evidence) Independent(attemptingCell string) bool {
	return e.CellID != "" && attemptingCell != "" && e.CellID != attemptingCell
}

// Attempt is the immutable record of one try at a task, stored at AttemptRef.
//
// It records what varvig produced (the change) and what Factory chose (the
// model, the environment). It deliberately does not record a read set, a write
// set, or an ordering: those belong to varvig's scheduler and a cell that
// computed them would be a second scheduler (CELL.md §10.1).
type Attempt struct {
	CellID string `json:"cell_id"`
	Task   string `json:"task"`
	N      int    `json:"n"`
	// Change is the varvig change hash this attempt produced, or empty if the
	// attempt produced no change (a legitimate outcome: the model may conclude
	// there is nothing to do, and that is worth recording rather than retrying).
	Change string `json:"change,omitempty"`
	// Environment is the hash of the environment the attempt was authored in.
	Environment string `json:"environment,omitempty"`
	// Artifacts are the content hashes of artifact-ref records this attempt
	// produced (CELL.md §7).
	Artifacts []string `json:"artifacts,omitempty"`
	CreatedAt int64    `json:"created_at"`
	// Note is a short human-facing summary. Never parsed.
	Note string `json:"note,omitempty"`
}

// Validate checks an attempt record's shape.
func (a Attempt) Validate() error {
	if err := CheckID(a.CellID); err != nil {
		return fmt.Errorf("cell: attempt: %w", err)
	}
	if !validTaskID(a.Task) {
		return fmt.Errorf("cell: attempt: invalid task id %q", a.Task)
	}
	if a.N < 1 {
		return fmt.Errorf("cell: attempt: number must be >= 1, got %d", a.N)
	}
	return nil
}

// ArtifactRef records the identity of, and reachability to, bytes that live
// outside varvig's object model (CELL.md §7). Its field set mirrors varvig's
// TypeArtifactRef (FEDERATION.md §1) so the note form can be replaced by the
// object form when a native CLI exists.
//
// ContentHash is identity; Locators are hints. A locator changing is not a new
// artifact — that distinction is why the same image reachable from three
// registries is one record with three locators rather than three records.
type ArtifactRef struct {
	ContentHash string   `json:"content_hash"`
	MediaType   string   `json:"media_type,omitempty"`
	Size        int64    `json:"size,omitempty"`
	Locators    []string `json:"locators,omitempty"`
	ProducedBy  string   `json:"produced_by,omitempty"`
}

// Normalize sorts and deduplicates locators, so an equal locator set always
// encodes identically.
func (a *ArtifactRef) Normalize() { a.Locators = sortDedup(a.Locators) }

// Validate checks an artifact-ref record.
func (a ArtifactRef) Validate() error {
	if a.ContentHash == "" {
		return fmt.Errorf("cell: artifact-ref has no content hash")
	}
	if a.Size < 0 {
		return fmt.Errorf("cell: artifact-ref %s has negative size", short(a.ContentHash))
	}
	return nil
}

// short trims a labelled hash for messages, keeping the label.
func short(h string) string {
	if len(h) <= 20 {
		return h
	}
	return h[:20] + "…"
}
