// Package profile turns a configuration file into a wired cell.
//
// It is where FACTORY.md §1.2 is either honoured or quietly broken: *tiers are
// configuration profiles, not code paths*. Micro and Mini differ only in which
// model runtime and budget the config names. So this package has exactly one
// function that builds a cell, it does not read the profile name while building
// one, and a test asserts that. If a tier ever needs a branch in Wire, the
// abstraction has failed and the failure should be visible in a diff rather than
// discovered a year later.
//
// Configuration is JSON rather than the YAML the spec's examples are written in,
// because this module has no third-party dependencies and Go's standard library
// has no YAML parser. Every field below maps one-to-one onto the spec's example
// keys, so a YAML snippet from the design notes translates mechanically.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/artifact"
	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/effect"
	"github.com/varvig/varvig-factory/executor"
	"github.com/varvig/varvig-factory/gate"
	"github.com/varvig/varvig-factory/loop"
	"github.com/varvig/varvig-factory/promote"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Duration is a time.Duration that reads from JSON as a string ("30m", "2h").
// A bare number would be ambiguous — nanoseconds are what Go would pick and
// minutes are what a person would mean — so only the string form is accepted.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("profile: durations must be strings like \"30m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("profile: bad duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D is the duration, or fallback when unset.
func (d Duration) D(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}

// InferenceConfig names a model runtime. Kind selects an adapter, never a tier:
// "http" serves ollama, vLLM, llama.cpp's server and a hosted API alike.
type InferenceConfig struct {
	// Kind is "none", "http", or "command".
	Kind string `json:"kind"`
	// Tier is what the cell advertises (CELL.md §3). It must agree with Kind:
	// "none" with a runtime, or a runtime with tier none, is a configuration
	// whose advertisement and behaviour disagree.
	Tier cell.Tier `json:"tier"`

	Endpoint     string `json:"endpoint,omitempty"`
	VersionURL   string `json:"version_url,omitempty"`
	Model        string `json:"model,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
	Context      int    `json:"context,omitempty"`
	System       string `json:"system,omitempty"`

	// AuthHeader names the header credentials go in; AuthValueEnv names the
	// environment variable holding the value. The value itself is never in the
	// config file, so a cell's configuration can be committed to the repository
	// it works on.
	AuthHeader   string `json:"auth_header,omitempty"`
	AuthValueEnv string `json:"auth_value_env,omitempty"`

	// Path, Args and VersionArgs configure the "command" kind.
	Path        string   `json:"path,omitempty"`
	Args        []string `json:"args,omitempty"`
	VersionArgs []string `json:"version_args,omitempty"`

	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	Seed        int64   `json:"seed,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

// SandboxConfig names a build executor.
type SandboxConfig struct {
	// Kind is "subprocess", "container", or "nix".
	Kind string `json:"kind"`
	// Runner is the container command, e.g. ["docker"] or ["podman"].
	Runner []string `json:"runner,omitempty"`
	// Image is the container image, which must be digest-pinned.
	Image string `json:"image,omitempty"`
	// Installable is the nix flake reference, e.g. ".#ci".
	Installable string `json:"installable,omitempty"`
	// Probes measure toolchain versions inside the executor. "go" is expanded to
	// the built-in Go probe.
	Probes []ProbeConfig `json:"probes,omitempty"`
	// Flags are outcome-affecting flags recorded in the environment.
	Flags map[string]string `json:"flags,omitempty"`
	// Platform overrides the reported platform; leave empty for the host's.
	Platform string `json:"platform,omitempty"`
	// Timeout bounds a job that names none.
	Timeout Duration `json:"timeout,omitempty"`
}

// ProbeConfig is one toolchain version measurement.
type ProbeConfig struct {
	Key     string   `json:"key"`
	Command []string `json:"command"`
}

// ArtifactConfig names an artifact store.
type ArtifactConfig struct {
	// Kind is "local" or "remote".
	Kind string `json:"kind"`
	// Root is the cell-local content-addressed directory.
	Root string `json:"root,omitempty"`
	// PushCommand replicates on promotion. Tokens {{path}}, {{hash}} and {{hex}}
	// are substituted.
	PushCommand []string `json:"push_command,omitempty"`
	// LocatorTemplate renders the locator recorded after a push.
	LocatorTemplate string `json:"locator_template,omitempty"`
	// Label names the adapter in logs, e.g. "oci".
	Label string `json:"label,omitempty"`
}

// CheckConfig is one named check.
type CheckConfig struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Timeout Duration `json:"timeout,omitempty"`
	// Kind is "build" or "verify". Empty means verify.
	Kind string `json:"kind,omitempty"`
}

