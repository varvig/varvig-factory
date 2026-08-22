package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

// TestFragmentIsDeterministicAcrossInvocations is FACTORY.md §9.4 at the adapter
// layer: the same adapter, two invocations, identical environment hash.
func TestFragmentIsDeterministicAcrossInvocations(t *testing.T) {
	probes := []Probe{
		{Key: "echo-a", Command: []string{"echo", "1.0.0"}},
		{Key: "echo-b", Command: []string{"echo", "2.0.0"}},
	}
	hash := func() string {
		// A fresh Exec each time, so the cached fragment cannot be what makes
		// the two hashes agree — that would test the cache, not the adapter.
		e := Subprocess(probes, map[string]string{"CGO_ENABLED": "0"})
		frag, err := e.Fragment(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		env, err := cell.MergeFragments(frag)
		if err != nil {
			t.Fatal(err)
		}
		h, err := env.Hash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if a, b := hash(), hash(); a != b {
		t.Fatalf("environment hash is not deterministic across invocations:\n %s\n %s", a, b)
	}
}

func TestFragmentReportsHostPlatformAndProbedVersions(t *testing.T) {
	e := Subprocess([]Probe{{Key: "tool", Command: []string{"echo", "v9.9.9"}}}, nil)
	frag, err := e.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; frag.Platform != want {
		t.Fatalf("platform = %q, want %q", frag.Platform, want)
	}
	if got := frag.Toolchains["tool"]; got != "v9.9.9" {
		t.Fatalf("probed version = %q, want v9.9.9", got)
	}
}

func TestFragmentRefusesWhenAProbeCannotRun(t *testing.T) {
	// An adapter that cannot describe itself must say so rather than emit a
	// partial environment: a partial environment still hashes, and the hash
	// would then certify a fiction (FACTORY.md §4).
	e := Subprocess([]Probe{{Key: "missing", Command: []string{"definitely-not-a-real-binary-xyz"}}}, nil)
	if _, err := e.Fragment(context.Background()); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("err = %v, want ErrIndescribable", err)
	}
}

func TestFragmentRefusesAProbeThatProducesNothing(t *testing.T) {
	e := Subprocess([]Probe{{Key: "quiet", Command: []string{"true"}}}, nil)
	if _, err := e.Fragment(context.Background()); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("err = %v, want ErrIndescribable", err)
	}
}

func TestGoProbeExtractsBareVersion(t *testing.T) {
	extract := GoProbes()[0].Extract
	if got := extract("go version go1.24.7 linux/amd64"); got != "1.24.7" {
		t.Fatalf("extract = %q, want 1.24.7", got)
	}
	// The platform suffix is dropped, so two hosts differing only in platform
	// do not also disagree on the toolchain — the platform is its own field.
	a := extract("go version go1.24.7 linux/amd64")
	b := extract("go version go1.24.7 darwin/arm64")
	if a != b {
		t.Fatalf("platform leaked into the toolchain token: %q vs %q", a, b)
	}
}

