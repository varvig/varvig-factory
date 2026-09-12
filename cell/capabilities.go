package cell

import (
	"fmt"
	"sort"
	"strings"
)

// Role is what a cell is willing to do. A cell may be a verifier or builder
// without ever attempting, and that is not a degraded mode — it is what makes
// independent verification a federation feature (FACTORY.md §3.2) and what
// makes the old-hardware story genuinely compelling rather than aspirational
// (§3.1).
type Role string

// The three roles. There is no "promote" role: promotion authority lives in the
// trust store as an allowed_keys right (CELL.md §1), not in a capabilities
// object a cell writes about itself. A cell asserting its own promotion rights
// would be a cell granting them.
const (
	RoleAttempt Role = "attempt"
	RoleVerify  Role = "verify"
	RoleBuild   Role = "build"
)

// Tier is the inference class a cell has available. It is a static fact about
// hardware, not a schedule.
type Tier string

// The inference tiers. TierNone is legal and normal: a Micro cell with no model
// at all still verifies and builds, which is most of the work.
const (
	TierNone  Tier = "none"
	TierSmall Tier = "small"
	TierLarge Tier = "large"
)

// Model identifies one model a cell can run. The identifier is structured, not
// free text, so regeneration routing can match field by field (FEDERATION.md
// §5).
type Model struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Context int    `json:"context,omitempty"`
}

// Inference is the static description of a cell's model capability.
type Inference struct {
	Tier   Tier    `json:"tier"`
	Models []Model `json:"models,omitempty"`
}

// Capabilities is what a cell advertises about itself, published at
// CapabilitiesRef (CELL.md §3).
//
// Static facts only. Liveness never goes in the DAG (FACTORY.md §2.1): GPU
// busy, queue depth and disk pressure are ephemeral and would accumulate as
// permanent garbage in an append-only store. The test for whether a field
// belongs here is whether its value can change without a human changing the
// configuration — if it can, it does not belong.
type Capabilities struct {
	CellID    string    `json:"cell_id"`
	Inference Inference `json:"inference"`
	Build     []string  `json:"build,omitempty"`
	Test      []string  `json:"test,omitempty"`
	Roles     []Role    `json:"roles"`
	// Effects are the effectful capabilities this cell is configured to perform
	// (CELL.md §8.2). Declaring one is not authority to spend — that comes from
	// a lease — it says only that this cell has an executor wired up for it.
	//
	// It is a static fact, so it belongs here: which integrations a cell has is
	// changed by an operator editing configuration, never by the cell itself.
	Effects []EffectCapability `json:"effects,omitempty"`
	// Robes are the responsibilities this cell instance wears (§5b.3).
	//
	// They belong here for the same reason roles do — an operator changes them,
	// the cell never does — and they are **not** authority. A robe makes a cell
	// inclined to do certain work; what it may do comes from its one enrolment
	// scope and nothing else. There is no key per robe and no trust-store edit
	// when robes change, which is what lets them be a derived projection rather
	// than a lifecycle.
	Robes []Robe `json:"robes,omitempty"`
}

// Robe is a responsibility a cell instance wears, temporarily or permanently.
//
// Capability = can do. Robe = responsible for. A cell may wear several, and a
// factory where nobody wears a given robe is a factory that does not do that
// thing — which for Procurement means a fully functional factory that simply
// does not grow (§5b.2).
//
// **Robes carry no authority.** Check each: Monitor only reads; Ambassador
// creates tickets, which any cell may propose; Procurement proposes Profiles,
// which is unprivileged; Provisioner is the only one that acts, and what it
// needs is infrastructure credentials and a lease, not elevated repository
// rights. A robe is a claim-policy input, not a permission.
type Robe string

