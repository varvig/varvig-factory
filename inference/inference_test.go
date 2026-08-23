package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
)

func TestParamsStringIsStable(t *testing.T) {
	// The params string lands in the environment hash, so its spelling is part
	// of the contract rather than a formatting detail.
	p := Params{Temperature: 0.2, TopP: 0.95, Seed: 7}
	if got, want := p.String(), "temp=0.2,top_p=0.95,seed=7"; got != want {
		t.Fatalf("params = %q, want %q", got, want)
	}
	if got := (Params{}).String(); got != "" {
		t.Fatalf("empty params = %q, want empty", got)
	}
	// Unset fields are omitted rather than rendered as zero, so "unset" and
	// "explicitly zero" do not collide in the hash.
	if got, want := (Params{Temperature: 0.7}).String(), "temp=0.7"; got != want {
		t.Fatalf("params = %q, want %q", got, want)
	}
}

func TestNoneDescribesItselfWithoutAModel(t *testing.T) {
	// A Micro cell's build and test evidence must not carry a model, or
	// deterministic evidence would look sampled (CELL.md §4.2).
	frag, err := None{}.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if frag.Model != nil {
		t.Fatalf("the model-less runtime reported a model: %+v", frag.Model)
	}
	if _, err := (None{}).Generate(context.Background(), Request{}); err == nil {
		t.Fatal("a cell with no model generated something")
	}
}

func TestHTTPFragmentProbesTheServerAndCachesIt(t *testing.T) {
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			probes++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"0.5.1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	rt := &HTTPRuntime{
		Endpoint:     srv.URL + "/v1/chat/completions",
		VersionURL:   srv.URL + "/api/version",
		Model:        "qwen2.5-coder",
		ModelVersion: "32b-q4",
		Params:       Params{Temperature: 0.2},
	}
	frag, err := rt.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := frag.Toolchains["inference-runtime"]; got != "0.5.1" {
		t.Fatalf("runtime version = %q, want 0.5.1 (measured from the server)", got)
	}
	if frag.Model == nil || frag.Model.ID != "qwen2.5-coder" || frag.Model.Version != "32b-q4" {
		t.Fatalf("model = %+v", frag.Model)
	}
	if frag.Model.Params != "temp=0.2" {
		t.Fatalf("params = %q", frag.Model.Params)
	}

	// Deterministic within a run, and one probe: the version must not vary
	// between two evidence records produced by the same cell in the same pass.
	again, err := rt.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatalf("probed %d times, want 1", probes)
	}
	ha, _ := cell.MergeFragments(frag)
	hb, _ := cell.MergeFragments(again)
	x, _ := ha.Hash()
	y, _ := hb.Hash()
	if x != y {
		t.Fatal("fragment is not stable across calls")
	}
}

func TestHTTPFragmentFallsBackToADigestOfWhateverTheServerReports(t *testing.T) {
	// Vendor-neutrality: an endpoint that reports its identity in some other
	// shape must still yield a stable, comparable token rather than an adapter
	// that refuses to work with it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"build":"unusual-shape","commit":"abc"}`))
	}))
	defer srv.Close()

	rt := &HTTPRuntime{VersionURL: srv.URL, Model: "m"}
	frag, err := rt.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := frag.Toolchains["inference-runtime"]
	if !strings.HasPrefix(got, cell.HashAlgorithm+":") {
		t.Fatalf("fallback token = %q, want a labelled digest", got)
	}
}

func TestHTTPWithoutAVersionURLIsIndescribable(t *testing.T) {
	// A version read from configuration is a claim about the server, not a
	// measurement of it (CELL.md §6).
	rt := &HTTPRuntime{Endpoint: "http://127.0.0.1:1/v1/chat/completions", Model: "m"}
	if _, err := rt.Fragment(context.Background()); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("err = %v, want ErrIndescribable", err)
	}
}

func TestHTTPUnreachableServerIsIndescribableNotSilent(t *testing.T) {
	rt := &HTTPRuntime{VersionURL: "http://127.0.0.1:1/version", Model: "m"}
	if _, err := rt.Fragment(context.Background()); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("err = %v, want ErrIndescribable", err)
	}
}

func TestHTTPGenerateSendsAndParsesTheChatShape(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"--- a.go\npackage a\n"}}],"usage":{"prompt_tokens":11,"completion_tokens":22}}`))
	}))
	defer srv.Close()

	rt := &HTTPRuntime{
		Endpoint:   srv.URL,
		Model:      "m",
		Params:     Params{Temperature: 0.2, Seed: 7},
		AuthHeader: "Authorization",
		AuthValue:  "Bearer secret",
		System:     "be brief",
	}
	resp, err := rt.Generate(context.Background(), Request{Intent: "add a file", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "package a") {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.TokensIn != 11 || resp.TokensOut != 22 {
		t.Fatalf("tokens = %d/%d, want 11/22", resp.TokensIn, resp.TokensOut)
	}
	if got.Model != "m" || len(got.Messages) != 2 {
		t.Fatalf("request = %+v", got)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Fatal("temperature was not sent, but is recorded in the environment")
	}
	if got.TopP != nil {
		t.Fatal("an unset top_p was sent as a value this adapter invented")
	}
	if got.Seed == nil || *got.Seed != 7 {
		t.Fatal("seed was not sent")
	}
}

func TestHTTPGenerateSurfacesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"out of capacity"}}`))
	}))
	defer srv.Close()
	rt := &HTTPRuntime{Endpoint: srv.URL, Model: "m"}
	_, err := rt.Generate(context.Background(), Request{})
	if err == nil {
		t.Fatal("a 429 was reported as success")
	}
	if !strings.Contains(err.Error(), "out of capacity") {
		t.Fatalf("error does not carry the server's reason: %v", err)
	}
}

