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
}

// Normalize sorts and deduplicates every list so that two cells configured
// identically publish byte-identical capabilities. It is idempotent.
func (c *Capabilities) Normalize() {
	c.Build = sortDedup(c.Build)
	c.Test = sortDedup(c.Test)

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
	// A cell that will author code needs a model to author it with. Catching
	// this at validation is the difference between a clear startup error and a
	// cell that claims tickets it can never attempt — which, because claims are
	// advisory, other cells would still see and consider.
	if c.Has(RoleAttempt) && c.Inference.Tier == TierNone {
		return fmt.Errorf("cell: role %q requires an inference tier other than %q", RoleAttempt, TierNone)
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
