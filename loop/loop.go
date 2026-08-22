// Package loop is the cell loop (FACTORY.md §5): a loop over repository state,
// not an RPC server.
//
//  1. sync with upstream (if reachable)
//  2. observe open tickets in scope
//  3. evaluate claim policy  → claim or skip
//  4. submit the task to varvig
//  5. build + test in sandbox → evidence + environment
//  6. write artifact-refs for any binary outputs
//  7. commit attempt as immutable state
//  8. pin what upstream should retain
//  9. sync upstream (if reachable)
//  10. evaluate promotion policy
//
// There is no queue, no controller and no leader. Upstream is a varvig peer,
// nothing more (§1.1). A disconnected cell continues working, and on reconnect
// exchanges missing objects and attempts compare-and-swap, which fails safely
// rather than overwriting — materially different from a worker queue, where
// losing the controller strands the work.
//
// # What this package must never grow
//
// Read/write sets, affected-set computation, ordering between tickets,
// serialization. All four belong to varvig's scheduler (§1, CELL.md §10.1). The
// loop names the scope a ticket already declared and lets varvig serialize. A
// guard test in ./guard enforces this mechanically rather than leaving it to
// reviewer vigilance.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/artifact"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/inference"
	"github.com/varvig/varvig-factory/promote"
	"github.com/varvig/varvig-factory/sandbox"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Check is one named command a cell runs to produce evidence.
type Check struct {
	Name    string
	Command []string
	Timeout time.Duration
	// Kind says which role the check belongs to, so a build-only cell does not
	// run a test suite it never claimed to support.
	Kind cell.Role
}

// Cell is a configured Factory cell.
type Cell struct {
	Capabilities cell.Capabilities
	V            varvigcli.Varvig
	Inference    inference.Runtime
	Sandbox      sandbox.Sandbox
	Artifacts    artifact.Store
	Ledger       *budget.Ledger
	Promoter     *promote.Promoter

	// Upstream is the peer address to sync with. Empty means a single-cell
	// deployment with no upstream, which is legitimate and not an error.
	Upstream string
	// Branch is the branch to fetch and push.
	Branch string
	// Checks are the commands that produce evidence.
	Checks []Check
	// ClaimTTL bounds a claim. Every claim carries an expiry (CELL.md §5).
	ClaimTTL time.Duration
	// TaskTTL bounds a varvig task credential.
	TaskTTL time.Duration
	// WorkDir is where task checkouts are made.
	WorkDir string
	// ArtifactGlobs name build outputs to record as artifact-refs, relative to
	// the checkout.
	ArtifactGlobs []string
	// Baselines maps a path scope to its declared environment baseline
	// (FEDERATION.md §2.3), for §6.3 condition 2.
	Baselines map[string]cell.Environment
	// YieldToFreshClaims is claim.Inputs.YieldToFreshClaims.
	YieldToFreshClaims bool
	// MaxAttemptsPerCell caps repeat attempts by this cell at one task.
	MaxAttemptsPerCell int

	Now func() time.Time
	Log func(string)
}

// Report is what one pass did. Every field is a count or a list rather than a
// bare error, because a pass that attempted one ticket and skipped nine has
// succeeded and the nine skips are the interesting part.
type Report struct {
	Offline    bool
	Observed   int
	Claimed    []string
	Skipped    map[claim.SkipReason]int
	Attempts   []AttemptResult
	Verified   []VerifyResult
	Promotions []promote.Outcome
	// Errors are non-fatal problems. The loop continues past a single ticket's
	// failure: one malformed ticket must not stop a cell.
	Errors []string
}

// AttemptResult is one attempt this cell made.
type AttemptResult struct {
	Task        string
	N           int
	Change      string
	Ref         string
	Environment string
	Evidence    cell.Evidence
	Artifacts   []cell.ArtifactRef
	Cost        float64
}

// VerifyResult is one peer attempt this cell verified.
type VerifyResult struct {
	Task     string
	Attempt  string
	Evidence cell.Evidence
}

func (c *Cell) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cell) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

func (c *Cell) branch() string { return c.Branch }