// The robes (§5b.3).
const (
	// RobeAmbassador is where untrusted input enters. Tickets it creates from
	// external requests must be marked externally-originated so claim policy
	// can treat them differently — this is the prompt-injection surface, and
	// the marking is the only thing that makes it visible downstream.
	RobeAmbassador Robe = "ambassador"
	// RobeProvisioner instantiates cells from a Blueprint. A provisioned cell's
	// ownership roots to the **owner**, never to the provisioner: otherwise a
	// compromised provisioner mints cells loyal to itself and the trust root
	// forks.
	RobeProvisioner Robe = "provisioner"
	// RobeProcurement decides what capability the factory should acquire. It
	// proposes and never provisions — the same separation §8.2 draws between
	// the principal deciding to spend and the one executing.
	RobeProcurement Robe = "procurement"
	// RobeMonitor strictly observes and reports. Never a source of truth, never
	// decides, which is what keeps liveness out of the DAG.
	RobeMonitor Robe = "monitor"
)

// Robes is every robe, sorted, for validation and for a CLI that lists them.
func Robes() []Robe {
	return []Robe{RobeAmbassador, RobeMonitor, RobeProcurement, RobeProvisioner}
}

// Valid reports whether r is a robe this contract defines.
func (r Robe) Valid() bool {
	for _, known := range Robes() {
		if r == known {
			return true
		}
	}
	return false
}

// Wears reports whether this cell wears a robe.
func (c Capabilities) Wears(r Robe) bool {
	for _, worn := range c.Robes {
		if worn == r {
			return true
		}
	}
	return false
}

// CostModel says how an effectful capability's price is known, and whether it
// has one at all.
//
// It lives in the cell contract rather than in the effect package because it is
// declared by configuration, alongside the capability it describes — and
// because where it is declared decides who gets to declare it. A ticket must
// never be able to say a capability is free; that is how a board order becomes
// unmetered.
type CostModel string

// The cost models. The empty value is meaningful and is the third case: a
// capability that declares no cost model **incurs no cost**, needs no lease,
// and is bounded only by the envelope's quantity and rate ceilings (§7.0).
// `effectful` and `costs money` are orthogonal.
const (
	// CostFixed is a price known from the capability contract.
	CostFixed CostModel = "fixed"
	// CostQuoted means the price is not known until an external service is
	// asked, so such capabilities require connectivity by their nature.
	CostQuoted CostModel = "quoted"
)

// Valid reports whether m is one of the three legitimate states.
func (m CostModel) Valid() bool {
	switch m {
	case "", CostFixed, CostQuoted:
		return true
	}
	return false
}

// Priced reports whether a capability with this cost model needs a lease.
func (m CostModel) Priced() bool { return m != "" }

// EffectCapability names one effectful capability a cell can perform.
type EffectCapability struct {
	// ID is the alias, e.g. "pcb-fabrication@1".
	ID string `json:"id"`
	// Interface is the interface hash the alias resolves to for this cell. It is
	// required: two factories may use one alias for different interfaces, and
	// here that ambiguity would be resolved by spending money (§2.1).
	Interface string `json:"interface"`
	// CostModel is how this capability's price is known, empty when it costs
	// nothing (§7.0). An empty value is a claim that the capability is free,
	// and §7.0's guard is what keeps that claim honest: an action that reports
	// a cost against a capability declaring no cost model is refused as
	// malformed rather than run unmetered.
	CostModel CostModel `json:"cost_model,omitempty"`
}

