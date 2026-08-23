package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/promote"
	"github.com/varvig/varvig-factory/varvigcli"
)

// PeerAttempt is another cell's attempt, as read from the repository.
type PeerAttempt struct {
	Ref     string
	Attempt cell.Attempt
}

// verifyPeerAttempts is the verification cell's whole job (FACTORY.md §3.1,
// §3.2).
//
// Because a cell can verify without attempting, evidence can be produced by a
// different cell than the one that authored the attempt — and that is precisely
// what makes autonomous promotion defensible (§6.3 condition 1). A signature
// proves who asserted the test result, not that the run was honest, so
// independence is the only leverage available and this is where it is created.
//
// A Micro cell does only this. It is not a fallback for a cell that cannot
// attempt: it is the strong role for cheap hardware — deterministic work, no
// model-quality problem — and it is what makes the old-hardware story genuinely
// compelling rather than aspirational.
func (c *Cell) verifyPeerAttempts(ctx context.Context, tickets []claim.Ticket) ([]VerifyResult, []string) {
	var results []VerifyResult
	var errs []string

	byID := map[string]claim.Ticket{}
	for _, t := range tickets {
		byID[t.ID] = t
	}

	attempts, err := c.PeerAttempts()
	if err != nil {
		return nil, []string{"reading peer attempts: " + err.Error()}
	}
	for _, pa := range attempts {
		if pa.Attempt.Change == "" {
			// An attempt that produced no change has nothing to verify. It is a
			// legitimate outcome, not an error.
			continue
		}
		already, err := c.haveEvidenceFor(pa.Attempt.Change)
		if err != nil {
			errs = append(errs, fmt.Sprintf("reading evidence for %s: %v", shortHash(pa.Attempt.Change), err))
			continue
		}
		if already {
			continue
		}
		t, ok := byID[pa.Attempt.Task]
		if !ok {
			// The attempt names a ticket this cell cannot see. That happens
			// legitimately mid-sync; skipping is right, and reporting it would be
			// noise on every pass.
			continue
		}
		req := t.Requirements()
		if missing := c.Capabilities.Missing(req.Build, req.Test); len(missing) > 0 {
			// A cell that cannot run the required checks must not write evidence
			// that says it did. Silence here is the honest answer.
			continue
		}

		ev, err := c.verifyAttempt(ctx, pa.Attempt)
		if err != nil {
			errs = append(errs, fmt.Sprintf("verifying %s: %v", shortHash(pa.Attempt.Change), err))
			continue
		}
		results = append(results, VerifyResult{Task: pa.Attempt.Task, Attempt: pa.Attempt.Change, Evidence: ev})
		c.logf("verified %s (attempt %d by %s): %s",
			shortHash(pa.Attempt.Change), pa.Attempt.N, pa.Attempt.CellID, verdictWord(ev))
	}
	return results, errs
}

func verdictWord(ev cell.Evidence) string {
	if ev.Passed() {
		return "pass"
	}
	return "not-pass (" + strings.Join(ev.Failures(), " ") + ")"
}

