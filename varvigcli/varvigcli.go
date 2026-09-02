// Package varvigcli is Factory's adapter over the `varvig` binary's public CLI.
//
// Factory never imports varvig's Go internals — they are unexportable across
// modules by design, and more importantly the binding between the two is meant
// to be the CLI and the wire protocol, not a shared type. Factory is a peer
// (FACTORY.md §1.1), and a peer that linked against the core's internals would
// be part of the core.
//
// The Varvig interface is what the rest of Factory depends on, so the loop is
// testable against Fake with no repository, no daemon and no disk, and the real
// Exec client is one implementation of the same surface.
//
// # Plumbing over porcelain
//
// Where varvig has a JSON plumbing command, this adapter uses it: `varvig read
// refs`, `varvig read proposals`, `varvig read blob`. Where it does not, the
// adapter parses porcelain, and each such method carries a comment naming the
// exact format it depends on so a change in varvig's human output surfaces here
// as a failing test rather than as a cell that silently sees no tickets.
//
// One design decision makes the porcelain parsing safe rather than merely
// tolerable: every payload Factory writes is canonical JSON, which by
// construction contains no newline (cell.Canonical). `varvig note list` prints a
// header line followed by the payload indented by two spaces, so a newline-free
// payload is recoverable exactly. A pretty-printed payload would not be.
package varvigcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

// ErrNoRef is returned when a ref does not exist. It is a named error because
// "does not exist yet" is the normal case for a claim or an attempt, and a cell
// that treated it as a failure would never make its first one.
var ErrNoRef = errors.New("varvigcli: ref does not exist")

// ErrCAS is returned when a compare-and-swap was refused because the ref moved.
// It is not a failure of the cell: it is varvig serializing concurrent work, and
// the correct response is to re-read and decide again (FACTORY.md §1).
var ErrCAS = errors.New("varvigcli: compare-and-swap refused")

// ErrUnreachable is returned when an upstream peer could not be reached. It is
// distinct from every other error because a disconnected cell must keep working
// (FACTORY.md §5.2) — collapsing it into a generic failure is how local-first
// operation quietly stops being real.
var ErrUnreachable = errors.New("varvigcli: upstream unreachable")

// ErrUnsupported is returned when this varvig build does not have the verb
// Factory asked for.
//
// It exists so that "your core is older than this cell" is a distinguishable
// answer rather than a generic failure. A caller can then degrade deliberately
// and say so — which matters because the degradation is usually invisible in the
// output and visible only much later, in what some other tool fails to report.
var ErrUnsupported = errors.New("varvigcli: this varvig build does not support that command")

// Ref is one entry of `varvig read refs`.
type Ref struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// Note is one note attached to an object.
type Note struct {
	Namespace string
	Author    string
	Payload   []byte
}

// Proposal is one speculation candidate, from `varvig read proposals`. The
// pool is varvig's, not Factory's: Factory adds candidates and reads scores, and
// varvig decides what may be promoted onto what.
type Proposal struct {
	Task    string  `json:"task"`
	Change  string  `json:"change"`
	Score   float64 `json:"score"`
	Scored  bool    `json:"scored"`
	Created int64   `json:"created"`
}

// Scope is a ticket's declared read and write set. Factory *reads* it and never
// computes it: deciding a write set is scheduling, and that belongs to varvig
// (CELL.md §10.1).
type Scope struct {
	Reads  []string
	Writes []string
}

// Declared reports whether the ticket has a scope at all. A ticket with none is
// unschedulable by construction (TICKETS.md §3.1), so a cell must not attempt
// it — not because Factory decided so, but because varvig will not serialize it.
func (s Scope) Declared() bool { return len(s.Reads) > 0 || len(s.Writes) > 0 }

// TaskRequest asks varvig for a scoped, propose-only task credential and a
// sparse checkout of its read set.
//
// This is the whole of "submit work to varvig" (FACTORY.md §5 step 4). Note what
// is absent: no priority, no ordering, no affected set. Factory names the scope
// the ticket already declared and lets varvig serialize.
type TaskRequest struct {
	Scope string
	TTL   time.Duration
	Base  string
	Dir   string
}

// Task is a live task credential.
type Task struct {
	ID  string
	Dir string
}

