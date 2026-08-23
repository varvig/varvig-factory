package cell

import (
	"fmt"
	"sort"
)

// EnvModel identifies the inference model behind a piece of evidence. It is
// present only when the evidence was produced by inference: build and test
// evidence has no model, and inventing one would make a deterministic
// environment look like a sampled one (CELL.md §4.2).
type EnvModel struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Params  string `json:"params,omitempty"`
}

// Environment is the answer to *against what*. Evidence records what was
// checked and with what result; the environment records the ground it was
// checked on, so cross-peer selection compares like with like instead of
// comparing "tests passed" on one toolchain against "tests passed" on another
// while appearing rigorous.
//
// The field set mirrors varvig's TypeEnvironment (FEDERATION.md §2) so that when
// varvig grows a native environment CLI, the note form here can be replaced by
// the object form without changing this shape or any consumer of it.
type Environment struct {
	Platform   string            `json:"platform,omitempty"`
	Toolchains map[string]string `json:"toolchains,omitempty"`
	Flags      map[string]string `json:"flags,omitempty"`
	Container  string            `json:"container,omitempty"`
	Model      *EnvModel         `json:"model,omitempty"`
}

// Fragment is one adapter's slice of an environment (CELL.md §6). Each adapter
// reports a fragment and the cell merges them; an adapter that cannot describe
// itself reproducibly cannot participate in cross-cell selection.
//
// A fragment is measured, not configured. An adapter that reports its toolchain
// version from a config file rather than from the toolchain it will actually
// invoke is making a claim, and the environment hash would then certify a claim
// instead of a measurement.
type Fragment struct {
	Platform   string
	Toolchains map[string]string
	Flags      map[string]string
	Container  string
	Model      *EnvModel
}

// ErrFragmentConflict reports two adapters disagreeing about the same key. It is
// a hard error rather than a last-writer-wins merge: if the sandbox says Go
// 1.24.7 and the model runtime says Go 1.22, one of them is wrong about the
// machine they are both running on, and emitting either value produces an
// environment hash that certifies a fiction.
type ErrFragmentConflict struct {
	Field string
	Key   string
	A, B  string
}

func (e ErrFragmentConflict) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("cell: adapters disagree on %s: %q vs %q", e.Field, e.A, e.B)
	}
	return fmt.Sprintf("cell: adapters disagree on %s[%q]: %q vs %q", e.Field, e.Key, e.A, e.B)
}

// MergeFragments folds adapter fragments into one environment descriptor. Merge
// order does not affect the result: equal values unify, unequal values are a
// conflict, so the operation is commutative and the environment hash cannot
// depend on which adapter was constructed first.
func MergeFragments(frags ...Fragment) (Environment, error) {
	var env Environment
	for _, f := range frags {
		if f.Platform != "" {
			if env.Platform != "" && env.Platform != f.Platform {
				return Environment{}, ErrFragmentConflict{Field: "platform", A: env.Platform, B: f.Platform}
			}
			env.Platform = f.Platform
		}
		if f.Container != "" {
			if env.Container != "" && env.Container != f.Container {
				return Environment{}, ErrFragmentConflict{Field: "container", A: env.Container, B: f.Container}
			}
			env.Container = f.Container
		}
		if f.Model != nil {
			if env.Model != nil && *env.Model != *f.Model {
				return Environment{}, ErrFragmentConflict{Field: "model", A: env.Model.ID, B: f.Model.ID}
			}
			m := *f.Model
			env.Model = &m
		}
		var err error
		if env.Toolchains, err = mergeMap(env.Toolchains, f.Toolchains, "toolchains"); err != nil {
			return Environment{}, err
		}
		if env.Flags, err = mergeMap(env.Flags, f.Flags, "flags"); err != nil {
			return Environment{}, err
		}
	}
	return env, nil
}

func mergeMap(into, from map[string]string, field string) (map[string]string, error) {
	if len(from) == 0 {
		return into, nil
	}
	if into == nil {
		into = make(map[string]string, len(from))
	}
	for k, v := range from {
		if old, ok := into[k]; ok && old != v {
			return nil, ErrFragmentConflict{Field: field, Key: k, A: old, B: v}
		}
		into[k] = v
	}
	return into, nil
}

// Hash is the environment hash of CELL.md §4.3 — canonical JSON, then the
// labelled digest. Two invocations of the same adapter set produce the same
// value (FACTORY.md §9.4); an environment that embeds a timestamp, a hostname, a
// PID or a working directory has failed that and is a bug, not a variation.
func (e Environment) Hash() (string, error) { return CanonicalHash(e) }

// Empty reports whether the descriptor says nothing. Evidence carrying an empty
// environment is unknown class and never matches (CELL.md §4.4), so this is
// checked rather than tolerated.
func (e Environment) Empty() bool {
	return e.Platform == "" && len(e.Toolchains) == 0 && len(e.Flags) == 0 &&
		e.Container == "" && e.Model == nil
}

// Class is the comparability verdict between two environments (CELL.md §4.4).
type Class int

// The three verdicts. There are three rather than two because "we cannot
// compare these" is a real answer and the one that has to reach a human: folding
// it into either "same" or "different" is how a cross-toolchain comparison ends
// up looking rigorous.
const (
	// ClassUnknown means at least one environment says nothing. It never
	// matches anything, including another unknown.
	ClassUnknown Class = iota
	// ClassSame means the platforms match and no shared toolchain key
	// disagrees.
	ClassSame
	// ClassCross means both are known and they differ.
	ClassCross
)

func (c Class) String() string {
	switch c {
	case ClassSame:
		return "same-class"
	case ClassCross:
		return "cross-class"
	default:
		return "unknown-class"
	}
}

// CompareClass reports whether a and b are the same environment class.
//
// Same class means: the platforms match, and every toolchain key *both* declare
// agrees. Keys only one side declares do not make them cross-class — a verifier
// that also has a linter installed is still the same ground as one that does
// not. A disagreement on a shared key is cross-class, because that is the case
// where "tests passed" on each side means something different.
func CompareClass(a, b Environment) Class {
	if a.Empty() || b.Empty() {
		return ClassUnknown
	}
	if a.Platform != b.Platform {
		return ClassCross
	}
	for k, av := range a.Toolchains {
		if bv, ok := b.Toolchains[k]; ok && av != bv {
			return ClassCross
		}
	}
	return ClassSame
}

// SameClass is CompareClass reduced to a boolean, for the many call sites that
// only need the admit/defer decision. ClassUnknown is false: absent evidence of
// comparability is not evidence of it.
func SameClass(a, b Environment) bool { return CompareClass(a, b) == ClassSame }

// ToolchainKeys returns the sorted toolchain keys, for stable rendering.
func (e Environment) ToolchainKeys() []string {
	keys := make([]string, 0, len(e.Toolchains))
	for k := range e.Toolchains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
