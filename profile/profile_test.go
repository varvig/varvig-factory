package profile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// TestWireIgnoresTheProfileName is FACTORY.md §1.2 held mechanically.
//
// Tiers are configuration profiles, not code paths: Micro and Mini must differ
// only in which model runtime and budget the config names. So Wire — the single
// function that turns a config into a running cell — must not read the profile
// name at all. A grep would be fooled by a comment; this reads the syntax tree of
// the function itself.
func TestWireIgnoresTheProfileName(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "profile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wire *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Wire" {
			wire = fn
		}
	}
	if wire == nil {
		t.Fatal("Wire not found; the guard cannot check what it cannot find")
	}
	ast.Inspect(wire, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "Profile" {
			pos := fset.Position(sel.Pos())
			t.Fatalf("Wire reads the profile name at %s; a tier that changes behaviour "+
				"anywhere but in config field values is a tier-specific code path (§1.2)", pos)
		}
		return true
	})
}

// TestTemplatesDifferOnlyInFieldValues is the other half of §1.2: the templates
// are the same type, and every difference between them is data.
func TestTemplatesDifferOnlyInFieldValues(t *testing.T) {
	micro := Micro("micro-a")
	mini := Mini("mini-a")
	medium := Medium("mini-b", "peer:9418")

	if reflect.TypeOf(micro) != reflect.TypeOf(mini) || reflect.TypeOf(mini) != reflect.TypeOf(medium) {
		t.Fatal("the templates are not the same type")
	}
	// Medium is Mini plus an upstream. If it ever diverges further, this notices —
	// "N cells plus an upstream peer" is a deployment, not a different binary.
	mediumAsMini := medium
	mediumAsMini.Profile = mini.Profile
	mediumAsMini.CellID = mini.CellID
	mediumAsMini.Upstream = mini.Upstream
	mediumAsMini.Branch = mini.Branch
	if !reflect.DeepEqual(mediumAsMini, mini) {
		t.Fatal("the medium template differs from mini in more than its upstream and branch")
	}
}

func TestMicroShipsAsAVerifyBuildCell(t *testing.T) {
	// §3.1: ship Micro with roles [verify, build] by default. Attempting is
	// opt-in, because a CPU-local model authoring code loses nearly every
	// selection while still consuming review attention.
	caps := Micro("micro-a").Capabilities()
	if caps.Has(cell.RoleAttempt) {
		t.Fatal("the micro template attempts by default")
	}
	if !caps.Has(cell.RoleVerify) || !caps.Has(cell.RoleBuild) {
		t.Fatalf("roles = %v, want build and verify", caps.Roles)
	}
	if caps.Inference.Tier != cell.TierNone {
		t.Fatalf("tier = %q, want %q", caps.Inference.Tier, cell.TierNone)
	}
	// And no inference budget: a verify/build cell should not hold one it could
	// only spend by being misconfigured.
	if Micro("micro-a").Budget.InferenceDaily != 0 {
		t.Fatal("the micro template declares an inference budget")
	}
}

func TestTemplatesValidate(t *testing.T) {
	for _, cfg := range []Config{Micro("micro-a"), Mini("mini-a"), Medium("mini-b", "peer:9418")} {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("the shipped %s template is invalid: %v", cfg.Profile, err)
		}
	}
}

func TestValidateCatchesAnAdvertisementThatDisagreesWithTheRuntime(t *testing.T) {
	// A cell advertising tier large while configured with no runtime would claim
	// tickets it can never attempt — and because claims are advisory, other cells
	// would see and respect those claims.
	c := Mini("mini-a")
	c.Inference.Kind = "none"
	if err := c.Validate(); err == nil {
		t.Fatal("a cell with no runtime advertising tier large was accepted")
	}

	c = Micro("micro-a")
	c.Inference.Kind = "http"
	c.Inference.Model = "m"
	if err := c.Validate(); err == nil {
		t.Fatal("a cell with a runtime advertising tier none was accepted")
	}

	c = Mini("mini-a")
	c.Inference.Model = ""
	if err := c.Validate(); err == nil {
		t.Fatal("a runtime with no model named was accepted")
	}
}

func TestValidateRequiresADigestPinnedImage(t *testing.T) {
	// A tag is mutable, so a tag-pinned sandbox publishes a stable environment
	// hash while the ground under it moves.
	c := Mini("mini-a")
	c.Sandbox.Kind = "container"
	c.Sandbox.Image = "registry.internal/ci:latest"
	err := c.Validate()
	if err == nil {
		t.Fatal("a tag-pinned image was accepted")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("the error does not explain the requirement: %v", err)
	}

	c.Sandbox.Image = "registry.internal/ci@sha256:abc"
	if err := c.Validate(); err != nil {
		t.Fatalf("a digest-pinned image was rejected: %v", err)
	}
}