// PromotionConfig configures the promotion path.
type PromotionConfig struct {
	// StatePath is where the live mode and the enabled paths are kept. The kill
	// switch works by rewriting this file, so it must be somewhere the CLI and
	// the running loop both see (§6.5).
	StatePath string `json:"state_path,omitempty"`
	// GateModule is the wasm promotion-policy module to bind (§6.2).
	GateModule string `json:"gate_module,omitempty"`
	// Threshold and MinObservations configure the agreement gate (§6.4).
	Threshold       float64 `json:"agreement_threshold,omitempty"`
	MinObservations int     `json:"agreement_min_observations,omitempty"`
	// Fingerprint is this cell's key fingerprint, matched against the trust
	// store so a revoked allowed_keys line stops promotion immediately (§6.5).
	Fingerprint string `json:"fingerprint,omitempty"`
	// MaxTrustAge optionally bounds how old a successful sync may be and still
	// count as current for promotion (§4.3b). Empty means reachability alone
	// decides, which is what the spec asks for; set it to be stricter.
	MaxTrustAge Duration `json:"max_trust_age,omitempty"`
	// Baselines maps a path scope to the declared environment baseline for it
	// (§6.3 condition 2).
	Baselines map[string]cell.Environment `json:"baselines,omitempty"`
}

// Config is a cell's whole configuration.
type Config struct {
	// Profile is the template this config started from: "micro", "mini" or
	// "medium". It is **informational**. Wire never reads it, and a test
	// enforces that, because a tier that changes behaviour anywhere but in these
	// field values is a tier-specific code path (§1.2).
	Profile string `json:"profile,omitempty"`

	CellID string `json:"cell_id"`
	// Repo is the project repository this cell works in: the codebase, its
	// tickets, attempts, evidence and reservations.
	Repo string `json:"repo,omitempty"`
	// FactoryRepo is the factory-coordination repository: membership, cell
	// capabilities, interface schemas, envelopes and leases.
	//
	// Empty means this cell's project repository serves both roles — the
	// single-project factory, where there is nothing to fragment. Set it as soon
	// as there is a second project, because that is the point at which every
	// project growing its own copy of what the cell may spend stops being
	// harmless.
	FactoryRepo string `json:"factory_repo,omitempty"`
	// VarvigBin overrides the `varvig` binary.
	VarvigBin string `json:"varvig_bin,omitempty"`
	// Rendezvous is the set of peers the project replica syncs with.
	//
	// A set, not a priority list: every member is equally a rendezvous, all of
	// them are contacted each pass, and the order is shuffled so none is
	// systematically first (FACTORY.md §3.0). Empty is a single-cell deployment.
	Rendezvous []string `json:"rendezvous,omitempty"`
	// FactoryRendezvous is the set of peers the coordination replica syncs with.
	//
	// There is deliberately no fallback to Rendezvous when this is empty. A cell
	// with two real repositories and only the project set configured would fetch
	// its authority from project peers, which are the wrong peers for the
	// question "what may I spend" — and a default that is right for one
	// deployment shape and silently wrong for another is worse than no default.
	// A collapsed deployment names the same addresses in both and says so.
	FactoryRendezvous []string `json:"factory_rendezvous,omitempty"`
	Branch            string   `json:"branch,omitempty"`

	Roles []cell.Role `json:"roles"`
	Build []string    `json:"build,omitempty"`
	Test  []string    `json:"test,omitempty"`

	// Robes are the responsibilities this cell wears (FACTORY.md §5b.3).
	//
	// Configuration, because a cell wears what it says it wears and no cell can
	// give another one a robe — pushing a robe onto a peer would be
	// authoritative assignment, which needs consensus this design does not
	// build. Every other cell reads this from the replicated capabilities
	// object and derives the projection locally.
	//
	// Setting one grants nothing. A robe is a claim-policy input; what a cell
	// may do comes from the single scope it was granted at enrolment, which is
	// the same bundle whether it wears everything or nothing.
	Robes []cell.Robe `json:"robes,omitempty"`

	Inference InferenceConfig `json:"inference"`
	Sandbox   SandboxConfig   `json:"sandbox"`
	Artifacts ArtifactConfig  `json:"artifacts"`
	Budget    budget.Budget   `json:"budget"`
	Checks    []CheckConfig   `json:"checks,omitempty"`
	Promotion PromotionConfig `json:"promotion,omitempty"`

	ClaimTTL Duration `json:"claim_ttl,omitempty"`
	TaskTTL  Duration `json:"task_ttl,omitempty"`
	Interval Duration `json:"interval,omitempty"`

	// StateDir holds the cell's operational state: the budget ledger and the
	// promotion switch.
	StateDir string `json:"state_dir,omitempty"`
	// WorkDir holds task checkouts.
	WorkDir string `json:"work_dir,omitempty"`
	// ArtifactGlobs name build outputs to record as artifact-refs.
	ArtifactGlobs []string `json:"artifact_globs,omitempty"`
	// YieldToFreshClaims skips a task another cell has freshly claimed. On by
	// default in the templates because duplicating work costs budget; turn it off
	// to run deliberately redundant attempts (§5.1 — duplicates are the point).
	YieldToFreshClaims bool `json:"yield_to_fresh_claims,omitempty"`
	// MaxAttemptsPerCell caps repeat attempts by this cell at one task.
	MaxAttemptsPerCell int `json:"max_attempts_per_cell,omitempty"`
	// DeclineExternallyOriginated skips tickets an Ambassador created from an
	// outside request (§5b.3).
	//
	// Off by default, which is the only honest default: whether external work
	// needs a person to look at it first is an operator's judgment about their
	// factory, and a Factory that decided for them would either block work
	// nobody wanted blocked or wave through work somebody did.
	DeclineExternallyOriginated bool `json:"decline_externally_originated,omitempty"`

	// Effects configures effectful capabilities (§6.7). Absent is the normal
	// case: a cell that builds and tests code has no business holding a
	// purchasing integration, and requiring every deployment to configure one
	// would be absurd.
	Effects EffectConfig `json:"effects,omitempty"`
}