// HookResult is one wasm module run, from `varvig hook run`. ExitCode is the
// module's verdict.
type HookResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// TrustEntry is one line of the repository trust store (AUTH.md §2). Factory
// reads it to answer a question it must not answer from its own configuration:
// whether this cell's key actually holds `promote` at a path (FACTORY.md §6.1).
type TrustEntry struct {
	Fingerprint string
	Name        string
	Scope       string
	Rights      []string
}

// Can reports whether this entry grants right at path.
func (e TrustEntry) Can(right, path string) bool {
	if !hasRight(e.Rights, right) {
		return false
	}
	return scopeCovers(e.Scope, path)
}

// GCReport is the outcome of a sweep. ExternalUnreachable is what
// `--report-external` printed: artifact hashes that went unreachable this pass.
// varvig reports; deletion is Factory's or the operator's action, because varvig
// holds no registry credentials and must never acquire them (§8).
type GCReport struct {
	ExternalUnreachable []ExternalArtifact
	Raw                 string
}

// ExternalArtifact is one unreachable external artifact.
type ExternalArtifact struct {
	ContentHash string
	MediaType   string
	Locators    []string
}

// Varvig is the subset of the varvig CLI that Factory drives.
type Varvig interface {
	// Version is the varvig build in use, for the cell's own logs.
	Version() (string, error)

	// TicketIDs lists every ticket's full id.
	TicketIDs() ([]string, error)
	// Spec is a ticket's intent text, verbatim.
	Spec(ticket string) (string, error)
	// TicketStatus is the derived governance status word.
	TicketStatus(ticket string) (string, error)
	// Scope is the ticket's declared read/write set.
	Scope(ticket string) (Scope, error)
	// Blockers are the tickets blocking this one, derived by varvig.
	Blockers(ticket string) ([]string, error)

	// Refs lists every ref and the hash it resolves to.
	Refs() ([]Ref, error)
	// ResolveRef resolves one ref, or ErrNoRef.
	ResolveRef(name string) (string, error)
	// UpdateRef sets a ref by compare-and-swap. An empty oldValue asserts the
	// ref does not yet exist. A refused swap is ErrCAS.
	UpdateRef(name, newValue, oldValue string) error
	// DeleteRef removes a ref by compare-and-swap on its current value. It is
	// how a pin is released ahead of its expiry (FACTORY.md §7): the expiry
	// alone would eventually reclaim it, but storage pressure is now.
	DeleteRef(name, oldValue string) error

	// PutBlob stores bytes and returns the object id.
	PutBlob(content []byte) (string, error)
	// ReadBlob reads a blob by object id or by a ref naming one.
	ReadBlob(idOrRef string) ([]byte, error)

	// AttachArtifact records an external artifact against a ticket, returning the
	// artifact-ref object's id.
	//
	// This is the *object* form: varvig stores a real TypeArtifactRef and pins it
	// for reachability, so the artifact appears in `varvig gc --report-external`
	// when it goes unreachable (FEDERATION.md §1). A JSON note carrying the same
	// fields does not — which is the whole reason to prefer this.
	//
	// ContentHash must be in Factory's labelled form; the adapter converts it to
	// the multihash varvig expects. Returns ErrUnsupported on a core without the
	// verb.
	AttachArtifact(ticket string, ref cell.ArtifactRef) (string, error)
	// TicketArtifacts lists the artifact-refs attached to a ticket.
	TicketArtifacts(ticket string) ([]cell.ArtifactRef, error)

	// AddNote attaches a note to an object in a namespace.
	AddNote(target, namespace string, payload []byte) error
	// Notes lists the notes attached to an object in a namespace.
	Notes(target, namespace string) ([]Note, error)

	// TaskStart mints a scoped, propose-only credential and a sparse checkout.
	TaskStart(req TaskRequest) (Task, error)
	// TaskStop revokes a credential early.
	TaskStop(id string) error
	// Commit commits a working directory, returning the change hash.
	Commit(dir, message string) (string, error)

	// SpecAdd records a speculation candidate for a task.
	SpecAdd(task, change string) error
	// Proposals lists a task's candidates and their scores.
	Proposals(task string) ([]Proposal, error)
	// SpecScore sets a candidate's score.
	SpecScore(task, change string, score float64) error
	// SpecPromote promotes the best candidate onto a ref, returning the change
	// promoted. varvig applies its own promotion checkpoint here — the veto gate
	// and the repository policy module — so a Factory gate is an additional
	// constraint, never a replacement for varvig's.
	SpecPromote(task, ref string) (string, error)
	// SpecPrune keeps the top K candidates.
	SpecPrune(task string, keep int) error

	// Fetch and Push exchange state with an upstream peer. Both return
	// ErrUnreachable when the peer cannot be reached, so a cell can tell "no
	// upstream" from "upstream refused".
	Fetch(addr, branch string) error
	Push(addr, branch string) error

	// HookSet binds a wasm module to an event, returning the module's object id.
	HookSet(event, modulePath string) (string, error)
	// HookRun runs the modules bound to an event with input on stdin.
	HookRun(ctx context.Context, event string, input []byte) ([]HookResult, error)

	// TrustList reads the repository trust store.
	TrustList() ([]TrustEntry, error)

	// GC sweeps unreachable objects and reports unreachable external artifacts.
	GC(reportExternal bool) (GCReport, error)
}