// Validate checks a cell's configuration before it does anything, so a
// misconfiguration is a startup error rather than a confusing runtime one.
func (c *Cell) Validate(ctx context.Context) error {
	c.Capabilities.Normalize()
	if err := c.Capabilities.Validate(); err != nil {
		return err
	}
	if c.V == nil {
		return errors.New("loop: no varvig client configured")
	}
	if c.Ledger == nil {
		return errors.New("loop: no budget ledger configured; a cell without a declared budget is not permitted (§7)")
	}
	if c.ClaimTTL <= 0 {
		return errors.New("loop: claim ttl must be positive; an unexpiring claim is a lock")
	}
	// Every adapter must be able to describe itself, checked once at startup
	// rather than at the first attempt (§4). A cell that cannot describe its
	// environment cannot participate in cross-cell selection, so it should not
	// start and produce evidence nobody can compare.
	if c.Sandbox != nil {
		if _, err := c.Sandbox.Fragment(ctx); err != nil {
			return fmt.Errorf("loop: sandbox adapter: %w", err)
		}
	}
	if c.Capabilities.Has(cell.RoleAttempt) {
		if c.Inference == nil {
			return errors.New("loop: cell holds the attempt role but has no model runtime")
		}
		if _, err := c.Inference.Fragment(ctx); err != nil {
			return fmt.Errorf("loop: model runtime adapter: %w", err)
		}
	}
	return nil
}

// PublishCapabilities writes this cell's capabilities object to its ref (§2).
// Static facts only: nothing here changes without a human changing the
// configuration, which is what keeps liveness out of the DAG (§2.1).
func (c *Cell) PublishCapabilities() error {
	c.Capabilities.Normalize()
	if err := c.Capabilities.Validate(); err != nil {
		return err
	}
	payload, err := cell.Canonical(c.Capabilities)
	if err != nil {
		return err
	}
	id, err := c.V.PutBlob(payload)
	if err != nil {
		return err
	}
	ref, err := cell.CapabilitiesRef(c.Capabilities.CellID)
	if err != nil {
		return err
	}
	old, err := c.V.ResolveRef(ref)
	if err != nil && !errors.Is(err, varvigcli.ErrNoRef) {
		return err
	}
	if old == id {
		return nil
	}
	return c.V.UpdateRef(ref, id, old)
}