// EffectConfig wires a cell for effectful capabilities.
//
// Nothing here grants authority to spend — that is a lease, which an overseer
// writes and this cell cannot. This says only which integrations exist and who
// authorizes their use.
type EffectConfig struct {
	// AuthorizedBy is the higher principal that authorizes this cell's effectful
	// actions. It must not be this cell (§9.15), and the check lives in
	// effect.Check rather than here, so a cell cannot authorize its own spending
	// by editing its own configuration.
	AuthorizedBy string `json:"authorized_by,omitempty"`
	// TTL is how long a reservation holds lease headroom before the hold lapses
	// (§9.14). Empty means no expiry, which is only right for a capability that
	// always answers synchronously — for anything asynchronous a lost response
	// then consumes the headroom for good.
	TTL Duration `json:"reservation_ttl,omitempty"`
	// Capabilities are the effectful capabilities this cell can perform.
	Capabilities []EffectCapabilityConfig `json:"capabilities,omitempty"`
}

// EffectCapabilityConfig is one wired effectful capability.
type EffectCapabilityConfig struct {
	// ID is the alias and Interface the hash it resolves to. The hash is
	// required: two factories may use one alias for different interfaces, and
	// here the ambiguity would be resolved by spending money (§2.1).
	ID        string `json:"id"`
	Interface string `json:"interface"`
	// CostModel is "fixed", "quoted", or absent for a capability that costs
	// nothing (§7.0). Absent is a real and common answer — a light switch, a
	// robot arm, a print with filament already paid for — and it means no lease
	// is needed, so the operator declaring it here is declaring that no budget
	// gates this action. An action that then reports a cost is refused as
	// malformed rather than run unmetered.
	CostModel cell.CostModel `json:"cost_model,omitempty"`
	// Executor selects how this capability is performed:
	//
	//	"connector"  a connector peer takes the offer and reports (the default
	//	             for anything real: it holds the vendor's credentials, runs
	//	             anywhere, and needs no rebuild to add)
	//	"refusing"   declines every action in-process, which is how an operator
	//	             proves the wiring works without anything being ordered
	//
	// There is no vendor name here and there never will be. Adding a vendor by
	// rebuilding the cell binary makes no sense, and it would put that vendor's
	// credentials in the cell's process — so vendors arrive as connector peers,
	// the shape core already uses for tracker bridges.
	Executor string `json:"executor,omitempty"`
}