// Exec runs the real `varvig` binary in Dir.
type Exec struct {
	// Bin defaults to "varvig" on PATH.
	Bin string
	// Dir is the repository. Every command runs there.
	Dir string
}

func (e Exec) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "varvig"
}

func (e Exec) run(args ...string) (string, error) { return e.runIn(e.Dir, nil, args...) }

func (e Exec) runIn(dir string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command(e.bin(), args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		return out.String(), classify(fmt.Errorf("varvig %s: %w: %s", strings.Join(args, " "), err, msg), msg)
	}
	return out.String(), nil
}

// classify maps varvig's error text onto the named errors that Factory branches
// on. It is string matching, which is fragile, and the fragility is confined to
// this one function on purpose: the three distinctions Factory genuinely needs —
// absent, refused-swap, unreachable — are worth a test each rather than a
// generic error everywhere.
func classify(err error, msg string) error {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "does not exist"), strings.Contains(l, "no such ref"),
		strings.Contains(l, "not exist"), strings.Contains(l, "unknown ref"):
		return fmt.Errorf("%w: %v", ErrNoRef, err)
	case strings.Contains(l, "compare-and-swap"), strings.Contains(l, "cas"),
		strings.Contains(l, "stale"), strings.Contains(l, "changed under"):
		return fmt.Errorf("%w: %v", ErrCAS, err)
	case strings.Contains(l, "connection refused"), strings.Contains(l, "no route to host"),
		strings.Contains(l, "i/o timeout"), strings.Contains(l, "dial tcp"),
		strings.Contains(l, "no such host"), strings.Contains(l, "network is unreachable"):
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	case strings.Contains(l, "unknown argument"), strings.Contains(l, "unknown subcommand"),
		strings.Contains(l, "unknown command"), strings.Contains(l, "unknown tickets subcommand"):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return err
}

// Version implements Varvig.
func (e Exec) Version() (string, error) {
	out, err := e.run("version")
	return strings.TrimSpace(out), err
}

// TicketIDs implements Varvig by listing the ticket ref namespace rather than
// parsing `varvig tickets list`.
//
// The porcelain listing prints a *truncated* id and a spec that may span lines;
// the ref namespace gives the full id, one per line, from JSON. Reaching for the
// plumbing here is not fastidiousness — a cell keyed on a 12-character prefix
// would collide eventually, and the collision would look like a cell attempting
// the wrong ticket.
func (e Exec) TicketIDs() ([]string, error) {
	refs, err := e.Refs()
	if err != nil {
		return nil, err
	}
	const prefix = "refs/varvig/tickets/"
	var out []string
	for _, r := range refs {
		rest, ok := strings.CutPrefix(r.Name, prefix)
		if !ok || rest == "" {
			continue
		}
		// refs/varvig/tickets/<id>/spec is the optional separated-spec ref, not
		// a ticket of its own.
		if strings.Contains(rest, "/") {
			continue
		}
		out = append(out, rest)
	}
	return out, nil
}

// Spec implements Varvig. `varvig tickets spec` exists precisely so a tool gets
// the raw spec rather than the truncated human rendering.
func (e Exec) Spec(ticket string) (string, error) {
	return e.run("tickets", "spec", ticket)
}

// TicketStatus implements Varvig. Format: one word on stdout.
func (e Exec) TicketStatus(ticket string) (string, error) {
	out, err := e.run("tickets", "status", ticket)
	return strings.TrimSpace(out), err
}

