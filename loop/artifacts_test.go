package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/artifact"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

const artTicket = "a1c0ffee00000000000000000000000000000000000000000000000000000001"
const artChange = "b2dec0de00000000000000000000000000000000000000000000000000000002"

// artifactCell is the smallest cell that can record an artifact: a store, a
// varvig client, and a glob.
func artifactCell(t *testing.T) (*Cell, *varvigcli.Fake, *[]string) {
	t.Helper()
	v := varvigcli.NewFake("mini-a")
	v.AddTicket(artTicket, "Build the image.", varvigcli.Scope{Reads: []string{"src"}, Writes: []string{"src"}}, "approved")
	logs := &[]string{}
	c := &Cell{
		Capabilities:  cell.Capabilities{CellID: "mini-a"},
		V:             v,
		Artifacts:     &artifact.LocalCAS{Root: filepath.Join(t.TempDir(), "cas")},
		ArtifactGlobs: []string{"out/*.bin"},
		Log:           func(s string) { *logs = append(*logs, s) },
	}
	return c, v, logs
}

func buildOutput(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "app.bin"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRecordArtifactsUsesTheObjectForm is the point of preferring the native
// verb: a real artifact-ref object is what GC's mark phase can see, so it is what
// makes `varvig gc --report-external` able to ever report anything.
func TestRecordArtifactsUsesTheObjectForm(t *testing.T) {
	c, v, logs := artifactCell(t)
	dir := buildOutput(t, "pretend-image-bytes")

	refs, err := c.recordArtifacts(context.Background(), dir, artTicket, artChange)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("recorded %d artifacts, want 1", len(refs))
	}
	if !v.Called("AttachArtifact") {
		t.Fatalf("the native verb was not used; calls: %v", v.Calls)
	}
	// And emphatically not the note form, which would get no reachability.
	if v.Called("AddNote factory/artifact") {
		t.Fatalf("an artifact was written as a note despite the verb being available; calls: %v", v.Calls)
	}

	// The attempt stays answerable through produced_by, which is the only thing
	// distinguishing two attempts' outputs under one ticket.
	attached, err := v.TicketArtifacts(artTicket)
	if err != nil {
		t.Fatal(err)
	}
	if len(attached) != 1 {
		t.Fatalf("ticket holds %d artifacts, want 1", len(attached))
	}
	if attached[0].ProducedBy != artChange {
		t.Fatalf("produced_by = %q, want the attempt's change", attached[0].ProducedBy)
	}
	if attached[0].ContentHash != refs[0].ContentHash {
		t.Fatalf("content hash round trip: %q vs %q", attached[0].ContentHash, refs[0].ContentHash)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "attached artifact") {
		t.Fatalf("the attachment was not reported: %v", *logs)
	}
}

// TestRecordArtifactsFallsBackAudibly is the other half. Degrading is acceptable;
// degrading quietly is not, because the symptom is a GC report that stays empty
// rather than an error anyone sees.
func TestRecordArtifactsFallsBackAudibly(t *testing.T) {
	c, v, logs := artifactCell(t)
	v.UnsupportedVerbs = map[string]bool{"tickets attach-artifact": true}
	dir := buildOutput(t, "pretend-image-bytes")

	refs, err := c.recordArtifacts(context.Background(), dir, artTicket, artChange)
	if err != nil {
		t.Fatalf("an older core made artifact recording fail outright: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("recorded %d artifacts, want 1", len(refs))
	}
	if !v.Called("AddNote factory/artifact") {
		t.Fatalf("no fallback note was written; calls: %v", v.Calls)
	}
	log := strings.Join(*logs, "\n")
	// The warning has to name the consequence, not just the fact. "Falling back"
	// alone tells an operator nothing they can act on.
	for _, want := range []string{"gc --report-external", "upgrade varvig"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the degradation warning does not mention %q:\n%s", want, log)
		}
	}
}

func TestRecordArtifactsSurfacesARealAttachFailure(t *testing.T) {
	// An unsupported verb is a reason to degrade. Any other failure is not, and
	// swallowing it would lose the artifact silently.
	c, v, _ := artifactCell(t)
	c.Artifacts = &artifact.LocalCAS{Root: filepath.Join(t.TempDir(), "cas")}
	dir := buildOutput(t, "bytes")
	// A ticket the Fake does not know: AttachArtifact on an unknown ticket is a
	// plain error, not ErrUnsupported.
	v.UnsupportedVerbs = nil
	if _, err := c.recordArtifacts(context.Background(), dir, artTicket, artChange); err != nil {
		t.Fatalf("baseline attach failed: %v", err)
	}
	// Now force a genuine failure: an artifact whose hash cannot be converted.
	badRef := cell.ArtifactRef{ContentHash: "md5:abcd"}
	if err := c.attachArtifact(artTicket, artChange, badRef); err == nil {
		t.Fatal("an unconvertible content hash was accepted")
	}
}

func TestRecordArtifactsWithNoChangeStillRecords(t *testing.T) {
	// An attempt that produced no change can still have produced a build output
	// worth naming. The ticket is the anchor; produced_by is simply empty.
	c, v, _ := artifactCell(t)
	dir := buildOutput(t, "bytes-without-a-change")

	refs, err := c.recordArtifacts(context.Background(), dir, artTicket, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("recorded %d artifacts, want 1", len(refs))
	}
	if !v.Called("AttachArtifact") {
		t.Fatalf("nothing was attached; calls: %v", v.Calls)
	}
}

func TestRecordArtifactsIsANoOpWithoutGlobs(t *testing.T) {
	c, v, _ := artifactCell(t)
	c.ArtifactGlobs = nil
	refs, err := c.recordArtifacts(context.Background(), buildOutput(t, "x"), artTicket, artChange)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("recorded %d artifacts with no globs configured", len(refs))
	}
	if v.Called("AttachArtifact") {
		t.Fatal("a cell with no artifact globs attached something")
	}
}

func TestRecordArtifactsPutStaysLocal(t *testing.T) {
	// §8 again, from the loop's side: recording a ref must not move bytes. The
	// locator the store returns is a file:// URI, so what leaves the cell is
	// identity and a hint, never the artifact.
	c, _, _ := artifactCell(t)
	refs, err := c.recordArtifacts(context.Background(), buildOutput(t, "bytes"), artTicket, artChange)
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range refs[0].Locators {
		if !strings.HasPrefix(loc, "file://") {
			t.Fatalf("a speculative artifact was given a non-local locator: %s", loc)
		}
	}
}
