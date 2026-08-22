package cell

import (
	"strings"
	"testing"
	"time"
)

func TestCanonicalSortsKeysRegardlessOfInsertionOrder(t *testing.T) {
	a := map[string]any{"b": 1, "a": 2, "c": map[string]any{"z": 1, "y": 2}}
	b := map[string]any{"c": map[string]any{"y": 2, "z": 1}, "a": 2, "b": 1}

	ba, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ba) != string(bb) {
		t.Fatalf("canonical form depends on insertion order:\n %s\n %s", ba, bb)
	}
	if got, want := string(ba), `{"a":2,"b":1,"c":{"y":2,"z":1}}`; got != want {
		t.Fatalf("canonical form = %s, want %s", got, want)
	}
}

func TestCanonicalPreservesLargeIntegers(t *testing.T) {
	// A size or a nanosecond timestamp above 2^53 must survive. Round-tripping
	// through float64 would silently corrupt it, and the corruption would only
	// show up as an artifact that cannot be found in a registry.
	const big = int64(9007199254740993) // 2^53 + 1
	b, err := Canonical(map[string]any{"size": big})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"size":9007199254740993}`; got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalDoesNotEscapeHTML(t *testing.T) {
	b, err := Canonical(map[string]any{"locator": "oci://reg/x?a=1&b=2"})
	if err != nil {
		t.Fatal(err)
	}
	// Go's default encoder would render "&" as \u0026: legal, but not shortest,
	// and unreadable in the note a human eventually reads.
	if strings.Contains(string(b), `\u0026`) {
		t.Fatalf("locator was HTML-escaped: %s", b)
	}
	if !strings.Contains(string(b), "a=1&b=2") {
		t.Fatalf("locator did not survive canonicalization: %s", b)
	}
}

// TestEnvironmentHashIsDeterministic is FACTORY.md §9.4 at the contract layer:
// the same descriptor, built twice with different map insertion orders, hashes
// identically. §9.4's adapter-level twin lives in the sandbox and inference
// packages.
func TestEnvironmentHashIsDeterministic(t *testing.T) {
	// Same facts, inserted in opposite orders. Go randomizes map iteration, so
	// an encoder that leaned on it would fail this test intermittently — which
	// is the failure mode worth pinning down, because intermittently unequal
	// environment hashes look like genuine cross-class evidence.
	versions := map[string]string{"go": "1.24.7", "gotestsum": "1.12.0", "clang": "18.1.8", "node": "22.11.0"}
	mk := func(order int) Environment {
		keys := []string{"go", "gotestsum", "clang", "node"}
		if order == 1 {
			keys = []string{"node", "clang", "gotestsum", "go"}
		}
		tc := map[string]string{}
		for _, k := range keys {
			tc[k] = versions[k]
		}
		flags := map[string]string{}
		flags["CGO_ENABLED"] = "0"
		flags["GOFLAGS"] = "-mod=readonly"
		return Environment{
			Platform:   "linux/amd64",
			Toolchains: tc,
			Flags:      flags,
			Model:      &EnvModel{ID: "qwen2.5-coder", Version: "32b-q4", Params: "temp=0.2"},
		}
	}
	h0, err := mk(0).Hash()
	if err != nil {
		t.Fatal(err)
	}
	h1, err := mk(1).Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h0 != h1 {
		t.Fatalf("environment hash is not deterministic:\n %s\n %s", h0, h1)
	}
	if !strings.HasPrefix(h0, HashAlgorithm+":") {
		t.Fatalf("hash %q is missing its self-describing label", h0)
	}
}

func TestEnvironmentHashDistinguishesModelFromNoModel(t *testing.T) {
	// An absent model and a present-but-empty model must not collide: one is
	// deterministic build evidence, the other is sampled inference evidence.
	withModel := Environment{Platform: "linux/amd64", Model: &EnvModel{ID: "m"}}
	without := Environment{Platform: "linux/amd64"}
	a, _ := withModel.Hash()
	b, _ := without.Hash()
	if a == b {
		t.Fatal("model presence does not change the environment hash")
	}
}

func TestMergeFragmentsIsOrderIndependentAndConflictsLoudly(t *testing.T) {
	sandbox := Fragment{
		Platform:   "linux/amd64",
		Toolchains: map[string]string{"go": "1.24.7"},
		Flags:      map[string]string{"CGO_ENABLED": "0"},
	}
	model := Fragment{
		Toolchains: map[string]string{"ollama": "0.5.1"},
		Model:      &EnvModel{ID: "qwen", Version: "32b"},
	}
	store := Fragment{Container: "sha256:deadbeef"}

	forward, err := MergeFragments(sandbox, model, store)
	if err != nil {
		t.Fatal(err)
	}
	backward, err := MergeFragments(store, model, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	hf, _ := forward.Hash()
	hb, _ := backward.Hash()
	if hf != hb {
		t.Fatalf("merge is not commutative: %s vs %s", hf, hb)
	}

	// Two adapters disagreeing about the machine they both run on is a hard
	// error: picking either value would certify a fiction.
	clash := Fragment{Toolchains: map[string]string{"go": "1.22.0"}}
	if _, err := MergeFragments(sandbox, clash); err == nil {
		t.Fatal("conflicting toolchain versions merged silently")
	} else if !strings.Contains(err.Error(), "go") {
		t.Fatalf("conflict error does not name the key: %v", err)
	}
}

func TestCompareClass(t *testing.T) {
	base := Environment{Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7"}}
	cases := []struct {
		name string
		a, b Environment
		want Class
	}{
		{"identical", base, base, ClassSame},
		{
			"extra key on one side is still same class",
			base,
			Environment{Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7", "shellcheck": "0.10"}},
			ClassSame,
		},
		{
			"shared key disagrees",
			base,
			Environment{Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.22.0"}},
			ClassCross,
		},
		{
			"platform differs",
			base,
			Environment{Platform: "darwin/arm64", Toolchains: map[string]string{"go": "1.24.7"}},
			ClassCross,
		},
		{"empty on one side", base, Environment{}, ClassUnknown},
		{"empty on both sides", Environment{}, Environment{}, ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareClass(tc.a, tc.b); got != tc.want {
				t.Fatalf("CompareClass = %v, want %v", got, tc.want)
			}
			if got := CompareClass(tc.b, tc.a); got != tc.want {
				t.Fatalf("CompareClass is not symmetric: reverse = %v, want %v", got, tc.want)
			}
		})
	}
	// Unknown never matches, including against itself: absent evidence of
	// comparability is not evidence of it.
	if SameClass(Environment{}, Environment{}) {
		t.Fatal("two unknown-class environments compared equal")
	}
}

func TestCapabilitiesNormalizeIsStableAndIdempotent(t *testing.T) {
	a := Capabilities{
		CellID:    "mini-a",
		Inference: Inference{Tier: TierLarge, Models: []Model{{ID: "b"}, {ID: "a", Version: "2"}, {ID: "a", Version: "1"}}},
		Build:     []string{"go", "flutter", "go"},
		Test:      []string{"unit", "integration"},
		Roles:     []Role{RoleVerify, RoleAttempt, RoleBuild, RoleVerify},
	}
	b := Capabilities{
		CellID:    "mini-a",
		Inference: Inference{Tier: TierLarge, Models: []Model{{ID: "a", Version: "1"}, {ID: "a", Version: "2"}, {ID: "b"}}},
		Build:     []string{"flutter", "go"},
		Test:      []string{"integration", "unit"},
		Roles:     []Role{RoleAttempt, RoleBuild, RoleVerify},
	}
	ha, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("equivalent capabilities hash differently:\n %s\n %s", ha, hb)
	}
	a.Normalize()
	first, _ := Canonical(a)
	a.Normalize()
	second, _ := Canonical(a)
	if string(first) != string(second) {
		t.Fatal("Normalize is not idempotent")
	}
}

func TestCapabilitiesValidate(t *testing.T) {
	micro := Capabilities{
		CellID:    "micro-b",
		Inference: Inference{Tier: TierNone},
		Build:     []string{"go"},
		Test:      []string{"unit"},
		Roles:     []Role{RoleBuild, RoleVerify},
	}
	if err := micro.Validate(); err != nil {
		t.Fatalf("a model-less verify/build cell must be valid: %v", err)
	}

	// Attempting with no model is a configuration the cell can never honour.
	attemptless := micro
	attemptless.Roles = []Role{RoleAttempt}
	if err := attemptless.Validate(); err == nil {
		t.Fatal("tier=none with role=attempt was accepted")
	}

	// Tier claims a model; declaring none is an empty promise.
	modelless := micro
	modelless.Inference = Inference{Tier: TierLarge}
	if err := modelless.Validate(); err == nil {
		t.Fatal("tier=large with no models was accepted")
	}

	// A tier=none cell must not carry models: the two fields would disagree.
	contradictory := micro
	contradictory.Inference = Inference{Tier: TierNone, Models: []Model{{ID: "m"}}}
	if err := contradictory.Validate(); err == nil {
		t.Fatal("tier=none with a declared model was accepted")
	}

	for _, bad := range []string{"", "Mini-A", "-mini", "mini/a", "mini..a", strings.Repeat("a", 64)} {
		c := micro
		c.CellID = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid cell id %q was accepted", bad)
		}
	}
}

func TestCapabilitiesSupportsAndMissing(t *testing.T) {
	c := Capabilities{Build: []string{"go", "flutter"}, Test: []string{"unit", "large-memory"}}
	if !c.Supports([]string{"go"}, []string{"unit"}) {
		t.Fatal("declared capabilities not matched")
	}
	if c.Supports([]string{"rust"}, nil) {
		t.Fatal("undeclared build capability matched")
	}
	// Equality, not prefix: "go" must not satisfy "golang".
	if c.Supports([]string{"golang"}, nil) {
		t.Fatal("capability matching is not by equality")
	}
	got := c.Missing([]string{"rust", "go"}, []string{"fuzz"})
	want := "build:rust test:fuzz"
	if strings.Join(got, " ") != want {
		t.Fatalf("Missing = %v, want %s", got, want)
	}
}

func TestRefNamesRejectPathEscapes(t *testing.T) {
	if _, err := AttemptRef("mini/a", "t1", 1); err == nil {
		t.Fatal("a cell id with a slash produced a ref")
	}
	if _, err := AttemptRef("mini-a", "../../heads/main", 1); err == nil {
		t.Fatal("a task id with a path escape produced a ref")
	}
	if _, err := AttemptRef("mini-a", "t1", 0); err == nil {
		t.Fatal("attempt number 0 produced a ref")
	}
	if _, err := ClaimRef("mini-a", "t..1"); err == nil {
		t.Fatal("a task id containing .. produced a claim ref")
	}
}

func TestAttemptRefRoundTrip(t *testing.T) {
	ref, err := AttemptRef("mini-a", "sha256:abc", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "refs/attempts/mini-a/sha256:abc/3"; ref != want {
		t.Fatalf("ref = %s, want %s", ref, want)
	}
	cellID, taskID, n, err := ParseAttemptRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if cellID != "mini-a" || taskID != "sha256:abc" || n != 3 {
		t.Fatalf("round trip = %s/%s/%d", cellID, taskID, n)
	}
	if _, _, _, err := ParseAttemptRef("refs/heads/main"); err == nil {
		t.Fatal("a branch parsed as an attempt ref")
	}
}

func TestEvidencePassedTreatsSkipAndErrorAsNotPass(t *testing.T) {
	base := Evidence{Attempt: "c1", CellID: "micro-b"}
	for _, st := range []Status{StatusSkip, StatusError, StatusFail} {
		e := base
		e.Checks = []Check{{Name: "unit", Status: StatusPass}, {Name: "integration", Status: st}}
		if e.Passed() {
			t.Fatalf("evidence with a %q check reported green", st)
		}
		if len(e.Failures()) != 1 {
			t.Fatalf("Failures did not report the %q check", st)
		}
	}
	e := base
	e.Checks = []Check{{Name: "unit", Status: StatusPass}}
	if !e.Passed() {
		t.Fatal("all-pass evidence did not report green")
	}
	// No checks is no evidence, not weak evidence.
	if base.Passed() {
		t.Fatal("evidence with no checks reported green")
	}
	if err := base.Validate(); err == nil {
		t.Fatal("evidence with no checks validated")
	}
}

func TestEvidenceIndependent(t *testing.T) {
	e := Evidence{CellID: "micro-b"}
	if !e.Independent("mini-a") {
		t.Fatal("evidence from another cell was not independent")
	}
	if e.Independent("micro-b") {
		t.Fatal("self-produced evidence reported as independent")
	}
	if e.Independent("") {
		t.Fatal("an unknown attempting cell was treated as independent")
	}
}

func TestClaimRequiresExpiryAndGoesStale(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, err := NewClaim("mini-a", "t1", 1, 30*time.Minute, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stale(now) {
		t.Fatal("a fresh claim is stale")
	}
	if c.Stale(now.Add(29 * time.Minute)) {
		t.Fatal("a claim expired early")
	}
	if !c.Stale(now.Add(31 * time.Minute)) {
		t.Fatal("a claim past not_after is not stale")
	}
	if got := c.Remaining(now.Add(31 * time.Minute)); got != 0 {
		t.Fatalf("Remaining on a stale claim = %s, want 0", got)
	}
	if _, err := NewClaim("mini-a", "t1", 1, 0, now, false); err == nil {
		t.Fatal("a claim with no ttl was accepted; that is a lock")
	}
	if err := (Claim{CellID: "mini-a", Task: "t1"}).Validate(); err == nil {
		t.Fatal("a claim with no not_after validated; that is a lock")
	}
}

func TestArtifactRefNormalizeDedupesLocators(t *testing.T) {
	a := ArtifactRef{ContentHash: "sha256:x", Locators: []string{"b", "a", "b"}}
	b := ArtifactRef{ContentHash: "sha256:x", Locators: []string{"a", "b"}}
	a.Normalize()
	b.Normalize()
	ha, _ := CanonicalHash(a)
	hb, _ := CanonicalHash(b)
	if ha != hb {
		t.Fatal("an equal locator set did not encode identically")
	}
	if err := (ArtifactRef{}).Validate(); err == nil {
		t.Fatal("an artifact-ref with no content hash validated")
	}
}