// Scope implements Varvig. Format of `varvig tickets scope <id>`:
//
//	reads:  a b c
//	writes: d e
//
// or the single line "(no scope declared)".
func (e Exec) Scope(ticket string) (Scope, error) {
	out, err := e.run("tickets", "scope", ticket)
	if err != nil {
		return Scope{}, err
	}
	var s Scope
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "reads:"):
			s.Reads = strings.Fields(strings.TrimPrefix(line, "reads:"))
		case strings.HasPrefix(line, "writes:"):
			s.Writes = strings.Fields(strings.TrimPrefix(line, "writes:"))
		}
	}
	return s, nil
}

// Blockers implements Varvig. Format: one full hash per line, or "(no blockers)".
func (e Exec) Blockers(ticket string) ([]string, error) {
	out, err := e.run("tickets", "blockers", ticket)
	if err != nil {
		return nil, err
	}
	var out2 []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "(") {
			continue
		}
		out2 = append(out2, line)
	}
	return out2, nil
}

// Refs implements Varvig via the JSON plumbing.
func (e Exec) Refs() ([]Ref, error) {
	out, err := e.run("read", "refs")
	if err != nil {
		return nil, err
	}
	var refs []Ref
	if err := json.Unmarshal([]byte(out), &refs); err != nil {
		return nil, fmt.Errorf("varvigcli: parsing `read refs`: %w", err)
	}
	return refs, nil
}

// ResolveRef implements Varvig. Format of `varvig show-ref <name>`:
// "<hash> <name>".
func (e Exec) ResolveRef(name string) (string, error) {
	out, err := e.run("show-ref", name)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("%w: %s", ErrNoRef, name)
	}
	return fields[0], nil
}

// UpdateRef implements Varvig. `varvig update-ref <name> <new> <old>` is silent
// on success.
//
// The old value is ALWAYS passed, and an empty oldValue is sent as the explicit
// zero — varvig's spelling of "expected absent". Omitting the argument instead
// means something quite different: varvig resolves the ref's current value and
// uses *that* as the expectation, which is an unconditional set. Sending nothing
// would therefore turn every create-only write into an overwrite, and the write
// that matters most is the attempt ref, which the contract requires to be created
// once and never moved (CELL.md §2). That immutability is what makes two
// partitioned cells' attempts both survive a reconnect; an unconditional set
// would silently discard one of them.
func (e Exec) UpdateRef(name, newValue, oldValue string) error {
	if oldValue == "" {
		oldValue = zeroValue
	}
	_, err := e.run("update-ref", name, newValue, oldValue)
	return err
}

// zeroValue is how varvig spells "no value" in a ref argument: absent, for an
// expected-old, and deletion, for a new value.
const zeroValue = "0"

// DeleteRef implements Varvig. varvig spells a deletion as an update to the
// zero value — `varvig update-ref <name> 0 <old>` — so the removal goes through
// the same compare-and-swap as any other ref move and lands in the same reflog.
func (e Exec) DeleteRef(name, oldValue string) error {
	args := []string{"update-ref", name, zeroValue}
	if oldValue != "" {
		// Unlike UpdateRef, an empty oldValue here is deliberately left off: a
		// caller deleting a ref it did not just read wants the deletion to
		// succeed, not to guess an expected value. Passing the zero would assert
		// the ref is already absent and fail on every real deletion.
		args = append(args, oldValue)
	}
	_, err := e.run(args...)
	return err
}