// Micro is the CPU-local profile: **roles verify and build, not attempt**
// (FACTORY.md §3.1).
//
// A CPU-local model authoring code loses nearly every selection while still
// consuming review attention — net negative. Micro is strong as a verification
// and build cell: deterministic work, cheap, no model-quality problem. So the
// template ships with no inference at all and attempting is opt-in, which is the
// spec's default rather than a cautious reading of it.
func Micro(cellID string) Config {
	return Config{
		Profile:   "micro",
		CellID:    cellID,
		Roles:     []cell.Role{cell.RoleBuild, cell.RoleVerify},
		Build:     []string{"go"},
		Test:      []string{"unit"},
		Inference: InferenceConfig{Kind: "none", Tier: cell.TierNone},
		Sandbox: SandboxConfig{
			Kind:   "subprocess",
			Probes: []ProbeConfig{{Key: "go"}},
			Flags:  map[string]string{"CGO_ENABLED": "0"},
		},
		Artifacts: ArtifactConfig{Kind: "local", Root: ".varvig-factory/artifacts"},
		Budget: budget.Budget{
			// No inference budget: a verify/build cell should not hold one it
			// could only spend by being misconfigured (CELL.md §8).
			VerifyConcurrent: 2,
			StorageGB:        20,
			AttemptsDefault:  1,
		},
		Checks: []CheckConfig{
			{Name: "build", Command: []string{"go", "build", "./..."}, Kind: "build"},
			{Name: "unit", Command: []string{"go", "test", "./..."}, Kind: "verify"},
		},
		ClaimTTL:           Duration(30 * time.Minute),
		TaskTTL:            Duration(2 * time.Hour),
		Interval:           Duration(2 * time.Minute),
		StateDir:           ".varvig-factory",
		WorkDir:            ".varvig-factory/work",
		YieldToFreshClaims: true,
	}
}

// Mini is the accelerated-capacity class, and ships paired with a local model:
// attempting enabled, same binary, config only (§10.4). Every difference from
// Micro below is a field value.
//
// **The pairing is a convenience, not a property of the class.** Cell class
// describes build and test capacity and says nothing about inference (§3):
// inference reaches a cell through an executor that may perfectly well be a
// hosted API, so it is a capability rather than a hardware fact. What this
// template does is bundle the two settings an operator most often wants
// together — more test capacity, and a model to author with.
//
// Both cross-combinations are legitimate and supported:
//
//	Micro(id).WithInference(hosted)   a commodity host that authors through an API
//	Mini(id).WithoutInference()       a high-core host that only verifies and builds
//
// If a reader ever has to be told which class implies a model, the two axes
// have been welded together again.
func Mini(cellID string) Config {
	c := Micro(cellID)
	c.Profile = "mini"
	c.Roles = []cell.Role{cell.RoleAttempt, cell.RoleBuild, cell.RoleVerify}
	c.Test = []string{"unit", "integration"}
	c.Inference = InferenceConfig{
		Kind:         "http",
		Tier:         cell.TierLarge,
		Endpoint:     "http://127.0.0.1:11434/v1/chat/completions",
		VersionURL:   "http://127.0.0.1:11434/api/version",
		Model:        "qwen2.5-coder",
		ModelVersion: "32b-q4",
		Context:      32768,
		Temperature:  0.2,
	}
	c.Budget = budget.Budget{
		InferenceDaily:   5000, // 50.00
		PerCallCost:      2,    // 0.02
		VerifyConcurrent: 4,
		StorageGB:        200,
		AttemptsDefault:  3,
	}
	return c
}

// Medium is a Mini cell that syncs with a rendezvous set — the federation-wide
// class (§3). It differs from Mini in one field, which is the honest shape of
// the difference: "N cells that sync with each other" is a deployment, not a
// class of binary.
//
// The variadic argument is a set and not a first-plus-fallbacks: every address
// given is contacted each pass, in a shuffled order, and none of them is a
// coordinator (§3.0).
func Medium(cellID string, rendezvous ...string) Config {
	c := Mini(cellID)
	c.Profile = "medium"
	c.Rendezvous = rendezvous
	// A Medium cell built this way is the collapsed, single-project shape, so
	// the same peers serve both repositories. Stated rather than defaulted:
	// there is no fallback, and a factory with a second project sets
	// factory_rendezvous to its own peers.
	c.FactoryRendezvous = rendezvous
	c.Branch = "refs/heads/main"
	return c
}

// Template returns a named starting profile.
//
// rendezvous may name several peers, comma-separated, because a set of one is a
// factory that stops when that one goes away — which is the thing a set exists
// to prevent. A Medium cell with no peers named is a Mini cell that has been
// given a class name, and is accepted as such rather than refused.
func Template(name, cellID, rendezvous string) (Config, error) {
	switch strings.ToLower(name) {
	case "micro":
		return Micro(cellID), nil
	case "mini":
		return Mini(cellID), nil
	case "medium":
		return Medium(cellID, splitPeers(rendezvous)...), nil
	}
	return Config{}, fmt.Errorf("profile: unknown profile %q (want micro, mini or medium)", name)
}

