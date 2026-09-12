package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
)

// script writes an executable shell script and returns its path. A harness is
// any program that edits a checkout, so a shell script is a real one — and
// using one keeps these tests honest about the seam being a process boundary
// rather than a Go interface with a subprocess somewhere behind it.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHarnessEditsTheCheckoutAndReportsRatherThanReturningContent(t *testing.T) {
	// The defining difference from every other executor here: what comes back
	// is prose, and the change is on disk.
	h := &Harness{
		Path:        script(t, `echo "package src" > src/new.go; echo "wrote src/new.go"`),
		VersionArgs: []string{"--version"},
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	resp, err := h.Author(context.Background(), Request{Task: "t1", Intent: "add a file", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "wrote src/new.go") {
		t.Fatalf("the harness's report did not come back: %q", resp.Text)
	}
	body, err := os.ReadFile(filepath.Join(dir, "src/new.go"))
	if err != nil {
		t.Fatalf("the harness edited nothing in the checkout: %v", err)
	}
	if strings.TrimSpace(string(body)) != "package src" {
		t.Fatalf("file content = %q", body)
	}

	// And it says so in its properties, which is the only way a caller can
	// know. An executor that edited in place while declaring otherwise would
	// have its own files overwritten by its description of them.
	if p := h.Properties(); !p.EditsInPlace || !p.Loops || p.Deterministic {
		t.Fatalf("properties = %+v; want edits-in-place, looping, non-deterministic", p)
	}
}

func TestHarnessRefusesToWorkWithNoCheckout(t *testing.T) {
	// The mistake this prevents by name: falling back to the cell's own working
	// directory, where the edits would look exactly like a successful attempt
	// and would be in the wrong tree.
	h := &Harness{Path: script(t, `echo hi`), VersionArgs: []string{"--version"}}
	if _, err := h.Author(context.Background(), Request{Intent: "x"}); !errors.Is(err, ErrNoCheckout) {
		t.Fatalf("err = %v, want ErrNoCheckout", err)
	}
}

func TestHarnessPassesTheSocketOnlyWhenItAsksForOne(t *testing.T) {
	// Authority attaches to the tool channel (§4.2), so whether a harness gets
	// one is not incidental — a harness handed a socket it never declared would
	// be holding a credential nobody decided to give it.
	reporting := script(t, `echo "socket=[$VARVIG_MCP_SOCKET]"`)

	withTools := &Harness{Path: reporting, VersionArgs: []string{"--version"}, SocketEnv: "VARVIG_MCP_SOCKET"}
	if !withTools.Properties().ToolsAttached {
		t.Fatal("a harness configured with a socket variable does not declare ToolsAttached")
	}
	resp, err := withTools.Author(context.Background(),
		Request{Dir: t.TempDir(), Intent: "x", Socket: "/run/varvig/task-abc.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "socket=[/run/varvig/task-abc.sock]") {
		t.Fatalf("the socket did not reach the harness: %q", resp.Text)
	}

	// Configured without one, it declares no tools and is handed nothing even
	// when a socket exists.
	noTools := &Harness{Path: reporting, VersionArgs: []string{"--version"}}
	if noTools.Properties().ToolsAttached {
		t.Fatal("a harness with no socket variable declared ToolsAttached")
	}
	resp, err = noTools.Author(context.Background(),
		Request{Dir: t.TempDir(), Intent: "x", Socket: "/run/varvig/task-abc.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "socket=[]") {
		t.Fatalf("a harness that takes no tools was given a socket anyway: %q", resp.Text)
	}
}

func TestHarnessFailureStillReportsWhatItSaid(t *testing.T) {
	// A harness that exits nonzero may have edited files first, and those edits
	// are on disk whatever this returns. The error must not imply otherwise.
	dir := t.TempDir()
	h := &Harness{
		Path:        script(t, `echo "partial work" > half.go; echo "ran out of budget"; echo "boom" >&2; exit 3`),
		VersionArgs: []string{"--version"},
	}
	resp, err := h.Author(context.Background(), Request{Dir: dir, Intent: "x"})
	if err == nil {
		t.Fatal("a harness exiting 3 was reported as success")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the error does not carry the harness's stderr: %v", err)
	}
	if !strings.Contains(resp.Text, "ran out of budget") {
		t.Fatalf("the harness's own account was dropped on failure: %q", resp.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "half.go")); err != nil {
		t.Fatal("the fixture did not leave a partial edit, so this proves nothing about one")
	}
}

func TestHarnessMustDescribeItself(t *testing.T) {
	// An executor that cannot describe itself cannot take part in cross-cell
	// selection at all (§4), and a harness is not exempt.
	none := &Harness{Path: script(t, `echo hi`)}
	if _, err := none.Fragment(context.Background()); !errors.Is(err, cell.ErrIndescribable) {
		t.Fatalf("err = %v, want cell.ErrIndescribable", err)
	}
	broken := &Harness{Path: script(t, `exit 1`), VersionArgs: []string{"--version"}}
	if _, err := broken.Fragment(context.Background()); !errors.Is(err, cell.ErrIndescribable) {
		t.Fatalf("err = %v, want cell.ErrIndescribable", err)
	}

	// A version banner with a build date below it must not change the
	// environment hash every build, so only the first line is measured.
	h := &Harness{
		Path:        script(t, `echo "harness 2.1.0"; echo "built 2026-09-12"`),
		VersionArgs: []string{"--version"},
	}
	frag, err := h.Fragment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := frag.Toolchains["harness"]; got != "harness 2.1.0" {
		t.Fatalf("measured version = %q, want the first line alone", got)
	}
	// And it measures the binary, not the model: a harness reaching a hosted
	// model cannot prove what served a request, so an unconfigured model is
	// absent rather than guessed.
	if frag.Model != nil {
		t.Fatalf("a harness with no configured model reported one: %+v", frag.Model)
	}
}