// Run loops until the context is cancelled, pausing interval between passes.
func (c *Cell) Run(ctx context.Context, interval time.Duration) error {
	if err := c.Validate(ctx); err != nil {
		return err
	}
	if err := c.PublishCapabilities(); err != nil {
		// Publishing is how a federation learns this cell exists. Failing to
		// publish is not fatal — a disconnected cell still works — but it is
		// worth saying.
		c.logf("could not publish capabilities: %v", err)
	}
	for {
		rep, err := c.Once(ctx)
		if err != nil {
			c.logf("pass failed: %v", err)
		} else {
			c.logf("%s", rep.Summary())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Once runs one pass of the ten steps.
func (c *Cell) Once(ctx context.Context) (Report, error) {
	rep := Report{Skipped: map[claim.SkipReason]int{}}

	// Step 1: sync with upstream, if reachable. Unreachable is not a failure —
	// it is a mode (§5.2).
	rep.Offline = c.fetch()

	// Step 2: observe open tickets.
	tickets, err := c.observe()
	if err != nil {
		return rep, err
	}
	rep.Observed = len(tickets)

	claims, ownAttempts, err := c.readClaimState()
	if err != nil {
		return rep, err
	}

	// The verify role runs first, and independently of attempting. A Micro cell
	// does only this, and it is the cell that makes autonomous promotion
	// defensible at all (§3.2) — so it is not an afterthought appended to the
	// attempt path.
	if c.Capabilities.Has(cell.RoleVerify) || c.Capabilities.Has(cell.RoleBuild) {
		results, errs := c.verifyPeerAttempts(ctx, tickets)
		rep.Verified = append(rep.Verified, results...)
		rep.Errors = append(rep.Errors, errs...)
	}

	for _, t := range tickets {
		spend := c.Ledger.CanSpend(c.now(), rep.Offline)
		verdict := claim.Evaluate(claim.Inputs{
			Capabilities:       c.Capabilities,
			Ticket:             t,
			BudgetOK:           spend.OK,
			BudgetReason:       string(spend.Reason),
			OwnAttempts:        ownAttempts[t.ID],
			MaxAttemptsPerCell: c.maxAttempts(t),
			ForeignClaims:      claims[t.ID],
			Offline:            rep.Offline,
			YieldToFreshClaims: c.YieldToFreshClaims,
			Now:                c.now(),
		})
		if !verdict.Claim {
			rep.Skipped[verdict.Skip]++
			c.logf("skip %s: %s", shortID(t.ID), verdict.Reason)
			continue
		}

		// Step 3: claim.
		if err := c.writeClaim(t, verdict.Attempt, rep.Offline); err != nil {
			// A refused claim CAS is normal: another process in this cell got
			// there first. It is not an error worth failing the pass over.
			c.logf("could not claim %s: %v", shortID(t.ID), err)
			rep.Errors = append(rep.Errors, fmt.Sprintf("claim %s: %v", shortID(t.ID), err))
			continue
		}
		rep.Claimed = append(rep.Claimed, t.ID)
		c.logf("%s", verdict.Reason)

		// Steps 4–8.
		result, err := c.attempt(ctx, t, verdict.Attempt, rep.Offline)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("attempt %s: %v", shortID(t.ID), err))
			continue
		}
		rep.Attempts = append(rep.Attempts, result)
	}

	// Step 9: sync upstream.
	if !rep.Offline {
		if err := c.V.Push(c.Upstream, c.branch()); err != nil {
			if errors.Is(err, varvigcli.ErrUnreachable) {
				rep.Offline = true
			} else {
				// A refused push is upstream having diverged. The local state is
				// intact and immutable; the next pass will fetch and reconcile.
				rep.Errors = append(rep.Errors, "push: "+err.Error())
			}
		}
	}

	// Step 10: promotion policy.
	outcomes, errs := c.evaluatePromotions(ctx, tickets)
	rep.Promotions = append(rep.Promotions, outcomes...)
	rep.Errors = append(rep.Errors, errs...)

	// Storage pressure: release pins before GC, so the cell drops its own
	// retention obligations deliberately rather than collecting state another
	// cell is evaluating (§7).
	if err := c.relieveStorage(); err != nil {
		rep.Errors = append(rep.Errors, "storage: "+err.Error())
	}
	return rep, nil
}

// fetch is step 1. It returns whether the cell is offline.
func (c *Cell) fetch() bool {
	if c.Upstream == "" {
		// No upstream configured is not offline: there is nothing to be
		// disconnected from, and treating it as offline would apply the tighter
		// offline budget to a single-cell deployment forever.
		return false
	}
	if err := c.V.Fetch(c.Upstream, c.branch()); err != nil {
		if errors.Is(err, varvigcli.ErrUnreachable) {
			c.logf("upstream %s unreachable; continuing offline", c.Upstream)
			return true
		}
		c.logf("fetch from %s failed: %v", c.Upstream, err)
		return false
	}
	return false
}

// observe is step 2: the open tickets, with the state the policy needs. Every
// field comes from varvig.
func (c *Cell) observe() ([]claim.Ticket, error) {
	ids, err := c.V.TicketIDs()
	if err != nil {
		return nil, err
	}
	refs, err := c.V.Refs()
	if err != nil {
		return nil, err
	}
	objects := map[string]string{}
	for _, r := range refs {
		objects[r.Name] = r.Hash
	}

	out := make([]claim.Ticket, 0, len(ids))
	for _, id := range ids {
		t := claim.Ticket{ID: id, Object: objects["refs/varvig/tickets/"+id]}
		if t.Spec, err = c.V.Spec(id); err != nil {
			c.logf("could not read spec for %s: %v", shortID(id), err)
			continue
		}
		if t.Scope, err = c.V.Scope(id); err != nil {
			c.logf("could not read scope for %s: %v", shortID(id), err)
			continue
		}
		if t.Status, err = c.V.TicketStatus(id); err != nil {
			c.logf("could not read status for %s: %v", shortID(id), err)
			continue
		}
		if t.Blockers, err = c.V.Blockers(id); err != nil {
			c.logf("could not read blockers for %s: %v", shortID(id), err)
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// readClaimState reads foreign claims and this cell's own attempt counts from
// the repository. Both come from ref names, which is why the naming rules are
// part of the contract rather than an implementation detail (CELL.md §2).
func (c *Cell) readClaimState() (map[string][]cell.Claim, map[string]int, error) {
	refs, err := c.V.Refs()
	if err != nil {
		return nil, nil, err
	}
	claims := map[string][]cell.Claim{}
	attempts := map[string]int{}
	for _, r := range refs {
		switch {
		case strings.HasPrefix(r.Name, cell.ClaimPrefix):
			rest := strings.TrimPrefix(r.Name, cell.ClaimPrefix)
			cellID, taskID, ok := strings.Cut(rest, "/")
			if !ok || cellID == c.Capabilities.CellID {
				continue
			}
			payload, err := c.V.ReadBlob(r.Hash)
			if err != nil {
				continue
			}
			var cl cell.Claim
			if err := json.Unmarshal(payload, &cl); err != nil || cl.Validate() != nil {
				continue
			}
			claims[taskID] = append(claims[taskID], cl)
		case strings.HasPrefix(r.Name, cell.AttemptPrefix):
			cellID, taskID, _, err := cell.ParseAttemptRef(r.Name)
			if err != nil || cellID != c.Capabilities.CellID {
				continue
			}
			attempts[taskID]++
		}
	}
	return claims, attempts, nil
}

func (c *Cell) maxAttempts(t claim.Ticket) int {
	if c.MaxAttemptsPerCell > 0 {
		return c.MaxAttemptsPerCell
	}
	return c.Ledger.Budget().Attempts(t.Requirements().Attempts)
}

// writeClaim is step 3. The claim ref is per-cell, so two cells never contend
// for the same name; the CAS here only guards against two processes inside one
// cell (CELL.md §5).
func (c *Cell) writeClaim(t claim.Ticket, attempt int, offline bool) error {
	cl, err := cell.NewClaim(c.Capabilities.CellID, t.ID, attempt, c.ClaimTTL, c.now(), offline)
	if err != nil {
		return err
	}
	payload, err := cell.Canonical(cl)
	if err != nil {
		return err
	}
	id, err := c.V.PutBlob(payload)
	if err != nil {
		return err
	}
	ref, err := cell.ClaimRef(c.Capabilities.CellID, t.ID)
	if err != nil {
		return err
	}
	old, err := c.V.ResolveRef(ref)
	if err != nil && !errors.Is(err, varvigcli.ErrNoRef) {
		return err
	}
	return c.V.UpdateRef(ref, id, old)
}

// attempt runs steps 4 through 8 for one ticket.
func (c *Cell) attempt(ctx context.Context, t claim.Ticket, n int, offline bool) (AttemptResult, error) {
	// Step 4: submit to varvig. This is the whole of "how work is scheduled":
	// name the scope the ticket declared and let varvig serialize. Factory does
	// not compute the scope, order it against other work, or decide what it
	// conflicts with.
	scope := taskScope(t.Scope)
	dir := ""
	if c.WorkDir != "" {
		dir = filepath.Join(c.WorkDir, fmt.Sprintf("%s-%d", sanitize(t.ID), n))
	}
	task, err := c.V.TaskStart(varvigcli.TaskRequest{Scope: scope, TTL: c.TaskTTL, Dir: dir})
	if err != nil {
		return AttemptResult{}, fmt.Errorf("task start: %w", err)
	}
	defer func() {
		if err := c.V.TaskStop(task.ID); err != nil {
			c.logf("could not revoke task %s: %v", task.ID, err)
		}
	}()

	// Authoring. The budget check happened in the claim policy; the spend is
	// recorded here, after the call, from what the runtime actually reported.
	resp, err := c.Inference.Generate(ctx, inference.Request{
		Task:    t.ID,
		Intent:  t.Spec,
		Attempt: n,
		Context: c.readContext(task.Dir, t.Scope),
	})
	if err != nil {
		return AttemptResult{}, fmt.Errorf("inference: %w", err)
	}
	cost := c.Ledger.Spend(c.now(), offline, resp.TokensIn, resp.TokensOut)

	written, err := ApplyOutput(task.Dir, resp.Text, t.Scope.Writes)
	if err != nil {
		return AttemptResult{}, err
	}
	if len(written) == 0 {
		// A model concluding there is nothing to do is a legitimate outcome and
		// worth recording as an attempt rather than retrying: the next attempt
		// would cost the same and, at temperature zero, say the same thing.
		c.logf("attempt %d at %s produced no file changes", n, shortID(t.ID))
	}

	change := ""
	if len(written) > 0 {
		if change, err = c.V.Commit(task.Dir, fmt.Sprintf("factory %s attempt %d for %s", c.Capabilities.CellID, n, shortID(t.ID))); err != nil {
			return AttemptResult{}, fmt.Errorf("commit: %w", err)
		}
		// Record the change as a speculation candidate. The pool, the scoring
		// and the selection are varvig's (TICKETS.md §3.3); Factory contributes
		// candidates to it.
		if err := c.V.SpecAdd(t.ID, change); err != nil {
			c.logf("could not add %s to the speculation pool: %v", shortHash(change), err)
		}
	}

	// Step 5: build and test → evidence + environment.
	env, evidence, err := c.runChecks(ctx, task.Dir, t.ID, change)
	if err != nil {
		return AttemptResult{}, err
	}
	envHash, err := c.recordEnvironment(change, env)
	if err != nil {
		return AttemptResult{}, err
	}
	evidence.Environment = envHash
	if change != "" {
		if err := c.recordEvidence(change, evidence); err != nil {
			return AttemptResult{}, err
		}
	}

	// Step 6: artifact-refs for binary outputs. Cell-local: Put never transfers
	// bytes, so a speculative build's output stays here until promotion (§8).
	artifacts, err := c.recordArtifacts(ctx, task.Dir, change)
	if err != nil {
		c.logf("could not record artifacts for %s: %v", shortID(t.ID), err)
	}

	// Step 7: commit the attempt as immutable state.
	att := cell.Attempt{
		CellID:      c.Capabilities.CellID,
		Task:        t.ID,
		N:           n,
		Change:      change,
		Environment: envHash,
		CreatedAt:   c.now().Unix(),
		Note:        fmt.Sprintf("%d file(s) changed", len(written)),
	}
	for _, a := range artifacts {
		att.Artifacts = append(att.Artifacts, a.ContentHash)
	}
	ref, err := c.writeAttempt(att)
	if err != nil {
		return AttemptResult{}, err
	}

	// Step 8: pin what upstream should retain.
	if change != "" {
		if err := c.pin(change); err != nil {
			c.logf("could not pin %s: %v", shortHash(change), err)
		}
	}

	return AttemptResult{
		Task: t.ID, N: n, Change: change, Ref: ref,
		Environment: envHash, Evidence: evidence, Artifacts: artifacts, Cost: cost,
	}, nil
}

// writeAttempt creates the attempt ref. Create-only: an attempt ref is written
// once and never moved (CELL.md §2), and the create-only CAS is what enforces
// it — which is what makes duplicate attempts survive a reconnect without any
// merge algorithm.
func (c *Cell) writeAttempt(att cell.Attempt) (string, error) {
	if err := att.Validate(); err != nil {
		return "", err
	}
	payload, err := cell.Canonical(att)
	if err != nil {
		return "", err
	}
	id, err := c.V.PutBlob(payload)
	if err != nil {
		return "", err
	}
	ref, err := cell.AttemptRef(att.CellID, att.Task, att.N)
	if err != nil {
		return "", err
	}
	if err := c.V.UpdateRef(ref, id, ""); err != nil {
		return "", fmt.Errorf("writing attempt %s: %w", ref, err)
	}
	return ref, nil
}

// pin asks upstream to retain an attempt's state (§5 step 8). A pin only ever
// writes under refs/pins/<peer>/, so it grants disk, never promotion
// (FEDERATION.md §4) — a peer honouring a pin is lending storage, not delegating
// authority.
//
// The ref name encodes the expiry, so the pin is idempotent for as long as it
// lasts and lapses on its own afterwards. That is why there is no renewal path
// here: a cell that still wants the state retained pins it again next pass, and
// a cell that has stopped caring stops pinning, which is exactly the behaviour
// an expiring pin gives for free.
func (c *Cell) pin(change string) error {
	ref, err := cell.PinRef(c.Capabilities.CellID, c.now().Add(pinTTL).Unix(), change)
	if err != nil {
		return err
	}
	if _, err := c.V.ResolveRef(ref); err == nil {
		return nil
	} else if !errors.Is(err, varvigcli.ErrNoRef) {
		return err
	}
	return c.V.UpdateRef(ref, change, "")
}

// runChecks executes the cell's checks and produces evidence plus the
// environment they ran in.
//
// The environment deliberately excludes the model fragment: build and test
// evidence has no model, and inventing one would make deterministic evidence
// look sampled (CELL.md §4.2).
func (c *Cell) runChecks(ctx context.Context, dir, taskID, change string) (cell.Environment, cell.Evidence, error) {
	env, err := c.checkEnvironment(ctx)
	if err != nil {
		return cell.Environment{}, cell.Evidence{}, err
	}
	ev := cell.Evidence{
		Attempt:    change,
		Task:       taskID,
		CellID:     c.Capabilities.CellID,
		ProducedAt: c.now().Unix(),
	}
	for _, chk := range c.checksFor() {
		slot := c.Ledger.AcquireVerify()
		if !slot.OK {
			// Saturated slots are not a check failure: recording one as StatusFail
			// would report broken code because the cell was busy.
			ev.Checks = append(ev.Checks, cell.Check{Name: chk.Name, Status: cell.StatusSkip, Detail: string(slot.Reason)})
			continue
		}
		res, err := c.Sandbox.Run(ctx, sandbox.Job{Name: chk.Name, Dir: dir, Command: chk.Command, Timeout: chk.Timeout})
		c.Ledger.ReleaseVerify()
		if err != nil {
			ev.Checks = append(ev.Checks, cell.Check{Name: chk.Name, Status: cell.StatusError, Detail: err.Error()})
			continue
		}
		ev.Checks = append(ev.Checks, res.Check())
	}
	if len(ev.Checks) == 0 {
		// A cell with no checks configured produces no evidence rather than
		// empty evidence that would satisfy a naive "has evidence?" test.
		ev.Checks = []cell.Check{{Name: "no-checks-configured", Status: cell.StatusSkip,
			Detail: "this cell has no checks configured, so it asserts nothing about this attempt"}}
	}
	ev.Normalize()
	return env, ev, nil
}

// checksFor returns the checks this cell's roles permit it to run.
func (c *Cell) checksFor() []Check {
	var out []Check
	for _, chk := range c.Checks {
		switch chk.Kind {
		case cell.RoleBuild:
			if c.Capabilities.Has(cell.RoleBuild) {
				out = append(out, chk)
			}
		case cell.RoleVerify, "":
			if c.Capabilities.Has(cell.RoleVerify) {
				out = append(out, chk)
			}
		}
	}
	return out
}

// checkEnvironment merges the sandbox and artifact-store fragments — no model.
func (c *Cell) checkEnvironment(ctx context.Context) (cell.Environment, error) {
	var frags []cell.Fragment
	if c.Sandbox != nil {
		f, err := c.Sandbox.Fragment(ctx)
		if err != nil {
			return cell.Environment{}, err
		}
		frags = append(frags, f)
	}
	if c.Artifacts != nil {
		f, err := c.Artifacts.Fragment(ctx)
		if err != nil {
			return cell.Environment{}, err
		}
		frags = append(frags, f)
	}
	return cell.MergeFragments(frags...)
}

// recordEnvironment writes an environment descriptor as a note and returns its
// hash. The note attaches to the attempt's change so it travels with the thing
// it describes; when there is no change it attaches to nothing and only the hash
// is returned, which is enough for the evidence record to name it.
func (c *Cell) recordEnvironment(target string, env cell.Environment) (string, error) {
	hash, err := env.Hash()
	if err != nil {
		return "", err
	}
	if target == "" {
		return hash, nil
	}
	payload, err := cell.Canonical(env)
	if err != nil {
		return "", err
	}
	// Environments deduplicate by hash: thousands of evidence records sharing an
	// environment should not write thousands of identical notes.
	existing, err := c.V.Notes(target, cell.NoteEnvironment)
	if err == nil {
		for _, n := range existing {
			if string(n.Payload) == string(payload) {
				return hash, nil
			}
		}
	}
	return hash, c.V.AddNote(target, cell.NoteEnvironment, payload)
}

func (c *Cell) recordEvidence(target string, ev cell.Evidence) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	payload, err := cell.Canonical(ev)
	if err != nil {
		return err
	}
	return c.V.AddNote(target, cell.NoteEvidence, payload)
}

// recordArtifacts is step 6.
func (c *Cell) recordArtifacts(ctx context.Context, dir, change string) ([]cell.ArtifactRef, error) {
	if c.Artifacts == nil || len(c.ArtifactGlobs) == 0 {
		return nil, nil
	}
	var out []cell.ArtifactRef
	for _, glob := range c.ArtifactGlobs {
		matches, err := filepath.Glob(filepath.Join(dir, glob))
		if err != nil {
			return out, err
		}
		sort.Strings(matches)
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || info.IsDir() {
				continue
			}
			ref, err := c.Artifacts.Put(ctx, m)
			if err != nil {
				return out, err
			}
			ref.ProducedBy = change
			ref.Normalize()
			if err := ref.Validate(); err != nil {
				return out, err
			}
			if change != "" {
				payload, err := cell.Canonical(ref)
				if err != nil {
					return out, err
				}
				if err := c.V.AddNote(change, cell.NoteArtifact, payload); err != nil {
					return out, err
				}
			}
			out = append(out, ref)
		}
	}
	return out, nil
}

// readContext reads the ticket's read set out of the checkout, as supporting
// material for the model. It reads only inside the checkout varvig materialized,
// which is the scope: the read set doubles as the capability boundary
// (TICKETS.md §3.1), so there is nothing here to enforce separately.
func (c *Cell) readContext(dir string, scope varvigcli.Scope) []inference.ContextFile {
	var out []inference.ContextFile
	for _, p := range scope.Reads {
		full := filepath.Join(dir, p)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir() {
			entries, err := os.ReadDir(full)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if b, err := os.ReadFile(filepath.Join(full, e.Name())); err == nil {
					out = append(out, inference.ContextFile{Path: filepath.Join(p, e.Name()), Content: string(b)})
				}
			}
			continue
		}
		if b, err := os.ReadFile(full); err == nil {
			out = append(out, inference.ContextFile{Path: p, Content: string(b)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// relieveStorage is FACTORY.md §7's storage rule, in its stated order: **pin
// release before GC**.
//
// The order is the whole point. A cell that let its disk fill while still
// holding pins has silently promised to retain state it can no longer hold, and
// a peer evaluating that state finds it gone. So the cell drops its own
// retention obligations deliberately — release local bytes, then release pins,
// then let varvig sweep — rather than collecting state someone else is
// evaluating until something breaks.
func (c *Cell) relieveStorage() error {
	local := c.localCAS()
	if local == nil {
		return nil
	}
	relief, err := budget.RelieveStoragePressure(c.Ledger.Budget(), &casReleaser{cas: local})
	if err != nil {
		return err
	}
	if len(relief.Released) > 0 {
		c.logf("released %d local artifact(s) under storage pressure (%d -> %d bytes, cap %d)",
			len(relief.Released), relief.Before, relief.After, relief.Cap)
	}
	if !relief.StillOver {
		return nil
	}

	// Local bytes were not enough. Drop retention obligations next, then sweep:
	// releasing a pin is what makes the objects behind it collectable at all, so
	// doing it after the sweep would free nothing until the following pass.
	released, perr := c.releasePins(pinsPerReliefPass)
	if released > 0 {
		c.logf("released %d pin(s) to relieve storage pressure; the state behind them is no longer retained by this cell", released)
	}
	if perr != nil {
		return fmt.Errorf("releasing pins: %w", perr)
	}
	report, gerr := c.V.GC(true)
	if gerr != nil {
		return fmt.Errorf("gc: %w", gerr)
	}
	// varvig reports unreachable external artifacts; it never deletes registry
	// bytes, because it holds no credentials there. Deletion is this cell's or
	// the operator's action (§8), so the report is surfaced rather than acted on
	// silently.
	for _, a := range report.ExternalUnreachable {
		c.logf("external artifact now unreachable: %s %s %v (deletion is Factory's or the operator's action, never varvig's)",
			a.ContentHash, a.MediaType, a.Locators)
	}
	if released == 0 {
		return fmt.Errorf("over the storage cap with nothing releasable: %d bytes against a cap of %d; the cell will keep claiming into a full disk",
			relief.After, relief.Cap)
	}
	return nil
}

// pinsPerReliefPass bounds how many retention obligations one pass drops. It is
// bounded rather than "as many as needed" so that a misconfigured cap cannot make
// a single pass abandon everything the cell was retaining for the federation.
const pinsPerReliefPass = 16

// localCAS finds the cell-local store behind whatever artifact store is
// configured, since only local bytes are this cell's to release.
func (c *Cell) localCAS() *artifact.LocalCAS {
	switch store := c.Artifacts.(type) {
	case *artifact.LocalCAS:
		return store
	case *artifact.Remote:
		return store.Local
	default:
		return nil
	}
}

// Summary renders a report as one line plus the skips.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "observed=%d claimed=%d attempts=%d verified=%d promotions=%d",
		r.Observed, len(r.Claimed), len(r.Attempts), len(r.Verified), len(r.Promotions))
	if r.Offline {
		b.WriteString(" offline")
	}
	reasons := make([]string, 0, len(r.Skipped))
	for reason := range r.Skipped {
		reasons = append(reasons, string(reason))
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(&b, " skip[%s]=%d", reason, r.Skipped[claim.SkipReason(reason)])
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "\n  error: %s", e)
	}
	return b.String()
}

// taskScope renders a ticket's read set as the scope string varvig's task
// credential takes. It is the declared read set, unmodified: narrowing or
// widening it here would be Factory second-guessing varvig's own boundary.
func taskScope(s varvigcli.Scope) string {
	if len(s.Reads) == 1 {
		return s.Reads[0]
	}
	if len(s.Reads) == 0 {
		return "/"
	}
	// A multi-path read set has no single scope string in varvig's task grant, so
	// the cell asks for the shared prefix of the declared paths. That is wider
	// than any single path and never wider than the declaration's own common
	// root, so it cannot admit a path the ticket did not name a parent of.
	return commonPrefix(s.Reads)
}

func commonPrefix(paths []string) string {
	if len(paths) == 0 {
		return "/"
	}
	segs := strings.Split(strings.Trim(paths[0], "/"), "/")
	for _, p := range paths[1:] {
		other := strings.Split(strings.Trim(p, "/"), "/")
		n := 0
		for n < len(segs) && n < len(other) && segs[n] == other[n] {
			n++
		}
		segs = segs[:n]
	}
	if len(segs) == 0 {
		return "/"
	}
	return strings.Join(segs, "/") + "/"
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func shortID(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}

func shortHash(s string) string { return shortID(s) }

// casReleaser adapts a LocalCAS to budget.Releaser. It releases local bytes
// only: a pin names a varvig object, an artifact does not, so the two
// obligations are dropped by different steps of relieveStorage rather than
// conflated here.
type casReleaser struct{ cas *artifact.LocalCAS }

func (r *casReleaser) UsedBytes() (int64, error) { return r.cas.UsedBytes(context.Background()) }

func (r *casReleaser) Candidates() ([]string, error) {
	refs, err := r.cas.List()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, a := range refs {
		out = append(out, a.ContentHash)
	}
	return out, nil
}

func (r *casReleaser) Release(contentHash string) error {
	return r.cas.Release(context.Background(), cell.ArtifactRef{ContentHash: contentHash})
}
