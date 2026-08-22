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
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/gate"
	"github.com/varvig/varvig-factory/inference"
	"github.com/varvig/varvig-factory/loop"
	"github.com/varvig/varvig-factory/promote"
	"github.com/varvig/varvig-factory/sandbox"
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

// SandboxConfig names a build sandbox.
type SandboxConfig struct {
	// Kind is "subprocess", "container", or "nix".
	Kind string `json:"kind"`
	// Runner is the container command, e.g. ["docker"] or ["podman"].
	Runner []string `json:"runner,omitempty"`
	// Image is the container image, which must be digest-pinned.
	Image string `json:"image,omitempty"`
	// Installable is the nix flake reference, e.g. ".#ci".
	Installable string `json:"installable,omitempty"`
	// Probes measure toolchain versions inside the sandbox. "go" is expanded to
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
	// Repo is the varvig repository this cell works in.
	Repo string `json:"repo,omitempty"`
	// VarvigBin overrides the `varvig` binary.
	VarvigBin string `json:"varvig_bin,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Branch    string `json:"branch,omitempty"`

	Roles []cell.Role `json:"roles"`
	Build []string    `json:"build,omitempty"`
	Test  []string    `json:"test,omitempty"`

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

// Mini is the GPU-local profile: attempting enabled, same binary, config only
// (§10.4). Every difference from Micro below is a field value.
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
		InferenceDaily:   50,
		PerCallCost:      0.02,
		VerifyConcurrent: 4,
		StorageGB:        200,
		AttemptsDefault:  3,
	}
	return c
}

// Medium is a Mini cell pointed at an upstream peer — the federation-wide tier
// (§3). It differs from Mini in one field, which is the honest shape of the
// difference: "N cells plus an upstream peer" is a deployment, not a tier of
// binary.
func Medium(cellID, upstream string) Config {
	c := Mini(cellID)
	c.Profile = "medium"
	c.Upstream = upstream
	c.Branch = "refs/heads/main"
	return c
}

// Template returns a named starting profile.
func Template(name, cellID, upstream string) (Config, error) {
	switch strings.ToLower(name) {
	case "micro":
		return Micro(cellID), nil
	case "mini":
		return Mini(cellID), nil
	case "medium":
		return Medium(cellID, upstream), nil
	}
	return Config{}, fmt.Errorf("profile: unknown profile %q (want micro, mini or medium)", name)
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
	caps.Normalize()
	return caps
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
			return fmt.Errorf("profile: inference.kind is %q but inference.tier is %q; a cell with no runtime must advertise tier %q",
				c.Inference.Kind, c.Inference.Tier, cell.TierNone)
		}
	case "http", "command":
		if c.Inference.Tier == cell.TierNone {
			return fmt.Errorf("profile: inference.kind is %q but inference.tier is %q; a cell with a runtime must advertise the tier it is",
				c.Inference.Kind, cell.TierNone)
		}
		if c.Inference.Model == "" {
			return fmt.Errorf("profile: inference.kind is %q but no model is named", c.Inference.Kind)
		}
	default:
		return fmt.Errorf("profile: unknown inference.kind %q (want none, http or command)", c.Inference.Kind)
	}
	switch strings.ToLower(c.Sandbox.Kind) {
	case "", "subprocess":
	case "container":
		if c.Sandbox.Image == "" {
			return fmt.Errorf("profile: sandbox.kind is container but no image is named")
		}
		if !strings.Contains(c.Sandbox.Image, "@") {
			// A tag is mutable. A tag-pinned sandbox publishes a stable
			// environment hash while the ground under it moves, which makes every
			// cross-cell comparison against it quietly wrong.
			return fmt.Errorf("profile: sandbox.image %q is not digest-pinned; use image@sha256:… so the environment hash means something", c.Sandbox.Image)
		}
	case "nix":
		if c.Sandbox.Installable == "" {
			return fmt.Errorf("profile: sandbox.kind is nix but no installable is named")
		}
	default:
		return fmt.Errorf("profile: unknown sandbox.kind %q (want subprocess, container or nix)", c.Sandbox.Kind)
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
	Cell     *loop.Cell
	Varvig   varvigcli.Varvig
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
	if v == nil {
		v = varvigcli.Exec{Bin: c.VarvigBin, Dir: c.repo()}
	}

	ledger, err := budget.NewLedger(c.Budget, c.statePath("ledger.json"), time.Now())
	if err != nil {
		return Built{}, err
	}
	sw, err := promote.NewSwitch(c.switchPath())
	if err != nil {
		return Built{}, err
	}

	runtime, err := c.runtime()
	if err != nil {
		return Built{}, err
	}
	box, err := c.sandbox()
	if err != nil {
		return Built{}, err
	}
	store, err := c.store()
	if err != nil {
		return Built{}, err
	}

	g := gate.Module{V: v}
	cl := &loop.Cell{
		Capabilities:       c.Capabilities(),
		V:                  v,
		Inference:          runtime,
		Sandbox:            box,
		Artifacts:          store,
		Ledger:             ledger,
		Upstream:           c.Upstream,
		Branch:             c.Branch,
		Checks:             c.checks(),
		ClaimTTL:           c.ClaimTTL.D(30 * time.Minute),
		TaskTTL:            c.TaskTTL.D(2 * time.Hour),
		WorkDir:            c.path(c.WorkDir),
		ArtifactGlobs:      c.ArtifactGlobs,
		Baselines:          c.Promotion.Baselines,
		YieldToFreshClaims: c.YieldToFreshClaims,
		MaxAttemptsPerCell: c.MaxAttemptsPerCell,
	}
	cl.Promoter = &promote.Promoter{
		V:           v,
		Switch:      sw,
		Gate:        g,
		Agreement:   agreement.NewGate(c.Promotion.Threshold, c.Promotion.MinObservations),
		Reverify:    cl,
		CellID:      c.CellID,
		Fingerprint: c.Promotion.Fingerprint,
	}
	return Built{
		Cell:     cl,
		Varvig:   v,
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

func (c Config) runtime() (inference.Runtime, error) {
	switch strings.ToLower(c.Inference.Kind) {
	case "", "none":
		return inference.None{}, nil
	case "http":
		return &inference.HTTPRuntime{
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
		return &inference.CommandRuntime{
			Path:         c.Inference.Path,
			Args:         c.Inference.Args,
			VersionArgs:  c.Inference.VersionArgs,
			Model:        c.Inference.Model,
			ModelVersion: c.Inference.ModelVersion,
			Params:       c.params(),
		}, nil
	}
	return nil, fmt.Errorf("profile: unknown inference.kind %q", c.Inference.Kind)
}

func (c Config) params() inference.Params {
	return inference.Params{
		Temperature: c.Inference.Temperature,
		TopP:        c.Inference.TopP,
		Seed:        c.Inference.Seed,
	}
}

func (c Config) probes() []sandbox.Probe {
	var out []sandbox.Probe
	for _, p := range c.Sandbox.Probes {
		// "go" with no command expands to the built-in probe, which knows to
		// strip the platform suffix `go version` appends.
		if strings.EqualFold(p.Key, "go") && len(p.Command) == 0 {
			out = append(out, sandbox.GoProbes()...)
			continue
		}
		out = append(out, sandbox.Probe{Key: p.Key, Command: p.Command})
	}
	return out
}

func (c Config) sandbox() (sandbox.Sandbox, error) {
	var box *sandbox.Exec
	switch strings.ToLower(c.Sandbox.Kind) {
	case "", "subprocess":
		box = sandbox.Subprocess(c.probes(), c.Sandbox.Flags)
	case "container":
		box = sandbox.Container(c.Sandbox.Runner, c.Sandbox.Image, c.probes(), c.Sandbox.Flags)
	case "nix":
		box = sandbox.Nix(c.Sandbox.Installable, c.probes(), c.Sandbox.Flags)
	default:
		return nil, fmt.Errorf("profile: unknown sandbox.kind %q", c.Sandbox.Kind)
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