// splitPeers parses a comma-separated rendezvous set, dropping blanks so a
// trailing comma or a stray space is not turned into an address nothing can
// dial.
func splitPeers(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if addr := strings.TrimSpace(part); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

// Load reads a config file.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// Unknown fields are an error rather than a silent no-op: a typo'd
	// "inference_daily" that quietly means "no cap" is the single worst
	// misconfiguration this file can carry.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("profile: reading %s: %w", path, err)
	}
	return c, nil
}

// Save writes a config file, indented for a human to edit.
func Save(path string, c Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Capabilities is what this config advertises (CELL.md §3).
func (c Config) Capabilities() cell.Capabilities {
	caps := cell.Capabilities{
		CellID: c.CellID,
		Build:  c.Build,
		Test:   c.Test,
		Roles:  c.Roles,
		Robes:  c.Robes,
		Inference: cell.Inference{
			Tier: c.Inference.Tier,
		},
	}
	if c.Inference.Tier != cell.TierNone && c.Inference.Model != "" {
		caps.Inference.Models = []cell.Model{{
			ID:      c.Inference.Model,
			Version: c.Inference.ModelVersion,
			Context: c.Inference.Context,
		}}
	}
	for _, e := range c.Effects.Capabilities {
		caps.Effects = append(caps.Effects, cell.EffectCapability{ID: e.ID, Interface: e.Interface, CostModel: e.CostModel})
	}
	caps.Normalize()
	return caps
}

// buildExecutors turns the configured capabilities into in-process executors.
//
// The set is deliberately small and is not where vendors get added: an
// integration holding credentials to a service that charges money belongs in a
// separate process, not in the cell binary and not behind a name in a config
// file. Vendors arrive as connector peers answering reservations from the
// repository, which is core's own pattern for tracker bridges and needs no
// rebuild — see EffectCapabilityConfig.Executor.
func (c Config) buildExecutors() (effect.Executors, error) {
	var out effect.Executors
	for _, e := range c.Effects.Capabilities {
		capability := effect.Capability{ID: e.ID, Interface: e.Interface, Effectful: true}
		if err := capability.Validate(); err != nil {
			return nil, err
		}
		switch e.Executor {
		case "connector":
			// Performed elsewhere: no in-process executor, and the loop offers
			// the reservation rather than acting on it.
			continue
		case "", "refusing":
			// A capability declared with no executor named must not silently
			// become one that acts.
			out = append(out, effect.Refusing{Capability: capability})
		default:
			return nil, fmt.Errorf("profile: capability %q names executor %q; the choices are %q and %q, and vendor integrations arrive as connector peers rather than by name here",
				e.ID, e.Executor, "connector", "refusing")
		}
	}
	return out, nil
}

// Validate checks the config for the mistakes that would otherwise surface as
// confusing runtime behaviour.
func (c Config) Validate() error {
	if err := cell.CheckID(c.CellID); err != nil {
		return err
	}
	if err := c.Capabilities().Validate(); err != nil {
		return err
	}
	if err := c.Budget.Validate(); err != nil {
		return err
	}
	// The advertisement and the runtime must agree. A cell advertising tier
	// "large" while configured with no runtime would claim tickets it can never
	// attempt — and because claims are advisory, other cells would see and
	// respect those claims.
	switch strings.ToLower(c.Inference.Kind) {
	case "", "none":
		if c.Inference.Tier != cell.TierNone {
			return fmt.Errorf("profile: executor.kind is %q but executor.tier is %q; a cell with no runtime must advertise tier %q",
				c.Inference.Kind, c.Inference.Tier, cell.TierNone)
		}
	case "http", "command":
		if c.Inference.Tier == cell.TierNone {
			return fmt.Errorf("profile: executor.kind is %q but executor.tier is %q; a cell with a runtime must advertise the tier it is",
				c.Inference.Kind, cell.TierNone)
		}
		if c.Inference.Model == "" {
			return fmt.Errorf("profile: executor.kind is %q but no model is named", c.Inference.Kind)
		}
	default:
		return fmt.Errorf("profile: unknown executor.kind %q (want none, http or command)", c.Inference.Kind)
	}
	switch strings.ToLower(c.Sandbox.Kind) {
	case "", "subprocess":
	case "container":
		if c.Sandbox.Image == "" {
			return fmt.Errorf("profile: executor.kind is container but no image is named")
		}
		if !strings.Contains(c.Sandbox.Image, "@") {
			// A tag is mutable. A tag-pinned sandbox publishes a stable
			// environment hash while the ground under it moves, which makes every
			// cross-cell comparison against it quietly wrong.
			return fmt.Errorf("profile: executor.image %q is not digest-pinned; use image@sha256:… so the environment hash means something", c.Sandbox.Image)
		}
	case "nix":
		if c.Sandbox.Installable == "" {
			return fmt.Errorf("profile: executor.kind is nix but no installable is named")
		}
	default:
		return fmt.Errorf("profile: unknown executor.kind %q (want subprocess, container or nix)", c.Sandbox.Kind)
	}
	switch strings.ToLower(c.Artifacts.Kind) {
	case "", "local":
	case "remote":
		if len(c.Artifacts.PushCommand) == 0 {
			return fmt.Errorf("profile: artifacts.kind is remote but no push_command is configured")
		}
	default:
		return fmt.Errorf("profile: unknown artifacts.kind %q (want local or remote)", c.Artifacts.Kind)
	}
	for _, chk := range c.Checks {
		if chk.Name == "" || len(chk.Command) == 0 {
			return fmt.Errorf("profile: check %q has no name or no command", chk.Name)
		}
		switch strings.ToLower(chk.Kind) {
		case "", "verify", "build":
		default:
			return fmt.Errorf("profile: check %q has unknown kind %q (want build or verify)", chk.Name, chk.Kind)
		}
	}
	return nil
}

// Built is a wired cell plus the pieces a CLI needs to address separately.
type Built struct {
	Cell *loop.Cell
	// Varvig is the project replica's client, kept for callers that only need a
	// repository to read from and do not care which role it is playing.
	Varvig varvigcli.Varvig
	// Factory and Project are the two replicas, so a caller asking about spend
	// asks the repository that authoritatively answers it.
	Factory  varvigcli.FactoryRepo
	Project  varvigcli.ProjectRepo
	Switch   *promote.Switch
	Ledger   *budget.Ledger
	Gate     gate.Module
	Interval time.Duration
}

// Wire builds a cell from a config.
//
// Note what is not here: any reference to c.Profile. Micro, Mini and Medium
// reach this function as different field values and leave it as the same type
// running the same code (§1.2). TestWireIgnoresTheProfileName holds that
// property, and the §9.1 tier-equivalence test holds the behavioural half.
//
// (It is not called Build because a cell's `build` capabilities are a
// configuration field of that name, and one identifier meaning two things in the
// same type is how a reader ends up looking at the wrong one.)
func (c Config) Wire(v varvigcli.Varvig) (Built, error) {
	if err := c.Validate(); err != nil {
		return Built{}, err
	}
	// The two replicas.
	//
	// A caller that supplied its own client — every test, and the in-process
	// simulator — gets the collapsed configuration, because one client is one
	// repository however many roles it plays. Combining that with a configured
	// factory_repo is refused rather than resolved: building a disk-backed
	// factory client alongside a supplied in-memory one would send authority
	// reads somewhere the caller did not ask for and cannot see.
	var factory varvigcli.FactoryRepo
	var project varvigcli.ProjectRepo
	switch {
	case v != nil && c.FactoryRepo != "":
		return Built{}, fmt.Errorf("profile: a varvig client was supplied and factory_repo is set to %q; these ask for different factory replicas and only one can be right", c.FactoryRepo)
	case v != nil:
		factory, project = varvigcli.Collapsed(v)
	case c.FactoryRepo != "":
		project = varvigcli.ProjectRepo{Varvig: varvigcli.Exec{Bin: c.VarvigBin, Dir: c.repo()}}
		factory = varvigcli.FactoryRepo{Varvig: varvigcli.Exec{Bin: c.VarvigBin, Dir: c.FactoryRepo}}
	default:
		factory, project = varvigcli.Collapsed(varvigcli.Exec{Bin: c.VarvigBin, Dir: c.repo()})
	}
	v = project.Varvig

	ledger, err := budget.NewLedger(c.Budget, c.statePath("ledger.json"), time.Now())
	if err != nil {
		return Built{}, err
	}
	sw, err := promote.NewSwitch(c.switchPath())
	if err != nil {
		return Built{}, err
	}

	author, err := c.authoring()
	if err != nil {
		return Built{}, err
	}
	check, err := c.checking()
	if err != nil {
		return Built{}, err
	}
	executors, err := c.buildExecutors()
	if err != nil {
		return Built{}, err
	}
	store, err := c.store()
	if err != nil {
		return Built{}, err
	}

	g := gate.Module{Project: project}
	cl := &loop.Cell{
		Capabilities:       c.Capabilities(),
		Factory:            factory,
		Project:            project,
		Authoring:          author,
		Checking:           check,
		Artifacts:          store,
		Ledger:             ledger,
		Rendezvous:         loop.Peers(c.Rendezvous),
		FactoryRendezvous:  loop.Peers(c.FactoryRendezvous),
		Branch:             c.Branch,
		Checks:             c.checks(),
		ClaimTTL:           c.ClaimTTL.D(30 * time.Minute),
		TaskTTL:            c.TaskTTL.D(2 * time.Hour),
		WorkDir:            c.path(c.WorkDir),
		ArtifactGlobs:      c.ArtifactGlobs,
		Baselines:          c.Promotion.Baselines,
		YieldToFreshClaims: c.YieldToFreshClaims,
		MaxAttemptsPerCell: c.MaxAttemptsPerCell,

		DeclineExternallyOriginated: c.DeclineExternallyOriginated,
		MaxTrustAge:                 authority.MaxAge(c.Promotion.MaxTrustAge.D(0)),
		Executors:                   executors,
		Connectors:                  c.connectorCapabilities(),
		EffectAuthorizedBy:          c.Effects.AuthorizedBy,
		EffectTTL:                   int64(c.Effects.TTL.D(0) / time.Second),
	}
	cl.Promoter = &promote.Promoter{
		Project:     project,
		Switch:      sw,
		Gate:        g,
		Agreement:   agreement.NewGate(c.Promotion.Threshold, c.Promotion.MinObservations),
		Reverify:    cl,
		CellID:      c.CellID,
		Fingerprint: c.Promotion.Fingerprint,
		MaxTrustAge: authority.MaxAge(c.Promotion.MaxTrustAge.D(0)),
	}
	return Built{
		Cell:     cl,
		Varvig:   v,
		Factory:  factory,
		Project:  project,
		Switch:   sw,
		Ledger:   ledger,
		Gate:     g,
		Interval: c.Interval.D(2 * time.Minute),
	}, nil
}

func (c Config) repo() string {
	if c.Repo == "" {
		return "."
	}
	return c.Repo
}

// path resolves a config path relative to the repository, so a config file can
// use relative paths and a cell can be moved without editing every one.
func (c Config) path(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.repo(), p)
}

