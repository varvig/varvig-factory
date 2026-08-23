// Package agreement measures the promotion-agreement rate: while running gated,
// for each promoted ticket, whether the highest-scoring attempt was the one the
// human promoted (FACTORY.md §6.4).
//
// That rate is the only honest basis for enabling autonomous promotion, and it
// is per-module rather than global — a cell may be trustworthy at generating
// serializers and useless at touching billing, and a global average hides
// exactly that.
//
// The risk being managed is varvig-design.md §5: the moment promotion is
// automatic, the test suite silently becomes the real source of truth, and
// speculation scoring will find whatever it does not check. Autonomy is not
// forbidden here — it is earned per scope, with evidence.
package agreement

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Observation is one recorded comparison between what scoring ranked first and
// what was actually promoted (CELL.md §9).
//
// Note what it does not record: who promoted, or why. The metric is about the
// scorer's calibration, not about auditing a reviewer — varvig's attestations
// already carry the decision and its author, and duplicating that here would
// create a second, unsigned account of the same event.
type Observation struct {
	Scope string `json:"scope"`
	Task  string `json:"task"`
	// TopAttempt is the change the pool scored highest at promotion time.
	TopAttempt string `json:"top_attempt"`
	// PromotedAttempt is the change that was actually promoted.
	PromotedAttempt string `json:"promoted_attempt"`
	Agreed          bool   `json:"agreed"`
	ObservedAt      int64  `json:"observed_at"`
}

// Observe builds an observation. Agreement is exact identity of the change, not
// a similarity judgement: "close enough" is precisely the kind of softening that
// would let a scorer look calibrated while consistently ranking second-best
// first.
func Observe(scope, task, top, promoted string, now time.Time) Observation {
	return Observation{
		Scope:           scope,
		Task:            task,
		TopAttempt:      top,
		PromotedAttempt: promoted,
		Agreed:          top != "" && top == promoted,
		ObservedAt:      now.Unix(),
	}
}

// Validate rejects an observation that cannot mean anything.
func (o Observation) Validate() error {
	if o.Scope == "" {
		return fmt.Errorf("agreement: observation has no scope; the rate is per-module, never global")
	}
	if o.PromotedAttempt == "" {
		return fmt.Errorf("agreement: observation for %s records no promoted attempt", o.Task)
	}
	if o.TopAttempt == "" {
		// A promotion with an unscored pool is not a disagreement — there was
		// nothing to disagree with. Recording it as one would drag the rate down
		// for a reason that has nothing to do with the scorer.
		return fmt.Errorf("agreement: observation for %s records no top-scoring attempt; an unscored pool is not a disagreement", o.Task)
	}
	return nil
}

// Rate is the agreement rate over a scope.
type Rate struct {
	Scope        string
	Observations int
	Agreements   int
}

// Value is the rate in [0,1]. An empty sample is zero rather than one: no
// evidence of agreement is not evidence of agreement.
func (r Rate) Value() float64 {
	if r.Observations == 0 {
		return 0
	}
	return float64(r.Agreements) / float64(r.Observations)
}

func (r Rate) String() string {
	return fmt.Sprintf("%s  %d/%d  %.0f%%", r.Scope, r.Agreements, r.Observations, r.Value()*100)
}

// DefaultThreshold is the agreement rate below which autonomous mode refuses to
// enable (§6.4).
const DefaultThreshold = 0.80

// DefaultMinObservations is the smallest sample the gate accepts.
//
// FACTORY.md §6.4 names a threshold but not a sample size. A threshold alone is
// not enough: one ticket promoted in agreement is a rate of 100%, and it would
// unlock autonomous promotion for a scope on the strength of a single
// observation. The spec's own argument — that autonomy is *earned with
// evidence* — is what requires a floor here, so twenty is the default and it is
// configurable. It is a judgement, and it is stated rather than buried.
const DefaultMinObservations = 20

// Gate decides whether autonomous promotion may be enabled for a scope.
type Gate struct {
	Threshold       float64
	MinObservations int
}

// NewGate returns a gate with the defaults filled in.
func NewGate(threshold float64, minObservations int) Gate {
	g := Gate{Threshold: threshold, MinObservations: minObservations}
	if g.Threshold <= 0 {
		g.Threshold = DefaultThreshold
	}
	if g.MinObservations <= 0 {
		g.MinObservations = DefaultMinObservations
	}
	return g
}

// Verdict is the gate's answer, carrying the reason so a refusal can say why
// (§6.4: "should refuse to enable and say why").
type Verdict struct {
	Allowed bool
	Rate    Rate
	Reason  string
}