func TestRunClassifiesOutcomes(t *testing.T) {
	dir := t.TempDir()
	e := Subprocess(nil, nil)
	ctx := context.Background()

	pass, err := e.Run(ctx, Job{Name: "ok", Dir: dir, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if pass.Status != cell.StatusPass {
		t.Fatalf("status = %q, want pass", pass.Status)
	}

	// A nonzero exit is a measurement, not a malfunction: StatusFail and a nil
	// error.
	fail, err := e.Run(ctx, Job{Name: "no", Dir: dir, Command: []string{"false"}})
	if err != nil {
		t.Fatalf("a failing check returned an error: %v", err)
	}
	if fail.Status != cell.StatusFail {
		t.Fatalf("status = %q, want fail", fail.Status)
	}

	// A binary that will not start is StatusError: the code did not fail, the
	// harness did.
	broken, err := e.Run(ctx, Job{Name: "gone", Dir: dir, Command: []string{"definitely-not-a-real-binary-xyz"}})
	if err != nil {
		t.Fatal(err)
	}
	if broken.Status != cell.StatusError {
		t.Fatalf("status = %q, want error", broken.Status)
	}
}

func TestRunTimeoutIsErrorNotFailure(t *testing.T) {
	// A slow machine must not look like broken code (CELL.md §4.1).
	e := Subprocess(nil, nil)
	res, err := e.Run(context.Background(), Job{
		Name:    "slow",
		Dir:     t.TempDir(),
		Command: []string{"sleep", "5"},
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != cell.StatusError {
		t.Fatalf("status = %q, want error", res.Status)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Fatalf("output does not mention the timeout: %q", res.Output)
	}
}

func TestRunUsesTheJobDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := Subprocess(nil, nil)
	res, err := e.Run(context.Background(), Job{Name: "ls", Dir: dir, Command: []string{"cat", "marker"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != cell.StatusPass {
		t.Fatalf("command did not run in the job directory: %+v", res)
	}
}

func TestWrapperSubstitutesTheDirectoryAndAppliesToProbes(t *testing.T) {
	// The container and nix profiles differ from subprocess only in Wrapper, so
	// the property that matters is that probes go through it too: otherwise a
	// container cell publishes its host's toolchain versions and compares as
	// same-class with a cell that has no container at all.
	e := &Exec{
		Wrapper: []string{"env", "MARKER={{dir}}", "sh", "-c"},
		Probes:  []Probe{{Key: "marker", Command: []string{"echo ${MARKER:-unset}"}}},
	}
	frag, err := e.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if frag.Toolchains["marker"] == "" {
		t.Fatalf("probe did not run through the wrapper: %+v", frag.Toolchains)
	}

	argv := e.wrap("/checkout", []string{"go", "test"})
	want := []string{"env", "MARKER=/checkout", "sh", "-c", "go", "test"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("wrap = %v, want %v", argv, want)
	}
}

func TestContainerRecordsADigestAndIgnoresATag(t *testing.T) {
	withDigest := Container(nil, "registry.internal/varvig/ci@sha256:abc123", nil, nil)
	if withDigest.Container != "sha256:abc123" {
		t.Fatalf("container hash = %q, want sha256:abc123", withDigest.Container)
	}
	// A tag is mutable, so it must not be recorded as an identity: doing so
	// would keep the environment hash stable while the ground moved.
	withTag := Container(nil, "registry.internal/varvig/ci:latest", nil, nil)
	if withTag.Container != "" {
		t.Fatalf("a tag was recorded as a container identity: %q", withTag.Container)
	}
}

func TestContainerAndNixAreConfigurationNotCodePaths(t *testing.T) {
	// FACTORY.md §1.2: the three sandbox profiles must be the same type with a
	// different Wrapper. If one of them ever becomes its own implementation,
	// this stops compiling — which is the point.
	var _ *Exec = Subprocess(nil, nil)
	var _ *Exec = Container(nil, "img@sha256:abc", nil, nil)
	var _ *Exec = Nix(".#ci", nil, nil)

	if len(Subprocess(nil, nil).Wrapper) != 0 {
		t.Fatal("subprocess has a wrapper")
	}
	if got := Container([]string{"podman"}, "img@sha256:abc", nil, nil).Wrapper; len(got) == 0 || got[0] != "podman" {
		t.Fatalf("container runner = %v, want podman first", got)
	}
	if len(Nix(".#ci", nil, nil).Wrapper) == 0 {
		t.Fatal("nix has no wrapper")
	}
}

func TestContainerRunsWithoutNetwork(t *testing.T) {
	// A check that reaches the internet is not reproducible, and its evidence
	// would assert a property of a mirror rather than of the code.
	w := strings.Join(Container(nil, "img@sha256:abc", nil, nil).Wrapper, " ")
	if !strings.Contains(w, "--network none") {
		t.Fatalf("container wrapper does not disable the network: %s", w)
	}
}

func TestResultCheckCarriesTheStatusVerbatim(t *testing.T) {
	r := Result{Name: "unit", Status: cell.StatusFail, DurationMS: 12, Output: "TestFoo failed\nmore"}
	c := r.Check()
	if c.Status != cell.StatusFail || c.Name != "unit" || c.DurationMS != 12 {
		t.Fatalf("check = %+v", c)
	}
	if c.Detail == "" {
		t.Fatal("check carries no detail for a human")
	}
}
