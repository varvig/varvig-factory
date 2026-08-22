package promote

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/gate"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Condition names the §6.3 requirements. They are named constants because a
// refusal has to say which one failed — "not promotable" sends an operator to
// read the source, and that is the moment they start looking for the override.
type Condition string

// The conditions. The first five are FACTORY.md §6.3 verbatim, in its order. The
// last two are not additions to the policy: they are the surrounding facts
// without which the five would be checking a promotion that could not happen
// anyway.
const (
	// CondIndependentEvidence is §6.3.1: evidence must come from a cell other
	// than the attempting cell. Self-verified attempts are never autonomously
	// promotable.
	CondIndependentEvidence Condition = "independent-evidence"
	// CondEnvironmentClass is §6.3.2: the environment class must match the
	// declared baseline for that path. Cross-class comparison defers to a human.
	CondEnvironmentClass Condition = "environment-class"
	// CondReverified is §6.3.3: re-verification before promotion, not merely
	// evidence replay.
	CondReverified Condition = "re-verified"
	// CondPathEnabled is §6.3.4: the path scope must be explicitly enabled.
	CondPathEnabled Condition = "path-scope-enabled"
	// CondAgreementMetric is §6.3.5: a promotion-agreement metric must exist for
	// the scope, above threshold.
	CondAgreementMetric Condition = "agreement-metric"

	// CondTrustGrant is the trust store actually granting `promote` at this path
	// to this cell's key. Without it the promotion would be refused by varvig
	// anyway; checking it here turns a confusing downstream rejection into a
	// clear local one, and makes revoking the allowed_keys line sufficient to
	// stop promotion (§6.5).
	CondTrustGrant Condition = "trust-grant"
	// CondGateVerdict is the wasm policy module's verdict (§6.2).
	CondGateVerdict Condition = "gate-verdict"
)

// Reverifier runs the checks again, now, against the attempt.
//
// This is §6.3 condition 3 and it is a distinct interface rather than a flag
// because the distinction it protects is easy to lose: replaying a stored
// evidence record proves that a cell once said the tests passed. Re-verification
// proves they pass against the state about to be promoted. Only the second is
// worth anything at the moment of promotion, and only the second costs anything
// — which is exactly why an implementation would drift toward the first.
type Reverifier interface {
	// Reverify produces fresh evidence for an attempt. The returned evidence
	// must carry this cell's id and the environment it actually ran in.
	Reverify(ctx context.Context, attempt cell.Attempt) (cell.Evidence, error)
}

// Request is one promotion decision.
type Request struct {
	// Attempt is the candidate.
	Attempt cell.Attempt
	// Evidence is every evidence record known for the attempt, from every cell.
	Evidence []cell.Evidence
	// Environments maps environment hash to descriptor, for the evidence above
	// and for the attempt.
	Environments map[string]cell.Environment
	// Baseline is the declared environment baseline for the path, if any.
	Baseline *cell.Environment
	// Scope is the path scope this promotion writes to.
	Scope string
	// Ticket is the varvig ticket id, and TicketObject the object its ref
	// resolves to — the anchor agreement observations attach to.
	Ticket, TicketObject string
	// Ref is the ref to promote onto. Empty means varvig's default (HEAD).
	Ref string
}

// Outcome is what a promotion decision did and why.
type Outcome struct {
	Mode Mode
	// Promoted is true only if this cell actually moved the ref.
	Promoted bool
	// Change is what was promoted, when Promoted is true.
	Change string
	// Failed lists every condition that was not satisfied, in the order the
	// conditions are declared. Every one is reported, not just the first: an
	// operator fixing one blocker wants to know about the other three now, not
	// after the next run.
	Failed []Failure
	// Gate is the policy module's result, when a module ran.
	Gate gate.Result
	// GateRan reports whether a module was bound and ran at all.
	GateRan bool
	// Reverification is the fresh evidence produced by condition 3, when it ran.
	Reverification *cell.Evidence
}

// Failure is one unsatisfied condition.
type Failure struct {
	Condition Condition
	Detail    string
}

func (f Failure) String() string { return string(f.Condition) + ": " + f.Detail }

// DeferredToHuman reports whether the outcome leaves the decision with a human.
// That is the state of every gated run and of every autonomous run that did not
// satisfy all conditions — the two are the same outcome and are reported as one.
func (o Outcome) DeferredToHuman() bool { return !o.Promoted }