func (c Config) statePath(name string) string {
	dir := c.StateDir
	if dir == "" {
		dir = ".varvig-factory"
	}
	return filepath.Join(c.path(dir), name)
}

// switchPath is where the kill switch lives.
func (c Config) switchPath() string {
	if c.Promotion.StatePath != "" {
		return c.path(c.Promotion.StatePath)
	}
	return c.statePath("promotion.json")
}

func (c Config) authoring() (executor.Authoring, error) {
	switch strings.ToLower(c.Inference.Kind) {
	case "", "none":
		return executor.None{}, nil
	case "http":
		return &executor.HTTP{
			Endpoint:     c.Inference.Endpoint,
			VersionURL:   c.Inference.VersionURL,
			Model:        c.Inference.Model,
			ModelVersion: c.Inference.ModelVersion,
			System:       c.Inference.System,
			AuthHeader:   c.Inference.AuthHeader,
			AuthValue:    os.Getenv(c.Inference.AuthValueEnv),
			Params:       c.params(),
		}, nil
	case "command":
		return &executor.Command{
			Path:         c.Inference.Path,
			Args:         c.Inference.Args,
			VersionArgs:  c.Inference.VersionArgs,
			Model:        c.Inference.Model,
			ModelVersion: c.Inference.ModelVersion,
			Params:       c.params(),
		}, nil
	}
	return nil, fmt.Errorf("profile: unknown executor.kind %q", c.Inference.Kind)
}

