package varvigcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
)

// The tests in this file drive the *real* `varvig` binary. They skip when one is
// not on PATH, so CI — which has no varvig — stays green, and a developer who has
// one gets the only check that can actually catch what matters here.
//
// What matters here is CLI format drift. Every other test in this package pins a
// format by asserting against a string this file's own author wrote down, which
// proves the parser matches the fixture and nothing about whether the fixture
// matches varvig. These tests close that loop: varvig writes the bytes, the
// adapter reads them back, and a change in either surfaces as a failure rather
// than as a cell that silently sees no artifacts.
func varvigRepo(t *testing.T) Exec {
	t.Helper()
	bin, err := exec.LookPath("varvig")
	if err != nil {
		t.Skip("no varvig binary on PATH; skipping the integration check " +
			"(build one from varvig/varvig and re-run to exercise the real CLI)")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	cmd := exec.Command(bin, "init", "repo")
	cmd.Dir = dir
	// A deterministic author keeps the note listings this test parses stable.
	cmd.Env = append(os.Environ(), "VARVIG_AUTHOR=integration")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("varvig init: %v: %s", err, out)
	}
	return Exec{Bin: bin, Dir: repo}
}

// newTicket mints a ticket and returns its full id.
func newTicket(t *testing.T, e Exec, spec string) string {
	t.Helper()
	out, err := e.run("tickets", "new", "-m", spec)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		t.Fatalf("could not read a ticket id from %q", out)
	}
	return fields[1]
}

func TestIntegrationAttachAndReadArtifact(t *testing.T) {
	e := varvigRepo(t)
	ticket := newTicket(t, e, "Build the app image.")

	// A content hash in Factory's own labelled form — the adapter must convert it
	// to the multihash the verb parses, or varvig rejects the argument outright.
	ref := cell.ArtifactRef{
		ContentHash: "sha256:46d4fece1941224acfda42351d2b496a5c902e9f9e1dd6fc81e5254907c40665",
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		Size:        19,
		Locators: []string{
			"oci://reg.internal/app@sha256:46d4fece",
			"oci://mirror/app@sha256:46d4fece",
		},
	}
	ref.Normalize()

	id, err := e.AttachArtifact(ticket, ref)
	if err != nil {
		t.Fatalf("AttachArtifact: %v", err)
	}
	if !cell.IsMultihash(id) {
		t.Fatalf("the returned artifact-ref id %q is not a multihash", id)
	}

	got, err := e.TicketArtifacts(ticket)
	if err != nil {
		t.Fatalf("TicketArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %d artifacts, want 1: %+v", len(got), got)
	}
	// Round trip through varvig's multihash and back to Factory's label.
	if got[0].ContentHash != ref.ContentHash {
		t.Fatalf("content hash round trip: got %q, want %q", got[0].ContentHash, ref.ContentHash)
	}
	if got[0].MediaType != ref.MediaType || got[0].Size != ref.Size {
		t.Fatalf("media type or size did not survive: %+v", got[0])
	}
	if strings.Join(got[0].Locators, " ") != strings.Join(ref.Locators, " ") {
		t.Fatalf("locators = %v, want %v", got[0].Locators, ref.Locators)
	}
}