// Summary renders the outcome for a log line or a CLI.
func (o Outcome) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s ", o.Mode)
	if o.Promoted {
		fmt.Fprintf(&b, "promoted=%s", short(o.Change))
	} else {
		b.WriteString("promoted=no deferred-to-human")
	}
	if o.GateRan {
		fmt.Fprintf(&b, " gate=%s", o.Gate.Verdict)
	} else {
		b.WriteString(" gate=none")
	}
	for _, f := range o.Failed {
		fmt.Fprintf(&b, "\n  unmet %s", f)
	}
	return b.String()
}

// Promoter evaluates and, when every condition is met, performs promotion.
type Promoter struct {
	V         varvigcli.Varvig
	Switch    *Switch
	Gate      gate.Module
	Agreement agreement.Gate
	Reverify  Reverifier
	// CellID is this cell's id, used for the independence check.
	CellID string
	// Fingerprint is this cell's key fingerprint, matched against the trust
	// store. Empty skips the trust check with a recorded failure rather than
	// silently passing it — a cell that does not know its own key cannot claim a
	// grant.
	Fingerprint string
	// Now defaults to time.Now.
	Now func() time.Time
	// Log receives one line per decision. Optional, and separate from the
	// returned Outcome so that a gated cell running the gate for measurement
	// (§10.9 — the module runs and logs its verdict without acting) has
	// somewhere to put it.
	Log func(string)
}

func (p *Promoter) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Promoter) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(fmt.Sprintf(format, args...))
	}
}

// Promote evaluates a request and promotes if and only if every condition holds.
//
// There is one path. In gated mode the mode check is simply one more unmet
// condition, so a gated cell runs the gate module, evaluates every other
// condition, records the verdict — and does not act. That is §10.9's "the module
// runs and logs its verdict without acting", and it is also how the autonomous
// path stays exercised long before anyone enables it.
func (p *Promoter) Promote(ctx context.Context, req Request) (Outcome, error) {
	mode, err := p.Switch.Mode()
	if err != nil {
		return Outcome{Mode: ModeGated}, err
	}
	out := Outcome{Mode: mode}

	// Condition 4 first, because it is the cheapest and because its failure is
	// the most common and the least alarming: most paths are not enabled, and
	// that is the intended state.
	enabling, enabled, err := p.Switch.EnabledFor(req.Scope)
	if err != nil {
		return out, err
	}
	if !enabled {
		out.Failed = append(out.Failed, Failure{CondPathEnabled,
			fmt.Sprintf("autonomous promotion is not enabled for scope %q", req.Scope)})
	}

	// Condition 1: independent evidence.
	independent := false
	var evidenceCells []string
	for _, e := range req.Evidence {
		if e.CellID != "" {
			evidenceCells = append(evidenceCells, e.CellID)
		}
		if e.Independent(req.Attempt.CellID) && e.Passed() {
			independent = true
		}
	}
	if !independent {
		sort.Strings(evidenceCells)
		detail := "no passing evidence from a cell other than the attempting cell " + req.Attempt.CellID
		if len(evidenceCells) > 0 {
			detail += " (evidence from: " + strings.Join(dedupe(evidenceCells), ", ") + ")"
		} else {
			detail += " (no evidence at all)"
		}
		out.Failed = append(out.Failed, Failure{CondIndependentEvidence, detail})
	}

	// Condition 2: environment class against the declared baseline.
	if err := p.checkEnvironmentClass(req); err != nil {
		out.Failed = append(out.Failed, Failure{CondEnvironmentClass, err.Error()})
	}

	// Condition 5: the agreement metric for this scope.
	rate, err := agreement.RateFor(p.V, req.Scope)
	if err != nil {
		return out, err
	}
	verdict := p.Agreement.Allow(rate)
	if !verdict.Allowed {
		out.Failed = append(out.Failed, Failure{CondAgreementMetric, verdict.Reason})
	}

	// The trust store: does this cell's key actually hold promote here?
	if err := p.checkTrust(req.Scope); err != nil {
		out.Failed = append(out.Failed, Failure{CondTrustGrant, err.Error()})
	}

	// The gate module. It runs in every mode — that is how a gated cell measures
	// its policy before trusting it.
	in := p.gateInput(req, mode, rate, independent)
	gateResult, gateErr := p.Gate.Evaluate(ctx, in)
	out.Gate = gateResult
	switch {
	case errors.Is(gateErr, gate.ErrNoModule):
		// An unconfigured gate is not an approving gate (§6.2): the promotion
		// rule must be a reviewed, versioned object, and there is not one.
		out.Failed = append(out.Failed, Failure{CondGateVerdict,
			"no promotion-policy module is bound; an unconfigured gate is not an approving gate"})
	case gateErr != nil:
		out.GateRan = true
		out.Failed = append(out.Failed, Failure{CondGateVerdict,
			"policy module could not be run: " + gateErr.Error()})
	default:
		out.GateRan = true
		if gateResult.Verdict != gate.Promote {
			out.Failed = append(out.Failed, Failure{CondGateVerdict,
				fmt.Sprintf("policy module returned %s: %s", gateResult.Verdict, gateResult.Reason())})
		}
	}

	// Condition 3: re-verification. It runs last of the checks, because it is
	// the only expensive one — there is no point re-running a test suite for an
	// attempt that is already disqualified on four other grounds. It runs even
	// in gated mode when everything else holds, so that the cost of autonomy is
	// measured before it is relied on.
	if len(out.Failed) == 0 || (len(out.Failed) == 1 && out.Failed[0].Condition == CondPathEnabled && mode == ModeGated) {
		fresh, rerr := p.reverify(ctx, req)
		if rerr != nil {
			out.Failed = append(out.Failed, Failure{CondReverified, rerr.Error()})
		} else {
			out.Reverification = &fresh
		}
	} else {
		out.Failed = append(out.Failed, Failure{CondReverified,
			"skipped: other conditions already unmet, so re-verification would only cost time"})
	}

	// Mode is the last input, checked like any other. Deliberately not a branch
	// around everything above: that is what keeps the autonomous path exercised.
	if mode != ModeAutonomous {
		out.Failed = append(out.Failed, Failure{Condition("mode"),
			fmt.Sprintf("cell is running in %s mode; a human decides", mode)})
	}

	sortFailures(out.Failed)
	if len(out.Failed) > 0 {
		p.logf("promotion deferred for %s: %s", short(req.Attempt.Change), out.Summary())
		return out, nil
	}

	// Every condition holds. Promote through varvig, which applies its own
	// promotion checkpoint on top — the veto gate and the repository's policy
	// module (TICKETS.md §4). Factory's gate is an additional constraint, never
	// a replacement for varvig's, so a veto still stops this.
	change, err := p.V.SpecPromote(req.Ticket, req.Ref)
	if err != nil {
		return out, fmt.Errorf("promote: varvig refused the promotion: %w", err)
	}
	out.Promoted, out.Change = true, change
	p.logf("promoted %s onto %s (scope %s enabled by %s)", short(change), req.Ref, req.Scope, enabling)
	return out, nil
}