func (v Verdict) String() string {
	if v.Allowed {
		return fmt.Sprintf("autonomous promotion permitted for %s: %s", v.Rate.Scope, v.Rate)
	}
	return fmt.Sprintf("autonomous promotion refused for %s: %s", v.Rate.Scope, v.Reason)
}

// Allow evaluates the gate against a measured rate.
func (g Gate) Allow(r Rate) Verdict {
	switch {
	case r.Observations == 0:
		return Verdict{Rate: r, Reason: fmt.Sprintf(
			"no promotion-agreement metric exists for this scope yet; run gated until there are at least %d recorded promotions",
			g.MinObservations)}
	case r.Observations < g.MinObservations:
		return Verdict{Rate: r, Reason: fmt.Sprintf(
			"only %d recorded promotions for this scope (need %d); a rate over %d is not a measurement",
			r.Observations, g.MinObservations, r.Observations)}
	case r.Value() < g.Threshold:
		return Verdict{Rate: r, Reason: fmt.Sprintf(
			"agreement rate %.0f%% is below the %.0f%% threshold (%d of %d promotions matched the top-scoring attempt)",
			r.Value()*100, g.Threshold*100, r.Agreements, r.Observations)}
	}
	return Verdict{Allowed: true, Rate: r}
}

// Record writes an observation as a note in the factory/agreement namespace.
//
// The note attaches to the ticket's object rather than to the promoted change,
// so that reading a scope's history is a walk over tickets — which a cell can
// enumerate — instead of a search for changes it would have to already know
// about. Notes replicate by default (FEDERATION.md §3), so an observation made
// by one cell is available to the federation without a new protocol.
func Record(v varvigcli.Varvig, ticketObject string, o Observation) error {
	if err := o.Validate(); err != nil {
		return err
	}
	payload, err := cell.Canonical(o)
	if err != nil {
		return err
	}
	return v.AddNote(ticketObject, cell.NoteAgreement, payload)
}

// Observations reads every agreement observation in the repository, by walking
// the ticket namespace.
//
// It is O(tickets) reads. That is acceptable and deliberate: the alternative is
// a locally cached index, and a local index is precisely the kind of state that
// makes two cells disagree about a federated fact.
func Observations(v varvigcli.Varvig) ([]Observation, error) {
	ids, err := v.TicketIDs()
	if err != nil {
		return nil, err
	}
	refs, err := v.Refs()
	if err != nil {
		return nil, err
	}
	byTicket := map[string]string{}
	for _, r := range refs {
		byTicket[r.Name] = r.Hash
	}

	var out []Observation
	for _, id := range ids {
		target, ok := byTicket["refs/varvig/tickets/"+id]
		if !ok {
			continue
		}
		notes, err := v.Notes(target, cell.NoteAgreement)
		if err != nil {
			return nil, err
		}
		for _, n := range notes {
			var o Observation
			if err := json.Unmarshal(n.Payload, &o); err != nil {
				// A malformed note is skipped rather than fatal: a peer running
				// a newer Factory may write a shape this build does not know,
				// and refusing to compute any rate because one note is
				// unfamiliar would make the metric fragile across versions.
				continue
			}
			if o.Validate() != nil {
				continue
			}
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObservedAt != out[j].ObservedAt {
			return out[i].ObservedAt < out[j].ObservedAt
		}
		return out[i].Task < out[j].Task
	})
	return out, nil
}

// Tally aggregates observations by scope, exactly as recorded. Scopes are not
// rolled up into parents: "src/" and "src/generated/" are different modules with
// different risk, and averaging them is how a good record in one place licenses
// autonomy in another.
func Tally(obs []Observation) map[string]Rate {
	out := map[string]Rate{}
	for _, o := range obs {
		r := out[o.Scope]
		r.Scope = o.Scope
		r.Observations++
		if o.Agreed {
			r.Agreements++
		}
		out[o.Scope] = r
	}
	return out
}

// Rates reads and aggregates in one call.
func Rates(v varvigcli.Varvig) (map[string]Rate, error) {
	obs, err := Observations(v)
	if err != nil {
		return nil, err
	}
	return Tally(obs), nil
}

// RateFor returns the rate for one scope, zero-valued when nothing is recorded.
func RateFor(v varvigcli.Varvig, scope string) (Rate, error) {
	rates, err := Rates(v)
	if err != nil {
		return Rate{}, err
	}
	r, ok := rates[scope]
	if !ok {
		return Rate{Scope: scope}, nil
	}
	return r, nil
}

// Report renders every scope's rate, worst first — the order that puts the scope
// an operator needs to look at on the first line.
func Report(rates map[string]Rate) []Rate {
	out := make([]Rate, 0, len(rates))
	for _, r := range rates {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value() != out[j].Value() {
			return out[i].Value() < out[j].Value()
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}