// PutBlob implements Varvig via `varvig hash-object -w -`.
func (e Exec) PutBlob(content []byte) (string, error) {
	out, err := e.runIn(e.Dir, content, "hash-object", "-w", "-")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ReadBlob implements Varvig. `varvig read blob` writes raw bytes and resolves a
// ref argument, so a claim or a capabilities record is one call from its ref.
func (e Exec) ReadBlob(idOrRef string) ([]byte, error) {
	out, err := e.run("read", "blob", idOrRef)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// AttachArtifact implements Varvig via `varvig tickets attach-artifact`.
//
// Format: "attached artifact <object id> to <short ticket>". The content hash and
// producing change go over as multihashes, which is what the verb parses; the
// conversion is lossless because Factory's digest is already SHA-256 and
// SHA2-256 is a registered multihash code (cell.ToMultihash).
func (e Exec) AttachArtifact(ticket string, ref cell.ArtifactRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	contentHash, err := cell.ToMultihash(ref.ContentHash)
	if err != nil {
		return "", err
	}
	args := []string{"tickets", "attach-artifact", ticket, "--content-hash", contentHash}
	if ref.MediaType != "" {
		args = append(args, "--media-type", ref.MediaType)
	}
	if ref.Size > 0 {
		args = append(args, "--size", strconv.FormatInt(ref.Size, 10))
	}
	// Locators are already sorted and deduplicated by Normalize, and the verb
	// preserves them, so an equal locator set produces an equal record.
	for _, loc := range ref.Locators {
		args = append(args, "--locator", loc)
	}
	if ref.ProducedBy != "" {
		// The producing change is a varvig object id, already multihash hex —
		// passed through rather than converted, since converting a hash that is
		// not Factory-labelled would be a category error.
		args = append(args, "--produced-by", ref.ProducedBy)
	}
	out, err := e.run(args...)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(firstLine(out))
	if len(fields) < 3 {
		return "", fmt.Errorf("varvigcli: could not read an artifact id from %q", firstLine(out))
	}
	return fields[2], nil
}

// TicketArtifacts implements Varvig via `varvig tickets artifacts`, which prints
// JSON lines — one compact object per artifact-ref, with exactly the field names
// cell.ArtifactRef uses.
func (e Exec) TicketArtifacts(ticket string) ([]cell.ArtifactRef, error) {
	out, err := e.run("tickets", "artifacts", ticket)
	if err != nil {
		return nil, err
	}
	return parseArtifactLines(out)
}

// parseArtifactLines decodes the JSON-lines output, converting each content hash
// back into Factory's labelled form.
func parseArtifactLines(out string) ([]cell.ArtifactRef, error) {
	var refs []cell.ArtifactRef
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var wire struct {
			ContentHash string   `json:"content_hash"`
			MediaType   string   `json:"media_type"`
			Size        int64    `json:"size"`
			Locators    []string `json:"locators"`
			ProducedBy  string   `json:"produced_by"`
		}
		if err := json.Unmarshal([]byte(line), &wire); err != nil {
			return nil, fmt.Errorf("varvigcli: parsing `tickets artifacts` line %q: %w", line, err)
		}
		labelled, err := cell.FromMultihash(wire.ContentHash)
		if err != nil {
			// A content hash in an algorithm this build cannot name is not
			// something to pass along unlabelled: it would look like a Factory
			// hash and compare as equal to nothing.
			return nil, fmt.Errorf("varvigcli: artifact content hash: %w", err)
		}
		ref := cell.ArtifactRef{
			ContentHash: labelled,
			MediaType:   wire.MediaType,
			Size:        wire.Size,
			Locators:    wire.Locators,
			ProducedBy:  wire.ProducedBy,
		}
		ref.Normalize()
		refs = append(refs, ref)
	}
	return refs, nil
}

// AddNote implements Varvig. The payload goes through a temporary file rather
// than -m: a note payload is JSON and putting it in argv would make it subject
// to the platform's argument-length limit for no benefit.
func (e Exec) AddNote(target, namespace string, payload []byte) error {
	f, err := os.CreateTemp("", "varvig-factory-note-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(payload); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = e.run("note", "add", target, "--ns", namespace, "-f", f.Name())
	return err
}

// Notes implements Varvig. Format of `varvig note list <target> --ns <ns>`:
//
//	[ns] <shortid> by <author> at <rfc3339>
//	  <payload>
//
// The payload is indented by two spaces on the line after each header. Factory's
// payloads are canonical JSON and therefore newline-free (see the package
// comment), so this recovers them exactly.
func (e Exec) Notes(target, namespace string) ([]Note, error) {
	out, err := e.run("note", "list", target, "--ns", namespace)
	if err != nil {
		if errors.Is(err, ErrNoRef) {
			return nil, nil
		}
		return nil, err
	}
	return parseNotes(out), nil
}

func parseNotes(out string) []Note {
	var notes []Note
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "[") {
			continue
		}
		end := strings.Index(line, "]")
		if end < 0 {
			continue
		}
		n := Note{Namespace: line[1:end]}
		if by := strings.Index(line, " by "); by >= 0 {
			rest := line[by+4:]
			if at := strings.Index(rest, " at "); at >= 0 {
				n.Author = rest[:at]
			} else {
				n.Author = rest
			}
		}
		if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") {
			n.Payload = []byte(strings.TrimPrefix(lines[i+1], "  "))
			i++
		}
		notes = append(notes, n)
	}
	return notes
}