func TestHTTPGenerateRejectsAnEmptyChoiceList(t *testing.T) {
	// A 200 with no choices is not an empty attempt, it is a broken server.
	// Treating it as "the model had nothing to say" would silently record a
	// no-op attempt and spend the budget for it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	rt := &HTTPRuntime{Endpoint: srv.URL, Model: "m"}
	if _, err := rt.Generate(context.Background(), Request{}); err == nil {
		t.Fatal("an empty choice list was accepted")
	}
}

func TestCommandRuntimeMeasuresItsVersionAndGenerates(t *testing.T) {
	rt := &CommandRuntime{
		Path:        "sh",
		Args:        []string{"-c", "cat >/dev/null; printf '%s' '--- a.txt\nhi\n'"},
		VersionArgs: []string{"-c", "printf '%s\n' 'shim 1.2.3'"},
		Model:       "local-gguf",
	}
	frag, err := rt.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := frag.Toolchains["inference-runtime"]; got != "shim 1.2.3" {
		t.Fatalf("version = %q, want 'shim 1.2.3'", got)
	}
	resp, err := rt.Generate(context.Background(), Request{Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "hi") {
		t.Fatalf("text = %q", resp.Text)
	}
	// A CLI reports no usage, and the adapter must not invent one: a fabricated
	// token count would go straight into the budget ledger as if it were
	// measured (FACTORY.md §7).
	if resp.TokensIn != 0 || resp.TokensOut != 0 {
		t.Fatalf("command runtime invented token counts: %d/%d", resp.TokensIn, resp.TokensOut)
	}
}

func TestCommandRuntimeWithoutVersionArgsIsIndescribable(t *testing.T) {
	rt := &CommandRuntime{Path: "sh", Model: "m"}
	if _, err := rt.Fragment(context.Background()); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("err = %v, want ErrIndescribable", err)
	}
}

func TestCommandRuntimeAcceptsAVersionOnStderr(t *testing.T) {
	// Plenty of tools print --version to stderr; calling the adapter
	// indescribable over a stream choice would be pedantry with a cost.
	rt := &CommandRuntime{
		Path:        "sh",
		VersionArgs: []string{"-c", "printf '%s\n' 'v4 on stderr' 1>&2"},
		Model:       "m",
	}
	frag, err := rt.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := frag.Toolchains["inference-runtime"]; got != "v4 on stderr" {
		t.Fatalf("version = %q", got)
	}
}

func TestPromptIsSharedAcrossAdapters(t *testing.T) {
	// Two runtimes given the same request must see the same prompt: otherwise a
	// cross-cell comparison of two attempts compares prompts as much as models.
	r := Request{Intent: "do the thing", Context: []ContextFile{{Path: "a.go", Content: "package a"}}}
	p := Prompt(r)
	if !strings.Contains(p, "do the thing") || !strings.Contains(p, "a.go") || !strings.Contains(p, "package a") {
		t.Fatalf("prompt is missing the request: %s", p)
	}
	if p != Prompt(r) {
		t.Fatal("Prompt is not deterministic")
	}
}

func TestFakeVariesRepliesByAttempt(t *testing.T) {
	// Duplicate attempts are the point (FACTORY.md §5.1), so making two
	// attempts differ has to be possible without the loop dictating how.
	f := &Fake{Replies: []string{"first", "second"}}
	a, _ := f.Generate(context.Background(), Request{Attempt: 1})
	b, _ := f.Generate(context.Background(), Request{Attempt: 2})
	if a.Text != "first" || b.Text != "second" {
		t.Fatalf("replies = %q, %q", a.Text, b.Text)
	}
	// Out of range clamps rather than panicking: attempts_default may exceed
	// the scripted list, and a panic in a test fake is a wasted afternoon.
	c, _ := f.Generate(context.Background(), Request{Attempt: 9})
	if c.Text != "second" {
		t.Fatalf("clamped reply = %q", c.Text)
	}
	if f.Calls != 3 {
		t.Fatalf("Calls = %d, want 3", f.Calls)
	}
}

var (
	_ Runtime = None{}
	_ Runtime = (*HTTPRuntime)(nil)
	_ Runtime = (*CommandRuntime)(nil)
	_ Runtime = (*Fake)(nil)
)
