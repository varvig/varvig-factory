package robe

import (
	"sort"

	"github.com/varvig/varvig-factory/cell"
)

// Resolution is the deterministic half of §5b.1: which existing provider
// satisfies a requirement.
//
// It is a **library, not a service and not a robe**. It runs locally on every
// cell over replicated state, which is what makes it compatible with a flat
// factory (§3.0): every cell computes the same answer from the same refs with
// no coordinator, so there is nothing to elect and nothing to be unavailable.
//
// The distinction from Procurement is the one the spec insists on and it is
// about time, not about scope:
//
//	Resolution   which existing provider fits    always available, answers now
//	Procurement  what should we acquire          its own cadence, needs judgment
//
// **Resolution must never block on procurement.** A cell that finds no provider
// proceeds or declines immediately — it does not wait for one to be acquired,
// because waiting would make a deterministic local function depend on a robe
// that may not exist in this factory at all.
type Resolution struct {
	// Interface is the interface hash asked about.
	Interface string
	// Providers are the cells that declare it, sorted by cell id.
	Providers []string
}

// Met reports whether anything provides the requirement.
func (r Resolution) Met() bool { return len(r.Providers) > 0 }

// Resolve answers which cells provide an interface, from a projection of the
// factory's replicated capabilities.
//
// It takes the projection rather than reading refs itself so that resolving a
// hundred requirements costs one read rather than a hundred, and — more
// importantly — so that every requirement in one pass is resolved against one
// consistent view. Two answers derived from two reads taken a moment apart
// would be a factory that disagrees with itself about what it can do.
//
// Matching is on the **interface hash**, never the alias. Two factories may use
// one alias for different interfaces (§2.1), and a resolver that matched names
// would quietly pair a requirement with a provider implementing something else.
func Resolve(p Providers, interfaceHash string) Resolution {
	r := Resolution{Interface: interfaceHash}
	if interfaceHash == "" {
		return r
	}
	for _, c := range p.Cells {
		for _, e := range c.Effects {
			if e.Interface == interfaceHash {
				r.Providers = append(r.Providers, c.CellID)
				break
			}
		}
	}
	sort.Strings(r.Providers)
	return r
}

// Providers is the replicated state resolution reads: every cell's declared
// capabilities.
//
// It is a distinct type from Projection because the two answer different
// questions from the same refs — who wears what, and who can do what — and
// collapsing them would make every robe change look like a capability change to
// whatever cached one.
type Providers struct {
	Cells []cell.Capabilities
}

// Unmet is a requirement nothing in the factory provides.
//
// Recorded as repository state rather than acted on, which is the first step of
// the growth loop (§5b.2): resolution fails, the failure accretes, and
// Procurement observes the accumulation on its own cadence. Nothing here
// escalates, retries or waits — the cell that could not resolve has already
// proceeded or declined.
type Unmet struct {
	Interface string
	// Alias is what the requirement called it, kept for a human reading the
	// report. It is never matched on.
	Alias string
	// Task is the ticket that wanted it.
	Task string
}

// Unresolved returns the requirements in want that no cell provides, sorted.
//
// The zero case is the common one and costs nothing: a factory whose cells
// provide everything asked of them returns an empty slice, and no growth loop
// runs because there is nothing for one to observe.
func Unresolved(p Providers, want []Unmet) []Unmet {
	var out []Unmet
	for _, w := range want {
		if !Resolve(p, w.Interface).Met() {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Interface != out[j].Interface {
			return out[i].Interface < out[j].Interface
		}
		return out[i].Task < out[j].Task
	})
	return out
}