func (c Config) params() executor.Params {
	return executor.Params{
		Temperature: c.Inference.Temperature,
		TopP:        c.Inference.TopP,
		Seed:        c.Inference.Seed,
	}
}

func (c Config) probes() []executor.Probe {
	var out []executor.Probe
	for _, p := range c.Sandbox.Probes {
		// "go" with no command expands to the built-in probe, which knows to
		// strip the platform suffix `go version` appends.
		if strings.EqualFold(p.Key, "go") && len(p.Command) == 0 {
			out = append(out, executor.GoProbes()...)
			continue
		}
		out = append(out, executor.Probe{Key: p.Key, Command: p.Command})
	}
	return out
}

func (c Config) checking() (executor.Checking, error) {
	var box *executor.Exec
	switch strings.ToLower(c.Sandbox.Kind) {
	case "", "subprocess":
		box = executor.Subprocess(c.probes(), c.Sandbox.Flags)
	case "container":
		box = executor.Container(c.Sandbox.Runner, c.Sandbox.Image, c.probes(), c.Sandbox.Flags)
	case "nix":
		box = executor.Nix(c.Sandbox.Installable, c.probes(), c.Sandbox.Flags)
	default:
		return nil, fmt.Errorf("profile: unknown executor.kind %q", c.Sandbox.Kind)
	}
	box.Platform = c.Sandbox.Platform
	if d := c.Sandbox.Timeout.D(0); d > 0 {
		box.DefaultTimeout = d
	}
	return box, nil
}