func TestIntegrationProducedByLinksTheAttempt(t *testing.T) {
	// produced_by is the only thing that says which attempt built which output
	// when several attempts sit under one ticket, so it has to survive the round
	// trip. It is a varvig object id, passed through rather than converted.
	e := varvigRepo(t)
	ticket := newTicket(t, e, "Build twice.")
	change, err := e.ResolveRef("refs/varvig/tickets/" + ticket)
	if err != nil {
		t.Fatal(err)
	}

	ref := cell.ArtifactRef{
		ContentHash: "sha256:" + strings.Repeat("ab", 32),
		ProducedBy:  change,
	}
	if _, err := e.AttachArtifact(ticket, ref); err != nil {
		t.Fatalf("AttachArtifact: %v", err)
	}
	got, err := e.TicketArtifacts(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProducedBy != change {
		t.Fatalf("produced_by did not survive: %+v", got)
	}
}

// TestIntegrationArtifactIsGCReportable is the claim that justified this whole
// change. The object form must reach `gc --report-external`; the note form cannot.
func TestIntegrationArtifactIsGCReportable(t *testing.T) {
	e := varvigRepo(t)
	ticket := newTicket(t, e, "Build something collectable.")

	const digest = "46d4fece1941224acfda42351d2b496a5c902e9f9e1dd6fc81e5254907c40665"
	objectForm := cell.ArtifactRef{
		ContentHash: "sha256:" + digest,
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		Locators:    []string{"oci://reg.internal/app@sha256:" + digest},
	}
	if _, err := e.AttachArtifact(ticket, objectForm); err != nil {
		t.Fatalf("AttachArtifact: %v", err)
	}

	// The legacy note form, carrying the same field shape, for comparison.
	const noteDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	notePayload, err := cell.Canonical(cell.ArtifactRef{ContentHash: "sha256:" + noteDigest})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddNote(ticket, cell.NoteArtifact, notePayload); err != nil {
		t.Fatal(err)
	}

	// While reachable, neither is reported — that is correct, not a null result.
	reachable, err := e.GC(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reachable.ExternalUnreachable) != 0 {
		t.Fatalf("a reachable artifact was reported unreachable: %+v", reachable.ExternalUnreachable)
	}

	// Make everything unreachable: drop the note refs that pin the artifact-ref
	// and the ticket ref itself.
	refs, err := e.Refs()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/notes/") || strings.HasPrefix(r.Name, "refs/varvig/tickets/") {
			if err := e.DeleteRef(r.Name, r.Hash); err != nil {
				t.Fatalf("deleting %s: %v", r.Name, err)
			}
		}
	}
	// Reflogs are GC roots in their own right, so they must be expired in the
	// SAME invocation that reports: a separate pruning pass sweeps the
	// artifact-ref first, leaving the reporting pass with nothing to scan. That
	// ordering is also why Cell.relieveStorage uses the plain GC — it wants the
	// report without destroying undo history, and accepts that a just-unpinned
	// artifact is reported a while later.
	raw, err := e.run("gc", "--report-external", "--prune-reflog", "0s", "--keep", "0")
	if err != nil {
		t.Fatal(err)
	}
	report := parseGC(raw)

	// The object form is reported, with its locators, in the format parseGC reads.
	found := false
	for _, a := range report.ExternalUnreachable {
		if strings.HasSuffix(a.ContentHash, digest) {
			found = true
			if a.MediaType != objectForm.MediaType {
				t.Errorf("reported media type = %q", a.MediaType)
			}
			if len(a.Locators) == 0 {
				t.Error("reported no locators, so an operator cannot find the bytes to delete")
			}
		}
		if strings.Contains(a.ContentHash, noteDigest) {
			t.Error("a note-form artifact was reported; that would mean this test proves nothing")
		}
	}
	if !found {
		t.Fatalf("the object-form artifact was not reported by gc --report-external: %+v\nraw:\n%s",
			report.ExternalUnreachable, report.Raw)
	}
}

func TestIntegrationTicketReadsMatchThePorcelainParsers(t *testing.T) {
	// The porcelain parsers are the fragile part of this adapter. Exercise each
	// against real output once, so a format change fails here.
	e := varvigRepo(t)
	ticket := newTicket(t, e, "A ticket with a scope.")

	if _, err := e.run("tickets", "scope", ticket, "--reads", "src", "--writes", "src"); err != nil {
		t.Fatal(err)
	}

	ids, err := e.TicketIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != ticket {
		t.Fatalf("TicketIDs = %v, want [%s]", ids, ticket)
	}

	spec, err := e.Spec(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec, "A ticket with a scope.") {
		t.Fatalf("Spec = %q", spec)
	}

	scope, err := e.Scope(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Declared() || strings.Join(scope.Reads, ",") != "src" || strings.Join(scope.Writes, ",") != "src" {
		t.Fatalf("Scope = %+v", scope)
	}

	status, err := e.TicketStatus(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if status == "" {
		t.Fatal("TicketStatus returned nothing")
	}

	blockers, err := e.Blockers(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 0 {
		t.Fatalf("Blockers = %v, want none", blockers)
	}

	// Notes: the format whose safety rests on canonical JSON being newline-free.
	payload, err := cell.Canonical(map[string]any{"cell_id": "mini-a", "checks": []string{"unit"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddNote(ticket, cell.NoteEvidence, payload); err != nil {
		t.Fatal(err)
	}
	notes, err := e.Notes(ticket, cell.NoteEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("read %d notes, want 1", len(notes))
	}
	if string(notes[0].Payload) != string(payload) {
		t.Fatalf("note payload did not survive:\n got %s\nwant %s", notes[0].Payload, payload)
	}
}

func TestIntegrationRefCASAndDelete(t *testing.T) {
	e := varvigRepo(t)
	ticket := newTicket(t, e, "For ref plumbing.")
	hash, err := e.ResolveRef("refs/varvig/tickets/" + ticket)
	if err != nil {
		t.Fatal(err)
	}

	// A pin ref, in varvig's own shape, created only if absent.
	pin, err := cell.PinRef("mini-a", 4102444800, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateRef(pin, hash, ""); err != nil {
		t.Fatalf("creating a pin ref varvig should accept: %v", err)
	}
	// Creating it twice must be refused — the property attempt refs rely on.
	if err := e.UpdateRef(pin, hash, ""); err == nil {
		t.Fatal("a create-only ref update succeeded twice")
	}
	if err := e.DeleteRef(pin, hash); err != nil {
		t.Fatalf("deleting the pin: %v", err)
	}
	if _, err := e.ResolveRef(pin); err == nil {
		t.Fatal("the pin still resolves after deletion")
	}
}