// PeerAttempts reads every attempt authored by another cell.
func (c *Cell) PeerAttempts() ([]PeerAttempt, error) {
	refs, err := c.V.Refs()
	if err != nil {
		return nil, err
	}
	var out []PeerAttempt
	for _, r := range refs {
		cellID, _, _, err := cell.ParseAttemptRef(r.Name)
		if err != nil || cellID == c.Capabilities.CellID {
			continue
		}
		payload, err := c.V.ReadBlob(r.Hash)
		if err != nil {
			continue
		}
		var att cell.Attempt
		if err := json.Unmarshal(payload, &att); err != nil || att.Validate() != nil {
			continue
		}
		out = append(out, PeerAttempt{Ref: r.Name, Attempt: att})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// haveEvidenceFor reports whether this cell has already published evidence for a
// change. Re-verifying every pass would burn the verification budget on answers
// the federation already has.
func (c *Cell) haveEvidenceFor(change string) (bool, error) {
	notes, err := c.V.Notes(change, cell.NoteEvidence)
	if err != nil {
		return false, err
	}
	for _, n := range notes {
		var ev cell.Evidence
		if err := json.Unmarshal(n.Payload, &ev); err != nil {
			continue
		}
		if ev.CellID == c.Capabilities.CellID {
			return true, nil
		}
	}
	return false, nil
}

// verifyAttempt checks out an attempt and runs the checks against it, writing
// evidence and the environment it ran in.
func (c *Cell) verifyAttempt(ctx context.Context, att cell.Attempt) (cell.Evidence, error) {
	scope, err := c.V.Scope(att.Task)
	if err != nil {
		return cell.Evidence{}, err
	}
	task, err := c.V.TaskStart(varvigcli.TaskRequest{
		Scope: taskScope(scope),
		TTL:   c.TaskTTL,
		// Base is the attempt's own change: verification runs against the state
		// under consideration, not against the branch head. Verifying the head
		// would produce evidence about something else entirely and label it with
		// this attempt's hash.
		Base: att.Change,
	})
	if err != nil {
		return cell.Evidence{}, fmt.Errorf("task start: %w", err)
	}
	defer func() {
		if err := c.V.TaskStop(task.ID); err != nil {
			c.logf("could not revoke task %s: %v", task.ID, err)
		}
	}()

	env, ev, err := c.runChecks(ctx, task.Dir, att.Task, att.Change)
	if err != nil {
		return cell.Evidence{}, err
	}
	envHash, err := c.recordEnvironment(att.Change, env)
	if err != nil {
		return cell.Evidence{}, err
	}
	ev.Environment = envHash
	if err := c.recordEvidence(att.Change, ev); err != nil {
		return cell.Evidence{}, err
	}
	return ev, nil
}

// Reverify implements promote.Reverifier: it runs the checks again, now, against
// the attempt under consideration.
//
// This is §6.3 condition 3, and the reason it reuses verifyAttempt is that
// re-verification and verification must be the same act. If re-verification were
// a cheaper, separate path, it would drift toward replaying stored evidence —
// which proves that a cell once said the tests passed, not that they pass
// against the state about to be promoted.
func (c *Cell) Reverify(ctx context.Context, att cell.Attempt) (cell.Evidence, error) {
	return c.verifyAttempt(ctx, att)
}

// evaluatePromotions is step 10.
//
// It runs in every mode. In gated mode it evaluates every condition, runs the
// policy module, logs the verdict — and does not act (§10.9). That is not
// wasted work: it is how the autonomous path stays exercised and how the
// agreement metric that licenses autonomy gets collected in the first place.
func (c *Cell) evaluatePromotions(ctx context.Context, tickets []claim.Ticket) ([]promote.Outcome, []string) {
	if c.Promoter == nil {
		return nil, nil
	}
	var outcomes []promote.Outcome
	var errs []string

	attempts, err := c.allAttempts()
	if err != nil {
		return nil, []string{"reading attempts: " + err.Error()}
	}
	for _, t := range tickets {
		for _, att := range attempts[t.ID] {
			if att.Change == "" {
				continue
			}
			req, err := c.promotionRequest(t, att)
			if err != nil {
				errs = append(errs, fmt.Sprintf("promotion request for %s: %v", shortHash(att.Change), err))
				continue
			}
			out, err := c.Promoter.Promote(ctx, req)
			if err != nil {
				errs = append(errs, fmt.Sprintf("promotion of %s: %v", shortHash(att.Change), err))
				continue
			}
			outcomes = append(outcomes, out)
			if out.Promoted {
				// A promotion this cell performed is still an observation worth
				// recording: the agreement metric is about the scorer, not about
				// who acted on it, and excluding autonomous promotions would make
				// the metric stop moving exactly when it matters most.
				if _, err := c.Promoter.ObservePromotion(req); err != nil {
					errs = append(errs, fmt.Sprintf("observing promotion of %s: %v", shortHash(att.Change), err))
				}
			}
		}
	}
	return outcomes, errs
}

// promotionRequest gathers everything a promotion decision needs. Every input is
// read from the repository rather than remembered from this pass: an attempt made
// by a peer three days ago must be evaluable by this cell today.
func (c *Cell) promotionRequest(t claim.Ticket, att cell.Attempt) (promote.Request, error) {
	req := promote.Request{
		Attempt:      att,
		Environments: map[string]cell.Environment{},
		Scope:        promotionScope(t.Scope),
		Ticket:       t.ID,
		TicketObject: t.Object,
		Ref:          c.branch(),
	}
	if req.Ref == "" {
		req.Ref = "refs/heads/main"
	}

	evNotes, err := c.V.Notes(att.Change, cell.NoteEvidence)
	if err != nil {
		return req, err
	}
	for _, n := range evNotes {
		var ev cell.Evidence
		if err := json.Unmarshal(n.Payload, &ev); err != nil {
			continue
		}
		if ev.Validate() != nil {
			continue
		}
		req.Evidence = append(req.Evidence, ev)
	}

	envNotes, err := c.V.Notes(att.Change, cell.NoteEnvironment)
	if err != nil {
		return req, err
	}
	for _, n := range envNotes {
		var env cell.Environment
		if err := json.Unmarshal(n.Payload, &env); err != nil {
			continue
		}
		hash, err := env.Hash()
		if err != nil {
			continue
		}
		req.Environments[hash] = env
	}

	if baseline, ok := c.baselineFor(req.Scope); ok {
		req.Baseline = &baseline
	}
	return req, nil
}

// baselineFor finds the declared environment baseline covering a scope, longest
// match first — so a specific baseline for src/generated/ wins over a general
// one for src/.
func (c *Cell) baselineFor(scope string) (cell.Environment, bool) {
	best, found := "", false
	for path := range c.Baselines {
		if varvigcli.ScopeCovers(path, scope) && len(path) >= len(best) {
			best, found = path, true
		}
	}
	if !found {
		return cell.Environment{}, false
	}
	return c.Baselines[best], true
}

// allAttempts groups every attempt in the repository by task, including this
// cell's own: a cell may promote a peer's attempt, and in autonomous mode it must
// be able to — the independence requirement is about who produced the *evidence*,
// not about who promotes.
func (c *Cell) allAttempts() (map[string][]cell.Attempt, error) {
	refs, err := c.V.Refs()
	if err != nil {
		return nil, err
	}
	out := map[string][]cell.Attempt{}
	for _, r := range refs {
		if _, _, _, err := cell.ParseAttemptRef(r.Name); err != nil {
			continue
		}
		payload, err := c.V.ReadBlob(r.Hash)
		if err != nil {
			continue
		}
		var att cell.Attempt
		if err := json.Unmarshal(payload, &att); err != nil || att.Validate() != nil {
			continue
		}
		out[att.Task] = append(out[att.Task], att)
	}
	for task := range out {
		sort.Slice(out[task], func(i, j int) bool {
			if out[task][i].CellID != out[task][j].CellID {
				return out[task][i].CellID < out[task][j].CellID
			}
			return out[task][i].N < out[task][j].N
		})
	}
	return out, nil
}

// promotionScope is the path a promotion writes to: the ticket's declared write
// set. Factory reads it; it does not choose it.
func promotionScope(s varvigcli.Scope) string {
	if len(s.Writes) == 1 {
		return s.Writes[0]
	}
	if len(s.Writes) == 0 {
		return ""
	}
	return commonPrefix(s.Writes)
}

// pinTTL is how long a pin asks upstream to retain an attempt. It is a duration
// rather than "forever" because an unexpiring pin is a permanent claim on another
// peer's disk (FEDERATION.md §4) — and because a speculative attempt that nobody
// promoted in a week is one nobody is going to.
const pinTTL = 7 * 24 * time.Hour

// releasePins drops this cell's oldest pins, dropping retention obligations
// deliberately rather than letting a disk fill while holding them (§7).
//
// It releases the *soonest-expiring* pins first: those are the ones the cell was
// already about to stop retaining, so releasing them early costs the federation
// the least.
func (c *Cell) releasePins(n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	prefix, err := cell.PinPeerPrefix(c.Capabilities.CellID)
	if err != nil {
		return 0, err
	}
	refs, err := c.V.Refs()
	if err != nil {
		return 0, err
	}
	type pin struct {
		ref      string
		hash     string
		notAfter int64
	}
	var pins []pin
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		_, notAfter, hash, err := cell.ParsePinRef(r.Name)
		if err != nil {
			continue
		}
		pins = append(pins, pin{ref: r.Name, hash: hash, notAfter: notAfter})
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].notAfter < pins[j].notAfter })

	released := 0
	for _, p := range pins {
		if released >= n {
			break
		}
		if err := c.V.DeleteRef(p.ref, p.hash); err != nil {
			if errors.Is(err, varvigcli.ErrNoRef) {
				continue
			}
			return released, err
		}
		released++
	}
	return released, nil
}