func TestValidateRejectsAnUnknownAdapterKind(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Inference.Kind = "telepathy" },
		func(c *Config) { c.Sandbox.Kind = "chroot" },
		func(c *Config) { c.Artifacts.Kind = "ftp" },
	} {
		c := Mini("mini-a")
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("an unknown adapter kind was accepted: %+v", c)
		}
	}
}

func TestValidateRejectsARemoteStoreWithNoPush(t *testing.T) {
	c := Mini("mini-a")
	c.Artifacts.Kind = "remote"
	if err := c.Validate(); err == nil {
		t.Fatal("a remote store with no push command was accepted")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	// A typo'd "inference_daily" that quietly means "no cap" is the single worst
	// misconfiguration this file can carry.
	path := filepath.Join(t.TempDir(), "factory.json")
	if err := os.WriteFile(path, []byte(`{"cell_id":"mini-a","inferrence_daily":50}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("an unknown config field was silently ignored")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.json")
	original := Medium("mini-a", "peer:9418")
	if err := Save(path, original); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, loaded) {
		t.Fatalf("round trip lost data:\n %+v\n %+v", original, loaded)
	}
}

func TestDurationsAreStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.json")
	// A bare number is ambiguous — nanoseconds are what Go would pick and minutes
	// are what a person would mean — so only the string form is accepted.
	if err := os.WriteFile(path, []byte(`{"cell_id":"mini-a","claim_ttl":1800}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a numeric duration was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"cell_id":"mini-a","claim_ttl":"30m","roles":["verify"],"inference":{"kind":"none","tier":"none"},"sandbox":{"kind":"subprocess"},"artifacts":{"kind":"local"},"budget":{"inference_daily":0,"verify_concurrent":1,"storage_gb":1,"attempts_default":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaimTTL.D(0).Minutes() != 30 {
		t.Fatalf("claim ttl = %s", c.ClaimTTL.D(0))
	}
}

func TestWireProducesAUsableCell(t *testing.T) {
	dir := t.TempDir()
	cfg := Micro("micro-a")
	cfg.Repo = dir
	built, err := cfg.Wire(varvigcli.NewFake("micro-a"))
	if err != nil {
		t.Fatal(err)
	}
	if built.Cell == nil || built.Ledger == nil || built.Switch == nil {
		t.Fatalf("incomplete wiring: %+v", built)
	}
	// Gated by default, everywhere.
	mode, err := built.Switch.Mode()
	if err != nil {
		t.Fatal(err)
	}
	if string(mode) != "gated" {
		t.Fatalf("a freshly wired cell is in %q mode", mode)
	}
	// The re-verifier is the cell itself, so re-verification and verification are
	// the same act (§6.3 condition 3).
	if built.Cell.Promoter.Reverify == nil {
		t.Fatal("no re-verifier is wired; autonomous promotion could only replay evidence")
	}
	// Relative state paths resolve inside the repository, so a cell can be moved
	// without editing every path.
	if !strings.HasPrefix(built.Cell.WorkDir, dir) {
		t.Fatalf("work dir %q is not inside the repo %q", built.Cell.WorkDir, dir)
	}
}

func TestGoProbeIsExpandedFromAKeyAlone(t *testing.T) {
	// "go" with no command is the common case, and the built-in probe knows to
	// strip the platform suffix `go version` appends.
	c := Micro("micro-a")
	probes := c.probes()
	if len(probes) != 1 || probes[0].Key != "go" || probes[0].Extract == nil {
		t.Fatalf("probes = %+v, want the built-in go probe", probes)
	}
}

func TestChecksCarryTheirRole(t *testing.T) {
	c := Micro("micro-a")
	checks := c.checks()
	if len(checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(checks))
	}
	kinds := map[cell.Role]bool{}
	for _, chk := range checks {
		kinds[chk.Kind] = true
	}
	// A build-only cell must not run a test suite it never claimed to support, so
	// each check names the role it belongs to.
	if !kinds[cell.RoleBuild] || !kinds[cell.RoleVerify] {
		t.Fatalf("checks do not carry both roles: %+v", checks)
	}
}

func TestTemplateRejectsAnUnknownName(t *testing.T) {
	if _, err := Template("macro", "a", ""); err == nil {
		t.Fatal("an unknown profile name was accepted")
	}
}