// TaskStart implements Varvig. Format: the first line is "task <id>".
func (e Exec) TaskStart(req TaskRequest) (Task, error) {
	args := []string{"task", "start"}
	if req.Scope != "" {
		args = append(args, "--scope", req.Scope)
	}
	if req.TTL > 0 {
		args = append(args, "--ttl", req.TTL.String())
	}
	if req.Base != "" {
		args = append(args, "--base", req.Base)
	}
	if req.Dir != "" {
		args = append(args, req.Dir)
	}
	out, err := e.run(args...)
	if err != nil {
		return Task{}, err
	}
	first := firstLine(out)
	id := strings.TrimSpace(strings.TrimPrefix(first, "task"))
	if id == "" {
		return Task{}, fmt.Errorf("varvigcli: could not read a task id from %q", first)
	}
	dir := req.Dir
	if dir == "" {
		// varvig's own default when no directory is named.
		dir = "./task-" + id
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(e.Dir, dir)
	}
	return Task{ID: id, Dir: dir}, nil
}

// TaskStop implements Varvig.
func (e Exec) TaskStop(id string) error {
	_, err := e.run("task", "stop", id)
	return err
}

// Commit implements Varvig. Format: "<hash> <message>" on the first line.
func (e Exec) Commit(dir, message string) (string, error) {
	out, err := e.runIn(dir, nil, "commit", "-m", message)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(firstLine(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("varvigcli: could not read a change hash from %q", firstLine(out))
	}
	return fields[0], nil
}

// SpecAdd implements Varvig.
func (e Exec) SpecAdd(task, change string) error {
	_, err := e.run("spec", "add", task, change)
	return err
}

// Proposals implements Varvig via the JSON plumbing, which reports full change
// hashes and scores — `spec list` truncates the hash.
func (e Exec) Proposals(task string) ([]Proposal, error) {
	args := []string{"read", "proposals"}
	if task != "" {
		args = append(args, task)
	}
	out, err := e.run(args...)
	if err != nil {
		return nil, err
	}
	var props []Proposal
	if err := json.Unmarshal([]byte(out), &props); err != nil {
		return nil, fmt.Errorf("varvigcli: parsing `read proposals`: %w", err)
	}
	return props, nil
}

// SpecScore implements Varvig.
func (e Exec) SpecScore(task, change string, score float64) error {
	_, err := e.run("spec", "score", task, change, strconv.FormatFloat(score, 'g', -1, 64))
	return err
}

// SpecPromote implements Varvig. Format: "promoted <shortid> onto <ref>". The
// short id is returned as-is; callers that need the full hash resolve the ref
// they promoted onto, which is the authoritative answer anyway.
func (e Exec) SpecPromote(task, ref string) (string, error) {
	args := []string{"spec", "promote", task}
	if ref != "" {
		args = append(args, ref)
	}
	out, err := e.run(args...)
	if err != nil {
		return "", err
	}
	if ref != "" {
		if id, rerr := e.ResolveRef(ref); rerr == nil {
			return id, nil
		}
	}
	fields := strings.Fields(firstLine(out))
	if len(fields) >= 2 {
		return fields[1], nil
	}
	return "", nil
}

// SpecPrune implements Varvig.
func (e Exec) SpecPrune(task string, keep int) error {
	_, err := e.run("spec", "prune", task, strconv.Itoa(keep))
	return err
}

// Fetch implements Varvig.
func (e Exec) Fetch(addr, branch string) error {
	args := []string{"fetch", addr}
	if branch != "" {
		args = append(args, branch)
	}
	_, err := e.run(args...)
	return err
}

// Push implements Varvig.
func (e Exec) Push(addr, branch string) error {
	args := []string{"push", addr}
	if branch != "" {
		args = append(args, branch)
	}
	_, err := e.run(args...)
	return err
}

// HookSet implements Varvig. Format: "hook <event> -> <module id>".
func (e Exec) HookSet(event, modulePath string) (string, error) {
	out, err := e.run("hook", "set", event, modulePath)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(firstLine(out))
	return fields[len(fields)-1], nil
}

// HookRun implements Varvig. Format, per module:
//
//	hook 0: exit=2
//	  stdout: …
//	  stderr: …
//
// or the single line `no hooks bound to "<event>"`, which yields no results —
// distinguishable from a module that ran and allowed, which matters because the
// promotion gate must not read "no gate configured" as "the gate said yes"
// (FACTORY.md §6.2).
func (e Exec) HookRun(ctx context.Context, event string, input []byte) ([]HookResult, error) {
	cmd := exec.CommandContext(ctx, e.bin(), "hook", "run", event)
	cmd.Dir = e.Dir
	cmd.Stdin = bytes.NewReader(input)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("varvig hook run %s: %w: %s", event, err, strings.TrimSpace(errb.String()))
	}
	return parseHookResults(out.String()), nil
}