// Normalize sorts and deduplicates every list so that two cells configured
// identically publish byte-identical capabilities. It is idempotent.
func (c *Capabilities) Normalize() {
	c.Build = sortDedup(c.Build)
	c.Test = sortDedup(c.Test)

	robes := make([]string, 0, len(c.Robes))
	for _, r := range c.Robes {
		robes = append(robes, string(r))
	}
	c.Robes = nil
	for _, r := range sortDedup(robes) {
		c.Robes = append(c.Robes, Robe(r))
	}

	roles := make([]string, 0, len(c.Roles))
	for _, r := range c.Roles {
		roles = append(roles, string(r))
	}
	roles = sortDedup(roles)
	c.Roles = make([]Role, 0, len(roles))
	for _, r := range roles {
		c.Roles = append(c.Roles, Role(r))
	}

	sort.Slice(c.Inference.Models, func(i, j int) bool {
		a, b := c.Inference.Models[i], c.Inference.Models[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Version < b.Version
	})
}

// Validate checks the §3 rules. It is called before publishing and after
// reading, because a capabilities object that arrived from a peer is exactly as
// untrusted as one that arrived from a config file.
func (c Capabilities) Validate() error {
	if err := CheckID(c.CellID); err != nil {
		return err
	}
	switch c.Inference.Tier {
	case TierNone:
		if len(c.Inference.Models) > 0 {
			return fmt.Errorf("cell: tier %q must declare no models, got %d", TierNone, len(c.Inference.Models))
		}
	case TierSmall, TierLarge:
		if len(c.Inference.Models) == 0 {
			return fmt.Errorf("cell: tier %q must declare at least one model", c.Inference.Tier)
		}
	default:
		return fmt.Errorf("cell: unknown inference tier %q", c.Inference.Tier)
	}
	for _, m := range c.Inference.Models {
		if m.ID == "" {
			return fmt.Errorf("cell: model with empty id")
		}
		if m.Context < 0 {
			return fmt.Errorf("cell: model %q has negative context", m.ID)
		}
	}
	if len(c.Roles) == 0 {
		return fmt.Errorf("cell: at least one role is required")
	}
	for _, r := range c.Roles {
		switch r {
		case RoleAttempt, RoleVerify, RoleBuild:
		default:
			return fmt.Errorf("cell: unknown role %q", r)
		}
	}
	for _, r := range c.Robes {
		if !r.Valid() {
			return fmt.Errorf("cell: unknown robe %q; robes are a fixed set because an unknown one is a claim-policy input nothing reads", r)
		}
	}
	// A cell that will author code needs a model to author it with. Catching
	// this at validation is the difference between a clear startup error and a
	// cell that claims tickets it can never attempt — which, because claims are
	// advisory, other cells would still see and consider.
	if c.Has(RoleAttempt) && c.Inference.Tier == TierNone {
		// A cell advertising that it attempts while declaring no model is
		// advertising something it cannot deliver, and a capabilities object is
		// read by other cells to decide what to expect of this one.
		//
		// Note what this does *not* say. It is a check on the declaration's
		// internal coherence, not on whether a model is reachable right now —
		// inference arrives through an executor that may be a hosted API, so
		// reachability is a runtime fact the loop discovers and this object,
		// which holds static facts only (§2.3), must not try to hold. A cell
		// whose executor is unreachable keeps this declaration and declines to
		// attempt; it does not become misconfigured (§9.17).
		return fmt.Errorf("cell: role %q needs at least one declared model; a cell that advertises attempting with inference tier %q is advertising what it cannot do", RoleAttempt, TierNone)
	}
	seen := map[string]bool{}
	for _, e := range c.Effects {
		if e.ID == "" {
			return fmt.Errorf("cell: effectful capability with empty id")
		}
		if e.Interface == "" {
			return fmt.Errorf("cell: effectful capability %q declares no interface hash; the hash is the identity, and an alias alone is ambiguous between factories", e.ID)
		}
		if !IsMultihash(e.Interface) {
			return fmt.Errorf("cell: effectful capability %q names interface %q, which is not an object hash", e.ID, e.Interface)
		}
		if !e.CostModel.Valid() {
			return fmt.Errorf("cell: effectful capability %q declares an unknown cost model %q", e.ID, e.CostModel)
		}
		if seen[e.ID] {
			// Two entries for one alias would make "which interface does this
			// cell mean by that name" ambiguous at the moment of spending.
			return fmt.Errorf("cell: effectful capability %q is declared twice", e.ID)
		}
		seen[e.ID] = true
	}
	return nil
}

// Has reports whether the cell holds role r.
func (c Capabilities) Has(r Role) bool {
	for _, got := range c.Roles {
		if got == r {
			return true
		}
	}
	return false
}

// Supports reports whether the cell declares every build and test capability in
// the given requirement lists. Matching is by equality on free-form tokens: a
// substring or a version-range match would be a small convenience now and an
// unfixable ambiguity across a federation, since the tokens are chosen by
// whoever writes the ticket.
func (c Capabilities) Supports(build, test []string) bool {
	return covers(c.Build, build) && covers(c.Test, test)
}

// Missing returns the requirements this cell does not declare, so a skip can
// say *why* it skipped instead of just declining.
func (c Capabilities) Missing(build, test []string) []string {
	var out []string
	for _, want := range build {
		if !contains(c.Build, want) {
			out = append(out, "build:"+want)
		}
	}
	for _, want := range test {
		if !contains(c.Test, want) {
			out = append(out, "test:"+want)
		}
	}
	sort.Strings(out)
	return out
}

// Hash is the capabilities object's canonical hash, so a peer can tell whether
// a cell's advertisement changed without diffing it field by field.
func (c Capabilities) Hash() (string, error) {
	c.Normalize()
	return CanonicalHash(c)
}

// String renders capabilities for a human, one line.
func (c Capabilities) String() string {
	models := make([]string, 0, len(c.Inference.Models))
	for _, m := range c.Inference.Models {
		if m.Version != "" {
			models = append(models, m.ID+"@"+m.Version)
		} else {
			models = append(models, m.ID)
		}
	}
	roles := make([]string, 0, len(c.Roles))
	for _, r := range c.Roles {
		roles = append(roles, string(r))
	}
	return fmt.Sprintf("%s tier=%s models=[%s] build=[%s] test=[%s] roles=[%s]",
		c.CellID, c.Inference.Tier, strings.Join(models, " "),
		strings.Join(c.Build, " "), strings.Join(c.Test, " "), strings.Join(roles, " "))
}

func sortDedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	w := 0
	for i, s := range out {
		if s == "" {
			continue
		}
		if i > 0 && s == out[i-1] {
			continue
		}
		out[w] = s
		w++
	}
	return out[:w]
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func covers(have, want []string) bool {
	for _, w := range want {
		if !contains(have, w) {
			return false
		}
	}
	return true
}

// Effect returns the configured effectful capability with this alias.
//
// The lookup exists so a caller takes the capability's terms — its interface
// and its cost model — from **this cell's configuration** rather than from the
// ticket asking for the work. That direction is the point: a ticket names what
// it wants done, and an operator decides what performing it costs. Reading the
// cost model off the request would let a ticket declare a priced capability
// free and slip past the lease entirely.
func (c Capabilities) Effect(id string) (EffectCapability, bool) {
	for _, e := range c.Effects {
		if e.ID == id {
			return e, true
		}
	}
	return EffectCapability{}, false
}

// PolicyCell reports whether this cell has no model configured at all.
//
// This is the distinction §3 says matters more than the capacity class: a
// policy cell verifies, builds, executes effectful capabilities and syncs, and
// does not attempt. It is **not a degraded cell.** Its evidence is
// deterministic, reproducible, and produced by a different cell than the one
// that authored the attempt — which is exactly the §6.3 condition that licenses
// autonomous promotion. The cheapest hardware in the factory produces what
// makes the expensive hardware trustworthy.
//
// The one limit worth stating: a policy cell cannot originate work. It needs
// tickets from somewhere and attempts to verify. Autonomous in its role, not
// self-directing.
func (c Capabilities) PolicyCell() bool { return c.Inference.Tier == TierNone }

// InferenceCell reports whether a model is configured for this cell.
//
// Orthogonal to the capacity class, and that orthogonality is the point: a
// Micro cell reaching a hosted API through an executor is an inference cell,
// and a Mini cell with no model configured is a policy cell. Inference is a
// *capability*, not a hardware fact (§3), so nothing here may be inferred from
// how much build and test capacity a cell has.
func (c Capabilities) InferenceCell() bool { return !c.PolicyCell() }
