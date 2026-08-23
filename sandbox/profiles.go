package sandbox

import (
	"context"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

// The constructors below are the whole of the "three sandbox implementations"
// in FACTORY.md §4. Each returns the same *Exec with a different Wrapper, which
// is what "configuration profiles, not code paths" means in practice (§1.2):
// there is one Run, one probe path, and one place a bug can live.

// Subprocess runs jobs directly on the host. It is the right choice when the
// isolation comes from somewhere else — a dedicated machine, a VM, a CI runner —
// and the wrong choice when untrusted model output is about to be executed.
func Subprocess(probes []Probe, flags map[string]string) *Exec {
	return &Exec{
		Probes:         probes,
		Flags:          flags,
		Label:          "subprocess",
		DefaultTimeout: 30 * time.Minute,
	}
}

// Container runs jobs inside an image. imageDigest must be a digest, not a tag:
// a tag is mutable, so a tag-pinned sandbox would keep publishing a stable
// environment hash while the ground under it moved — the one failure that makes
// every comparison downstream quietly wrong.
//
// runner is the container command, e.g. {"docker"} or {"podman"}. The mount and
// working directory are supplied here rather than by the caller so that every
// container cell mounts the checkout the same way.
func Container(runner []string, imageDigest string, probes []Probe, flags map[string]string) *Exec {
	if len(runner) == 0 {
		runner = []string{"docker"}
	}
	wrapper := append(append([]string(nil), runner...),
		"run", "--rm",
		// No network during checks by default: a test that reaches the internet
		// is not reproducible, and its evidence would assert a property of a
		// mirror rather than of the code.
		"--network", "none",
		"-v", "{{dir}}:/work",
		"-w", "/work",
		imageDigest,
	)
	return &Exec{
		Wrapper:        wrapper,
		Probes:         probes,
		Flags:          flags,
		Container:      containerHash(imageDigest),
		Label:          "container",
		DefaultTimeout: 30 * time.Minute,
	}
}

// Nix runs jobs in a nix development shell. installable is a flake reference,
// e.g. ".#ci". The shell is entered with --ignore-environment so that the host's
// PATH cannot leak a toolchain into a run that the environment descriptor does
// not mention.
func Nix(installable string, probes []Probe, flags map[string]string) *Exec {
	return &Exec{
		Wrapper: []string{
			"nix", "develop", "--ignore-environment", installable, "-c",
		},
		Probes:         probes,
		Flags:          flags,
		Label:          "nix",
		DefaultTimeout: 30 * time.Minute,
	}
}

// containerHash normalizes an image reference to the artifact content hash the
// environment descriptor carries. "registry/img@sha256:abc" becomes
// "sha256:abc"; a reference with no digest yields the empty string, so a
// tag-pinned image records no container rather than recording a mutable name as
// if it were an identity.
func containerHash(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// GoProbes are the probes for a Go toolchain: the common case, and an example of
// an Extract that drops the platform suffix `go version` appends. The suffix is
// dropped because the platform is already its own field — recording it twice
// would make two representations of one fact that could disagree.
func GoProbes() []Probe {
	return []Probe{{
		Key:     "go",
		Command: []string{"go", "version"},
		Extract: func(line string) string {
			// "go version go1.24.7 linux/amd64" -> "1.24.7"
			fields := strings.Fields(line)
			for _, f := range fields {
				if v, ok := strings.CutPrefix(f, "go1."); ok {
					return "1." + v
				}
			}
			return line
		},
	}}
}

// Fake is a Sandbox for tests and the demo: it reports a fixed fragment and
// returns scripted results without running anything. Like inference.Fake it is
// in the non-test build so the full lifecycle is exercisable on a machine with
// no toolchain at all.
type Fake struct {
	// Platform and Toolchains form the reported fragment.
	Platform   string
	Toolchains map[string]string
	Flags      map[string]string
	ContainerH string
	// Results maps a job name to the status it should report. A job with no
	// entry passes, so a test only has to name the checks it wants to fail.
	Results map[string]cell.Status
	// Err, if set, is returned by Run.
	Err error
	// Indescribable makes Fragment fail.
	Indescribable bool
	// Ran records the job names Run was called with, in order.
	Ran []string
}

// Name implements Sandbox.
func (f *Fake) Name() string { return "fake" }

// Fragment implements Sandbox.
func (f *Fake) Fragment(context.Context) (cell.Fragment, error) {
	if f.Indescribable {
		return cell.Fragment{}, ErrIndescribable
	}
	platform := f.Platform
	if platform == "" {
		platform = "linux/amd64"
	}
	tc := f.Toolchains
	if tc == nil {
		tc = map[string]string{"go": "1.24.7"}
	}
	return cell.Fragment{Platform: platform, Toolchains: tc, Flags: f.Flags, Container: f.ContainerH}, nil
}

// Run implements Sandbox.
func (f *Fake) Run(_ context.Context, job Job) (Result, error) {
	f.Ran = append(f.Ran, job.Name)
	if f.Err != nil {
		return Result{}, f.Err
	}
	status, ok := f.Results[job.Name]
	if !ok {
		status = cell.StatusPass
	}
	return Result{Name: job.Name, Status: status, DurationMS: 1}, nil
}