func parseHookResults(out string) []HookResult {
	var results []HookResult
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "hook ") && strings.Contains(trimmed, "exit="):
			code := 0
			if i := strings.Index(trimmed, "exit="); i >= 0 {
				code, _ = strconv.Atoi(strings.TrimSpace(trimmed[i+len("exit="):]))
			}
			results = append(results, HookResult{ExitCode: code})
		case strings.HasPrefix(trimmed, "stdout: ") && len(results) > 0:
			results[len(results)-1].Stdout = strings.TrimPrefix(trimmed, "stdout: ")
		case strings.HasPrefix(trimmed, "stderr: ") && len(results) > 0:
			results[len(results)-1].Stderr = strings.TrimPrefix(trimmed, "stderr: ")
		}
	}
	return results
}

// TrustList implements Varvig. Format of `varvig trust list`: whitespace-
// separated fingerprint, name, scope, rights — with "#" comments skipped.
func (e Exec) TrustList() ([]TrustEntry, error) {
	out, err := e.run("trust", "list")
	if err != nil {
		return nil, err
	}
	return parseTrust(out), nil
}

func parseTrust(out string) []TrustEntry {
	var entries []TrustEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		entries = append(entries, TrustEntry{
			Fingerprint: f[0],
			Name:        f[1],
			Scope:       f[2],
			Rights:      splitRights(f[3:]),
		})
	}
	return entries
}

func splitRights(fields []string) []string {
	var out []string
	for _, f := range fields {
		for _, r := range strings.Split(f, ",") {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, r)
			}
		}
	}
	return out
}

// GC implements Varvig. Format under --report-external:
//
//	external-unreachable:<n>
//	  <content hash>\t<media type>\t<locators>
func (e Exec) GC(reportExternal bool) (GCReport, error) {
	args := []string{"gc"}
	if reportExternal {
		args = append(args, "--report-external")
	}
	out, err := e.run(args...)
	if err != nil {
		return GCReport{}, err
	}
	return parseGC(out), nil
}

func parseGC(out string) GCReport {
	rep := GCReport{Raw: out}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 1 || f[0] == "" {
			continue
		}
		a := ExternalArtifact{ContentHash: f[0]}
		if len(f) > 1 {
			a.MediaType = f[1]
		}
		if len(f) > 2 {
			a.Locators = strings.Fields(f[2])
		}
		rep.ExternalUnreachable = append(rep.ExternalUnreachable, a)
	}
	return rep
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func hasRight(rights []string, want string) bool {
	for _, r := range rights {
		if r == want {
			return true
		}
	}
	return false
}

// scopeCovers reports whether a trust-store scope covers a path. "/" covers
// everything; otherwise the scope must be a path prefix. The comparison is on
// full path segments, so a scope of "src/generated/" does not cover
// "src/generated-secrets/" — a prefix match on raw strings would, and that is a
// privilege escalation by typo.
func scopeCovers(scope, path string) bool {
	scope = strings.TrimPrefix(scope, "/")
	path = strings.TrimPrefix(path, "/")
	if scope == "" {
		return true
	}
	if !strings.HasSuffix(scope, "/") {
		scope += "/"
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return strings.HasPrefix(path, scope)
}

// ScopeCovers is scopeCovers exported, so the promotion path can answer "is this
// path inside that scope?" with the same rule the trust store uses rather than a
// second, subtly different one.
func ScopeCovers(scope, path string) bool { return scopeCovers(scope, path) }
