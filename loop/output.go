package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/varvig/varvig-factory/varvigcli"
)

// FileMarker is the line prefix that introduces a file in a model's output. It
// matches the instruction in inference.Prompt, and the two must stay in step —
// which is why the marker is a named constant in the package that consumes it
// rather than a literal in both.
const FileMarker = "--- "

// ApplyOutput writes a model's output into a checkout and returns the paths it
// wrote, sorted.
//
// The output format is deliberately the dumbest thing that works: a line
// `--- path/to/file` followed by that file's complete new content. Not a unified
// diff — a diff has to apply cleanly against an exact base, and a model that
// miscounts a hunk header produces a failure that looks like a merge conflict
// rather than like a bad attempt. Whole-file output either parses or does not.
//
// # Scope
//
// A path outside the ticket's declared write set is refused, and the whole
// attempt fails rather than being partially applied.
//
// This is not Factory deciding a write set — that would be a second scheduler
// (CELL.md §10.1). The write set is varvig's, published on the ticket; this
// function only declines to write outside it. varvig remains the authority and
// enforces the same boundary at the gate; checking here turns "the commit was
// rejected for reasons involving a path you cannot see" into "the model tried to
// write outside its scope", which is the error an operator can act on.
func ApplyOutput(dir, text string, writes []string) ([]string, error) {
	blocks, err := parseBlocks(text)
	if err != nil {
		return nil, err
	}
	// Validate every path before writing any: a partially applied attempt is
	// worse than a refused one, because it commits half a change and the tests
	// then measure something nobody wrote on purpose.
	for _, b := range blocks {
		if err := checkPath(b.path, writes); err != nil {
			return nil, err
		}
	}
	var written []string
	for _, b := range blocks {
		full := filepath.Join(dir, b.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(full, []byte(b.content), 0o644); err != nil {
			return written, err
		}
		written = append(written, b.path)
	}
	sort.Strings(written)
	return written, nil
}

type block struct {
	path    string
	content string
}

func parseBlocks(text string) ([]block, error) {
	var blocks []block
	var current *block
	var buf strings.Builder
	inFence := false

	flush := func() {
		if current == nil {
			return
		}
		current.content = buf.String()
		blocks = append(blocks, *current)
		current = nil
		buf.Reset()
	}

	for _, line := range strings.Split(text, "\n") {
		// A fenced code block is passed through verbatim, including any line
		// inside it that happens to start with the marker: a patch or a diff
		// quoted inside a file's content must not be read as a new file header.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if current == nil {
				// Fences outside a file block are prose decoration around the
				// answer; drop them rather than treating them as content.
				inFence = !inFence
				continue
			}
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(line, FileMarker) {
			flush()
			path := strings.TrimSpace(strings.TrimPrefix(line, FileMarker))
			if path == "" {
				return nil, fmt.Errorf("loop: output has a file marker with no path")
			}
			current = &block{path: path}
			continue
		}
		if current == nil {
			// Prose before the first marker. Models explain themselves; that is
			// not an error and it is not content.
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	// Two blocks naming the same path is a model contradicting itself. Taking
	// the last one would silently pick a winner.
	seen := map[string]bool{}
	for _, b := range blocks {
		if seen[b.path] {
			return nil, fmt.Errorf("loop: output names %s twice", b.path)
		}
		seen[b.path] = true
	}
	return blocks, nil
}

// checkPath refuses anything outside the declared write set, and anything that
// is not a plain relative path.
func checkPath(path string, writes []string) error {
	if path == "" {
		return fmt.Errorf("loop: empty output path")
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return fmt.Errorf("loop: refusing absolute output path %q", path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("loop: refusing output path %q; it escapes the checkout", path)
	}
	// The untracked metadata directory and the trust store are the two paths a
	// model must never be able to write, whatever the write set says. The trust
	// store because writing it is privilege escalation (AUTH.md §2: you cannot
	// authorize yourself); the metadata directory because it is not source.
	if clean == ".varvig" || strings.HasPrefix(clean, ".varvig/") {
		return fmt.Errorf("loop: refusing to write %q; that is varvig's own metadata", path)
	}
	if clean == ".varvig.d/allowed_keys" {
		return fmt.Errorf("loop: refusing to write %q; a cell cannot author its own authorization", path)
	}
	if len(writes) == 0 {
		return fmt.Errorf("loop: refusing to write %q; the ticket declares no write set", path)
	}
	for _, w := range writes {
		if varvigcli.ScopeCovers(w, clean) || strings.TrimSuffix(w, "/") == clean {
			return nil
		}
	}
	return fmt.Errorf("loop: refusing to write %q; it is outside the ticket's declared write set [%s]",
		path, strings.Join(writes, " "))
}
