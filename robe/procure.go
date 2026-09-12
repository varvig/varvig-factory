package robe

import (
	"errors"
	"fmt"
	"sort"

	"github.com/varvig/varvig-factory/authority"
)

// Procurement is the judgment half of §5b.1: what capability the factory should
// acquire, as opposed to which existing provider fits.
//
// # The growth loop, and why every step is repository state
//
//	resolution fails
//	  → unmet requirement recorded as repo state
//	  → procurement observes the accumulation, writes a Profile
//	  → overseer authorizes
//	  → provisioner instantiates from a Blueprint
//
// Four participants, and no two of them need be online at the same moment. That
// is what makes the loop auditable and restartable rather than a distributed
// transaction: each step reads what the last one wrote, and a factory that
// loses power halfway through has a durable record of exactly how far it got.
//
// # This spends money to create things that spend money
//
// Which is why it is build-order last and why the guardrails below are refusals
// rather than advice. A factory that can grow its own spend surface is one
// where a slow drift nobody notices is the failure mode — not a single
// expensive mistake, which a spend cap already catches.
//
// **Procurement proposes; it never provisions.** Writing a Profile is a
// proposal. The same separation §6.7 draws between the principal deciding to
// spend and the one executing, for the same reason: a component that both
// decided what to acquire and acquired it would be authorizing itself.
type Procurement struct {
	// TotalCells caps how many cells this factory may have.
	//
	// **Cap total cells, not only total spend** (§5b.2). A spend ceiling alone
	// does not bound growth, because a new cell needs a lease from the same
	// envelope and a factory can sit just under its ceiling while the number of
	// things drawing on it climbs. The failure mode is drift nobody notices,
	// and a count is the thing that notices.
	//
	// Zero means no growth is permitted at all, which is the right default: a
	// factory that has not been told how large it may become has not authorized
	// becoming larger.
	TotalCells int
}

// ErrNoCap is returned when growth is proposed with no total-cell cap set.
var ErrNoCap = errors.New("robe: no total-cell cap is configured, so no growth is authorized")

// Proposal is a Profile procurement thinks the factory should instantiate.
//
// It is inert. Nothing here instantiates anything, and the type carries no
// method that could: acting on it requires an overseer's authorization and then
// a provisioner, which are two other principals in two later steps.
type Proposal struct {
	// Interface is what the proposed cell would provide.
	Interface string
	// Alias is what the unmet requirements called it, for a human reading this.
	Alias string
	// Wanted is how many unmet requirements accumulated for it — the evidence
	// the proposal rests on, recorded so an overseer can judge whether one
	// unmet requirement is a blip and forty are a gap.
	Wanted int
}

// Propose reads accumulated unmet requirements and returns what to acquire.
//
// It refuses rather than proposing when growth would breach a bound, and the
// refusal names which bound — because "procurement proposed nothing" and
// "procurement was not allowed to propose" are different states for an operator
// wondering why the factory is not growing.
//
// **Recursion-aware**: the cap is checked against the cells that exist now,
// including any a previous round of this loop created. A factory cannot
// bootstrap past its ceiling by creating cells that create cells, because every
// round counts every cell.
func (p Procurement) Propose(now Providers, unmet []Unmet, env authority.Envelope) ([]Proposal, error) {
	if p.TotalCells <= 0 {
		return nil, ErrNoCap
	}
	if counted(now) >= p.TotalCells {
		return nil, fmt.Errorf("%w: the factory has %d of %d permitted cells",
			ErrAtCap, counted(now), p.TotalCells)
	}
	// An envelope that exists and bounds nothing is not an unlimited one. This
	// mirrors the reading PermitCeiling takes: silence from a configured
	// overseer is an omission, not a blank cheque.
	if env.Configured() {
		if err := env.Validate(); err != nil {
			return nil, fmt.Errorf("robe: the envelope bounding growth is malformed: %w", err)
		}
	}

	counts := map[string]*Proposal{}
	for _, u := range unmet {
		if u.Interface == "" {
			continue
		}
		pr, ok := counts[u.Interface]
		if !ok {
			pr = &Proposal{Interface: u.Interface, Alias: u.Alias}
			counts[u.Interface] = pr
		}
		pr.Wanted++
	}

	// Only as many proposals as there is room for. Proposing five cells into
	// two slots would hand an overseer a decision the factory cannot honour,
	// and the honest thing is to ask for what can actually be granted.
	room := p.TotalCells - counted(now)
	out := make([]Proposal, 0, len(counts))
	for _, pr := range counts {
		out = append(out, *pr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wanted != out[j].Wanted {
			// Most-wanted first: the accumulation is the evidence, so the
			// requirement nothing has met forty times outranks the one missed
			// once.
			return out[i].Wanted > out[j].Wanted
		}
		return out[i].Interface < out[j].Interface
	})
	if len(out) > room {
		out = out[:room]
	}
	return out, nil
}

// ErrAtCap is returned when the factory already holds every cell it may.
var ErrAtCap = errors.New("robe: the factory is at its total-cell cap")

// Provision is what a provisioner would do with an authorized proposal, and it
// is deliberately not implemented here.
//
// The signature exists so the separation is legible in the code rather than
// only in prose: procurement's package can describe what happens next and
// cannot do it. Instantiating a cell needs infrastructure credentials and a
// lease, which a Procurement value has neither of and is never given.
func (Procurement) Provision(Proposal) error {
	return errors.New("robe: procurement proposes and never provisions; instantiating a Profile is a provisioner's act, after an overseer authorizes it")
}

// WouldExceed reports whether adding n cells would breach the cap.
//
// Exported so a provisioner can ask before acting rather than after: the
// provisioner is the one that actually creates cells, and a cap checked only at
// proposal time would be a cap that a stale authorization walks past.
func (p Procurement) WouldExceed(now Providers, n int) bool {
	return p.TotalCells <= 0 || counted(now)+n > p.TotalCells
}

// GrowthBound is every bound on growth, for a report an operator can read.
type GrowthBound struct {
	Cells     int
	CellCap   int
	Envelope  string
	Permitted bool
	Reason    string
}

// Bounds describes the factory's room to grow.
func (p Procurement) Bounds(now Providers, env authority.Envelope) GrowthBound {
	b := GrowthBound{Cells: counted(now), CellCap: p.TotalCells, Envelope: env.Overseer}
	switch {
	case p.TotalCells <= 0:
		b.Reason = ErrNoCap.Error()
	case counted(now) >= p.TotalCells:
		b.Reason = fmt.Sprintf("%d of %d permitted cells", counted(now), p.TotalCells)
	default:
		b.Permitted = true
	}
	return b
}

// counted is how many cells a cap counts, and it counts every cell the factory
// can see — including ones a previous round of the growth loop created.
//
// One function because "which cells count" is the question a recursion bug
// answers wrongly, and there should be exactly one place to get it right. Every
// bound below goes through it.
func counted(p Providers) int { return len(p.Cells) }
