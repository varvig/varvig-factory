package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/varvigcli"
)

func TestApplyOutputWritesWholeFiles(t *testing.T) {
	dir := t.TempDir()
	text := "I will add two files.\n\n" +
		"--- src/a.go\npackage src\n\nfunc A() {}\n" +
		"--- src/sub/b.go\npackage sub\n"
	written, err := ApplyOutput(dir, text, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, " ") != "src/a.go src/sub/b.go" {
		t.Fatalf("written = %v", written)
	}
	got, err := os.ReadFile(filepath.Join(dir, "src", "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package src\n\nfunc A() {}\n" {
		t.Fatalf("content = %q", got)
	}
	// Prose before the first marker is explanation, not content.
	if strings.Contains(string(got), "I will add") {
		t.Fatal("prose leaked into the file")
	}
}

func TestApplyOutputPassesFencedContentThrough(t *testing.T) {
	// A patch or a diff quoted inside a file's content must not be read as a new
	// file header — otherwise a model writing documentation about diffs would
	// scatter files across the checkout.
	dir := t.TempDir()
	text := "--- docs/notes.md\n```\n--- not/a/file.go\nsome diff text\n```\nreal content\n"
	written, err := ApplyOutput(dir, text, []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0] != "docs/notes.md" {
		t.Fatalf("written = %v, want just docs/notes.md", written)
	}
	got, err := os.ReadFile(filepath.Join(dir, "docs", "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "--- not/a/file.go") {
		t.Fatalf("the fenced marker was not preserved as content: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "not", "a", "file.go")); err == nil {
		t.Fatal("a fenced marker created a file")
	}
}

func TestApplyOutputRefusesOutsideTheDeclaredWriteSet(t *testing.T) {
	// Factory does not decide the write set — that would be a second scheduler.
	// It declines to write outside the one varvig published on the ticket.
	dir := t.TempDir()
	cases := []struct {
		name, path string
	}{
		{"outside the write set", "other/x.go"},
		{"absolute", "/etc/passwd"},
		{"traversal", "../outside.go"},
		{"varvig metadata", ".varvig/config"},
		{"the trust store", ".varvig.d/allowed_keys"},
		// A raw prefix match would let "src" cover "srcgen".
		{"sibling by prefix", "srcgen/x.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyOutput(dir, "--- "+tc.path+"\ncontent\n", []string{"src"})
			if err == nil {
				t.Fatalf("writing %q was allowed", tc.path)
			}
		})
	}
}

func TestApplyOutputIsAllOrNothing(t *testing.T) {
	// A partially applied attempt is worse than a refused one: it commits half a
	// change and the tests then measure something nobody wrote on purpose.
	dir := t.TempDir()
	text := "--- src/ok.go\npackage src\n--- ../escape.go\nbad\n"
	if _, err := ApplyOutput(dir, text, []string{"src"}); err == nil {
		t.Fatal("a mixed-validity output was applied")
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "ok.go")); err == nil {
		t.Fatal("the valid half of a refused output was written")
	}
}

func TestApplyOutputRejectsADuplicatePath(t *testing.T) {
	// Two blocks naming the same path is a model contradicting itself. Taking the
	// last one would silently pick a winner.
	dir := t.TempDir()
	text := "--- src/a.go\nfirst\n--- src/a.go\nsecond\n"
	if _, err := ApplyOutput(dir, text, []string{"src"}); err == nil {
		t.Fatal("a duplicate path was accepted")
	}
}

func TestApplyOutputWithNoWriteSetRefuses(t *testing.T) {
	dir := t.TempDir()
	if _, err := ApplyOutput(dir, "--- src/a.go\nx\n", nil); err == nil {
		t.Fatal("a write was allowed against a ticket with no declared write set")
	}
}

func TestApplyOutputWithNoContentIsNotAnError(t *testing.T) {
	// "If no change is needed, return nothing" is in the prompt, and a model
	// taking that instruction must not look like a failure.
	dir := t.TempDir()
	written, err := ApplyOutput(dir, "There is nothing to change here.\n", []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Fatalf("written = %v, want none", written)
	}
}

func TestApplyOutputRejectsAMarkerWithNoPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := ApplyOutput(dir, "--- \ncontent\n", []string{"src"}); err == nil {
		t.Fatal("a marker with no path was accepted")
	}
}

func TestTaskScopeIsTheDeclaredReadSet(t *testing.T) {
	// One path: verbatim. Factory neither narrows nor widens what varvig
	// published.
	if got := taskScope(varvigcli.Scope{Reads: []string{"src/api"}}); got != "src/api" {
		t.Fatalf("scope = %q", got)
	}
	// No read set: the whole repository, which is what varvig's own default is.
	if got := taskScope(varvigcli.Scope{}); got != "/" {
		t.Fatalf("scope = %q, want /", got)
	}
	// Several paths: the shared prefix of the declared paths. Wider than any
	// single one, never wider than their common root — so it cannot admit a path
	// the ticket did not name a parent of.
	if got := taskScope(varvigcli.Scope{Reads: []string{"src/api/a", "src/api/b"}}); got != "src/api/" {
		t.Fatalf("scope = %q, want src/api/", got)
	}
	// Disjoint paths have no common root, so the scope is the repository — and
	// that is varvig's problem to enforce, not Factory's to narrow by guessing.
	if got := taskScope(varvigcli.Scope{Reads: []string{"src/a", "docs/b"}}); got != "/" {
		t.Fatalf("scope = %q, want /", got)
	}
}

func TestCommonPrefixMatchesWholeSegments(t *testing.T) {
	if got := commonPrefix([]string{"src/api", "src/apiv2"}); got != "src/" {
		t.Fatalf("commonPrefix = %q, want src/ (segments, not characters)", got)
	}
	if got := commonPrefix(nil); got != "/" {
		t.Fatalf("commonPrefix(nil) = %q", got)
	}
}
