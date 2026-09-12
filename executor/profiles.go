package executor

import (
	"strings"
	"time"
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