func (c Config) store() (artifact.Store, error) {
	root := c.path(c.Artifacts.Root)
	if root == "" {
		root = c.statePath("artifacts")
	}
	local := &artifact.LocalCAS{Root: root}
	switch strings.ToLower(c.Artifacts.Kind) {
	case "", "local":
		return local, nil
	case "remote":
		return &artifact.Remote{
			Local:           local,
			PushCommand:     c.Artifacts.PushCommand,
			LocatorTemplate: c.Artifacts.LocatorTemplate,
			Label:           c.Artifacts.Label,
		}, nil
	}
	return nil, fmt.Errorf("profile: unknown artifacts.kind %q", c.Artifacts.Kind)
}

func (c Config) checks() []loop.Check {
	out := make([]loop.Check, 0, len(c.Checks))
	for _, chk := range c.Checks {
		kind := cell.RoleVerify
		if strings.EqualFold(chk.Kind, "build") {
			kind = cell.RoleBuild
		}
		out = append(out, loop.Check{
			Name:    chk.Name,
			Command: chk.Command,
			Timeout: chk.Timeout.D(0),
			Kind:    kind,
		})
	}
	return out
}

// connectorCapabilities names, by interface hash, the capabilities this cell
// leaves to a connector peer.
//
// Keyed by hash rather than alias because a connector serving a different
// interface under the same name is serving a different capability, and here that
// mistake would be resolved by spending money.
func (c Config) connectorCapabilities() map[string]bool {
	out := map[string]bool{}
	for _, e := range c.Effects.Capabilities {
		if e.Executor == "connector" {
			out[e.Interface] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WithInference attaches a model to a cell of any class, and grants it the
// attempt role.
//
// The role comes with the model because the two cannot honestly be separated in
// the *declaration*: a cell advertising that it attempts while declaring no
// model is advertising what it cannot do, which cell.Capabilities.Validate
// refuses. Whether the model answers right now is a different question, decided
// per pass and handled by declining rather than by refusing to run (§9.17).
//
// It does not touch the budget. A hosted model costs money per call and a local
// one on hardware already paid for costs nothing external, and this function
// cannot tell which it was handed — so the caller sets the cap, and §7.0 means
// leaving it unset is unenforced rather than zero.
func (c Config) WithInference(inf InferenceConfig) Config {
	c.Inference = inf
	if !hasRole(c.Roles, cell.RoleAttempt) {
		c.Roles = append([]cell.Role{cell.RoleAttempt}, c.Roles...)
	}
	return c
}

// WithoutInference makes a cell of any class a policy cell (§3): no model, and
// no attempt role.
//
// The role goes with the model for the same reason it arrives with it. What is
// left is not a degraded cell — it verifies, builds, executes effectful
// capabilities and syncs, and the evidence it produces is what licenses another
// cell's attempt to be promoted autonomously (§6.3.1).
//
// The inference budget is cleared too, because a cap on spending that cannot
// happen is a number that misleads whoever reads the config next.
func (c Config) WithoutInference() Config {
	c.Inference = InferenceConfig{Kind: "none", Tier: cell.TierNone}
	roles := make([]cell.Role, 0, len(c.Roles))
	for _, r := range c.Roles {
		if r != cell.RoleAttempt {
			roles = append(roles, r)
		}
	}
	c.Roles = roles
	c.Budget.InferenceDaily = 0
	c.Budget.OfflineInferenceDaily = 0
	c.Budget.PerCallCost = 0
	c.Budget.CostPerKTokenIn = 0
	c.Budget.CostPerKTokenOut = 0
	return c
}

func hasRole(roles []cell.Role, want cell.Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