// reverify runs condition 3 and checks that the fresh evidence is usable.
func (p *Promoter) reverify(ctx context.Context, req Request) (cell.Evidence, error) {
	if p.Reverify == nil {
		return cell.Evidence{}, errors.New("no re-verifier configured; §6.3 requires re-verification before promotion, not evidence replay")
	}
	fresh, err := p.Reverify.Reverify(ctx, req.Attempt)
	if err != nil {
		return cell.Evidence{}, fmt.Errorf("re-verification failed to run: %w", err)
	}
	if err := fresh.Validate(); err != nil {
		return cell.Evidence{}, fmt.Errorf("re-verification produced unusable evidence: %w", err)
	}
	if !fresh.Passed() {
		return cell.Evidence{}, fmt.Errorf("re-verification did not pass: %s", strings.Join(fresh.Failures(), " "))
	}
	// Fresh evidence that is simply the stored record handed back would satisfy
	// the letter of condition 3 and none of its purpose. A re-verifier that
	// returns evidence timestamped before the attempt is replaying, so say so.
	if req.Attempt.CreatedAt > 0 && fresh.ProducedAt > 0 && fresh.ProducedAt < req.Attempt.CreatedAt {
		return cell.Evidence{}, fmt.Errorf("re-verification returned evidence produced before the attempt (%d < %d); that is replay, not re-verification",
			fresh.ProducedAt, req.Attempt.CreatedAt)
	}
	return fresh, nil
}

// checkEnvironmentClass is §6.3 condition 2. Cross-class comparison must defer
// to a human, and — the case that is easy to get wrong — so must a *missing*
// environment: unknown class never matches (CELL.md §4.4).
func (p *Promoter) checkEnvironmentClass(req Request) error {
	if req.Baseline == nil || req.Baseline.Empty() {
		return fmt.Errorf("no environment baseline is declared for scope %q; without one there is nothing to match against", req.Scope)
	}
	// Every piece of passing, independent evidence must be same-class with the
	// baseline. Checking only one would let a cell pass by supplying a single
	// conforming record alongside a pile of cross-class ones.
	checked := 0
	for _, e := range req.Evidence {
		if !e.Passed() || !e.Independent(req.Attempt.CellID) {
			continue
		}
		env, ok := req.Environments[e.Environment]
		if !ok || env.Empty() {
			return fmt.Errorf("evidence from %s names no known environment; unknown class never matches", e.CellID)
		}
		if class := cell.CompareClass(env, *req.Baseline); class != cell.ClassSame {
			return fmt.Errorf("evidence from %s is %s with the baseline for %q", e.CellID, class, req.Scope)
		}
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("no passing independent evidence to compare against the baseline")
	}
	return nil
}

// checkTrust asks the repository trust store whether this cell may promote here.
// Revoking the allowed_keys line is the federation-wide kill switch (§6.5), so
// this check is what makes the revocation bite locally too, immediately.
func (p *Promoter) checkTrust(scope string) error {
	if p.Fingerprint == "" {
		return errors.New("this cell's key fingerprint is not configured, so no promote grant can be verified")
	}
	entries, err := p.V.TrustList()
	if err != nil {
		return fmt.Errorf("could not read the trust store: %v", err)
	}
	for _, e := range entries {
		if e.Fingerprint != p.Fingerprint {
			continue
		}
		if e.Can("promote", scope) {
			return nil
		}
	}
	return fmt.Errorf("the trust store grants this key no promote right at %q", scope)
}

func (p *Promoter) gateInput(req Request, mode Mode, rate agreement.Rate, independent bool) gate.Input {
	in := gate.Input{
		Attempt:      req.Attempt,
		Evidence:     req.Evidence,
		Environments: req.Environments,
		Baseline:     req.Baseline,
		Scope:        req.Scope,
		Mode:         string(mode),
		Independent:  independent,
		Agreement: gate.AgreementView{
			Observations: rate.Observations,
			Agreements:   rate.Agreements,
			Rate:         rate.Value(),
		},
	}
	if env, ok := req.Environments[req.Attempt.Environment]; ok {
		in.AttemptEnvironment = &env
	}
	if status, err := p.V.TicketStatus(req.Ticket); err == nil {
		in.TicketStatus = status
	}
	return in
}

// ObservePromotion records whether the highest-scoring attempt was the one
// promoted (§6.4). It is called after a promotion — by a human through the CLI
// or the app, or by this cell — and it is the measurement that licenses autonomy
// later.
//
// It reads the pool and the ref rather than being told the answer, so the
// observation records what varvig's scoring and varvig's ref actually said. A
// caller-supplied "top" would let the measured thing be whatever the caller
// believed.
func (p *Promoter) ObservePromotion(req Request) (agreement.Observation, error) {
	props, err := p.V.Proposals(req.Ticket)
	if err != nil {
		return agreement.Observation{}, err
	}
	top, ok := topScoring(props)
	if !ok {
		return agreement.Observation{}, fmt.Errorf("promote: no scored candidates for %s; an unscored pool is not a disagreement", short(req.Ticket))
	}
	ref := req.Ref
	if ref == "" {
		return agreement.Observation{}, errors.New("promote: observing a promotion needs the ref that was promoted onto")
	}
	promoted, err := p.V.ResolveRef(ref)
	if err != nil {
		return agreement.Observation{}, err
	}
	obs := agreement.Observe(req.Scope, req.Ticket, top, promoted, p.now())
	if err := agreement.Record(p.V, req.TicketObject, obs); err != nil {
		return obs, err
	}
	p.logf("agreement observed for %s: top=%s promoted=%s agreed=%t",
		short(req.Ticket), short(top), short(promoted), obs.Agreed)
	return obs, nil
}

// topScoring returns the highest-scoring candidate. Only scored candidates
// count: an unscored pool has expressed no opinion, and treating an arbitrary
// member of it as "the top" would manufacture agreement or disagreement out of
// nothing.
func topScoring(props []varvigcli.Proposal) (string, bool) {
	best, found := varvigcli.Proposal{}, false
	for _, p := range props {
		if !p.Scored {
			continue
		}
		if !found || p.Score > best.Score {
			best, found = p, true
		}
	}
	return best.Change, found
}

// conditionOrder fixes the reporting order of failures, so two runs with the
// same unmet conditions read identically.
var conditionOrder = map[Condition]int{
	CondIndependentEvidence: 1,
	CondEnvironmentClass:    2,
	CondReverified:          3,
	CondPathEnabled:         4,
	CondAgreementMetric:     5,
	CondTrustGrant:          6,
	CondGateVerdict:         7,
	Condition("mode"):       8,
}

func sortFailures(f []Failure) {
	sort.SliceStable(f, func(i, j int) bool {
		return conditionOrder[f[i].Condition] < conditionOrder[f[j].Condition]
	})
}

func dedupe(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i > 0 && s == in[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func short(h string) string {
	if len(h) <= 20 {
		return h
	}
	return h[:20] + "…"
}
