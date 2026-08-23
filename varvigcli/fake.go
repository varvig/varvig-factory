package varvigcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Varvig for tests and the runnable demo. It models the
// parts of varvig that Factory's behaviour turns on — refs moved by
// compare-and-swap, content-addressed objects, notes, the speculation pool, and
// peer-to-peer exchange with an upstream that can be partitioned — and nothing
// else.
//
// It is in the non-test build deliberately. The §9 suite includes a partition
// test, an offline test, and a two-cells-one-upstream prototype; those have to
// run in CI on a machine with no varvig binary, or the properties they protect
// will be checked on one developer's laptop and nowhere else.
//
// What the Fake is NOT is a second implementation of varvig. It does not compute
// affected sets, order concurrent work, or decide what may be promoted onto
// what. Where a real varvig would apply judgement, the Fake applies the simplest
// rule that preserves the property under test, and says so in a comment.
type Fake struct {
	mu sync.Mutex

	// Label names this peer in error messages.
	Label string

	refs    map[string]string
	objects map[string][]byte
	notes   map[string]map[string][]Note
	tickets map[string]*fakeTicket
	pools   map[string][]Proposal
	hooks   map[string][]func([]byte) HookResult
	trust   []TrustEntry

	// Upstream is the peer Fetch and Push exchange with. Nil means this cell has
	// no upstream configured, which is a legitimate single-cell deployment.
	Upstream *Fake
	// Partitioned makes Fetch and Push return ErrUnreachable. This is how the
	// §9.2 and §9.3 tests take upstream away without taking the cell down.
	Partitioned bool

	// CommitFunc, if set, replaces the default commit. The default hashes the
	// directory name and the message, which is enough to give each attempt a
	// distinct change hash without a filesystem.
	CommitFunc func(dir, message string) (string, error)

	// Calls records every method invoked, in order, so a test can assert what a
	// cell did — and, for §9.9, what it did not.
	Calls []string
	// Committed records (dir, message) per commit.
	Committed []string
}

type fakeTicket struct {
	spec     string
	status   string
	scope    Scope
	blockers []string
}

// NewFake returns an initialized Fake.
func NewFake(label string) *Fake {
	return &Fake{
		Label:   label,
		refs:    map[string]string{},
		objects: map[string][]byte{},
		notes:   map[string]map[string][]Note{},
		tickets: map[string]*fakeTicket{},
		pools:   map[string][]Proposal{},
		hooks:   map[string][]func([]byte) HookResult{},
	}
}

func (f *Fake) note(call string) { f.Calls = append(f.Calls, call) }

// Called reports whether a call with the given prefix was made.
func (f *Fake) Called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// --- test setup helpers ---

// AddTicket registers a ticket with a spec, a declared scope and a status.
func (f *Fake) AddTicket(id, spec string, scope Scope, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == "" {
		status = "approved"
	}
	f.tickets[id] = &fakeTicket{spec: spec, status: status, scope: scope}
	f.refs["refs/varvig/tickets/"+id] = fakeHash("ticket:" + id)
}

// SetBlockers sets a ticket's derived blockers.
func (f *Fake) SetBlockers(id string, blockers ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tickets[id]; t != nil {
		t.blockers = blockers
	}
}

// SetStatus sets a ticket's derived status.
func (f *Fake) SetStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tickets[id]; t != nil {
		t.status = status
	}
}

// SetTrust replaces the trust store.
func (f *Fake) SetTrust(entries ...TrustEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trust = entries
}

// BindHook registers a module for an event. The function is the module: it
// receives the input bytes and returns the verdict, which is how a wasm gate's
// exit code is simulated without a wasm toolchain in the test path.
func (f *Fake) BindHook(event string, module func([]byte) HookResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[event] = append(f.hooks[event], module)
}

// RefSnapshot returns a copy of every ref, for assertions.
func (f *Fake) RefSnapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.refs))
	for k, v := range f.refs {
		out[k] = v
	}
	return out
}

// --- Varvig implementation ---

// Version implements Varvig.
func (f *Fake) Version() (string, error) { return "varvig fake (" + f.Label + ")", nil }

// TicketIDs implements Varvig.
func (f *Fake) TicketIDs() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("TicketIDs")
	out := make([]string, 0, len(f.tickets))
	for id := range f.tickets {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// Spec implements Varvig.
func (f *Fake) Spec(ticket string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[ticket]
	if !ok {
		return "", fmt.Errorf("fake: no ticket %s", ticket)
	}
	return t.spec, nil
}

// TicketStatus implements Varvig.
func (f *Fake) TicketStatus(ticket string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[ticket]
	if !ok {
		return "", fmt.Errorf("fake: no ticket %s", ticket)
	}
	return t.status, nil
}

// Scope implements Varvig.
func (f *Fake) Scope(ticket string) (Scope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[ticket]
	if !ok {
		return Scope{}, fmt.Errorf("fake: no ticket %s", ticket)
	}
	return t.scope, nil
}

// Blockers implements Varvig.
func (f *Fake) Blockers(ticket string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[ticket]
	if !ok {
		return nil, fmt.Errorf("fake: no ticket %s", ticket)
	}
	return t.blockers, nil
}

// Refs implements Varvig.
func (f *Fake) Refs() ([]Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Ref, 0, len(f.refs))
	for name, hash := range f.refs {
		out = append(out, Ref{Name: name, Hash: hash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ResolveRef implements Varvig.
func (f *Fake) ResolveRef(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.refs[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoRef, name)
	}
	return h, nil
}

// UpdateRef implements Varvig with real compare-and-swap semantics, because CAS
// failing safely rather than overwriting is the property the partition and
// offline tests exist to protect (FACTORY.md §5.2).
func (f *Fake) UpdateRef(name, newValue, oldValue string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("UpdateRef " + name)
	cur, exists := f.refs[name]
	switch {
	case oldValue == "" && exists:
		return fmt.Errorf("%w: %s already exists", ErrCAS, name)
	case oldValue != "" && !exists:
		return fmt.Errorf("%w: %s does not exist", ErrCAS, name)
	case oldValue != "" && cur != oldValue:
		return fmt.Errorf("%w: %s is %s, not %s", ErrCAS, name, cur, oldValue)
	}
	f.refs[name] = newValue
	return nil
}

// DeleteRef implements Varvig.
func (f *Fake) DeleteRef(name, oldValue string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("DeleteRef " + name)
	cur, exists := f.refs[name]
	if !exists {
		return fmt.Errorf("%w: %s", ErrNoRef, name)
	}
	if oldValue != "" && cur != oldValue {
		return fmt.Errorf("%w: %s is %s, not %s", ErrCAS, name, cur, oldValue)
	}
	delete(f.refs, name)
	return nil
}

// PutBlob implements Varvig.
func (f *Fake) PutBlob(content []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fakeHash(string(content))
	f.objects[id] = append([]byte(nil), content...)
	return id, nil
}

// ReadBlob implements Varvig, resolving a ref argument like the real command.
func (f *Fake) ReadBlob(idOrRef string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := idOrRef
	if h, ok := f.refs[idOrRef]; ok {
		id = h
	}
	b, ok := f.objects[id]
	if !ok {
		return nil, fmt.Errorf("fake: no object %s", idOrRef)
	}
	return append([]byte(nil), b...), nil
}

// AddNote implements Varvig.
func (f *Fake) AddNote(target, namespace string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("AddNote " + namespace)
	if strings.Contains(string(payload), "\n") {
		// The real adapter recovers a payload from `note list` by reading the
		// line after the header, so a payload with a newline would be silently
		// truncated there. Failing here keeps the Fake from passing a test the
		// Exec client would fail.
		return fmt.Errorf("fake: note payload contains a newline; Factory payloads must be canonical JSON")
	}
	if f.notes[target] == nil {
		f.notes[target] = map[string][]Note{}
	}
	f.notes[target][namespace] = append(f.notes[target][namespace], Note{
		Namespace: namespace,
		Author:    f.Label,
		Payload:   append([]byte(nil), payload...),
	})
	return nil
}

// Notes implements Varvig.
func (f *Fake) Notes(target, namespace string) ([]Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Note(nil), f.notes[target][namespace]...), nil
}

// TaskStart implements Varvig. It does not check the scope against anything: in
// a real repository varvig enforces it, and a Fake that reimplemented the
// enforcement would be asserting Factory's opinion of varvig's rules rather
// than varvig's.
func (f *Fake) TaskStart(req TaskRequest) (Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("TaskStart " + req.Scope)
	id := fakeHash(fmt.Sprintf("task:%s:%d:%d", req.Scope, len(f.Calls), time.Now().UnixNano()))[7:19]
	dir := req.Dir
	if dir == "" {
		dir = "./task-" + id
	}
	return Task{ID: id, Dir: dir}, nil
}

// TaskStop implements Varvig.
func (f *Fake) TaskStop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("TaskStop " + id)
	return nil
}

// Commit implements Varvig.
func (f *Fake) Commit(dir, message string) (string, error) {
	f.mu.Lock()
	commitFunc := f.CommitFunc
	f.note("Commit")
	f.Committed = append(f.Committed, dir+": "+message)
	f.mu.Unlock()
	if commitFunc != nil {
		return commitFunc(dir, message)
	}
	return fakeHash("change:" + dir + ":" + message), nil
}

// SpecAdd implements Varvig.
func (f *Fake) SpecAdd(task, change string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("SpecAdd " + task)
	for _, p := range f.pools[task] {
		if p.Change == change {
			return nil // content-addressed: adding twice is one candidate
		}
	}
	f.pools[task] = append(f.pools[task], Proposal{Task: task, Change: change, Created: int64(len(f.pools[task]) + 1)})
	return nil
}

// Proposals implements Varvig.
func (f *Fake) Proposals(task string) ([]Proposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if task != "" {
		return append([]Proposal(nil), f.pools[task]...), nil
	}
	var out []Proposal
	tasks := make([]string, 0, len(f.pools))
	for t := range f.pools {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)
	for _, t := range tasks {
		out = append(out, f.pools[t]...)
	}
	return out, nil
}

// SpecScore implements Varvig.
func (f *Fake) SpecScore(task, change string, score float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("SpecScore " + task)
	for i := range f.pools[task] {
		if f.pools[task][i].Change == change {
			f.pools[task][i].Score = score
			f.pools[task][i].Scored = true
			return nil
		}
	}
	return fmt.Errorf("fake: %s is not a candidate for %s", change, task)
}

// SpecPromote implements Varvig by promoting the highest-scoring candidate onto
// ref. The selection rule matches varvig's — highest score wins — because
// Factory's agreement metric is *about* that rule, and a Fake that selected
// differently would make the metric measure the Fake.
func (f *Fake) SpecPromote(task, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("SpecPromote " + task)
	best, ok := bestOf(f.pools[task])
	if !ok {
		return "", fmt.Errorf("fake: no candidates for %s", task)
	}
	if ref == "" {
		ref = "refs/heads/main"
	}
	f.refs[ref] = best.Change
	return best.Change, nil
}

func bestOf(pool []Proposal) (Proposal, bool) {
	var best Proposal
	found := false
	for _, p := range pool {
		if !found || p.Score > best.Score {
			best, found = p, true
		}
	}
	return best, found
}

// SpecPrune implements Varvig.
func (f *Fake) SpecPrune(task string, keep int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("SpecPrune " + task)
	pool := append([]Proposal(nil), f.pools[task]...)
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].Score > pool[j].Score })
	if keep < len(pool) {
		pool = pool[:keep]
	}
	f.pools[task] = pool
	return nil
}

// Fetch implements Varvig by copying upstream state this peer is missing.
//
// Objects and notes are copied unconditionally — they are immutable and
// content-addressed, so there is nothing to conflict. Refs are copied only when
// absent locally: a divergent ref is left alone rather than clobbered, which is
// the reconnect behaviour §5.2 describes (CAS fails safely rather than
// overwriting).
func (f *Fake) Fetch(addr, branch string) error {
	up, err := f.peer("Fetch")
	if err != nil {
		return err
	}
	up.mu.Lock()
	refs := copyMap(up.refs)
	objects := copyBytes(up.objects)
	notes := copyNotes(up.notes)
	tickets := copyTickets(up.tickets)
	pools := copyPools(up.pools)
	up.mu.Unlock()

	f.mu.Lock()
	defer f.mu.Unlock()
	for id, b := range objects {
		if _, ok := f.objects[id]; !ok {
			f.objects[id] = b
		}
	}
	for name, hash := range refs {
		if _, ok := f.refs[name]; !ok {
			f.refs[name] = hash
		}
	}
	for target, byNS := range notes {
		if f.notes[target] == nil {
			f.notes[target] = map[string][]Note{}
		}
		for ns, list := range byNS {
			f.notes[target][ns] = mergeNotes(f.notes[target][ns], list)
		}
	}
	for id, t := range tickets {
		if _, ok := f.tickets[id]; !ok {
			f.tickets[id] = t
		}
	}
	for task, pool := range pools {
		for _, p := range pool {
			if !hasCandidate(f.pools[task], p.Change) {
				f.pools[task] = append(f.pools[task], p)
			}
		}
	}
	return nil
}

// Push implements Varvig. A ref that exists upstream with a different value is
// refused with ErrCAS rather than overwritten — the same rule as Fetch, from the
// other side.
func (f *Fake) Push(addr, branch string) error {
	up, err := f.peer("Push")
	if err != nil {
		return err
	}
	f.mu.Lock()
	refs := copyMap(f.refs)
	objects := copyBytes(f.objects)
	notes := copyNotes(f.notes)
	f.mu.Unlock()

	up.mu.Lock()
	defer up.mu.Unlock()
	for id, b := range objects {
		if _, ok := up.objects[id]; !ok {
			up.objects[id] = b
		}
	}
	var refused []string
	for name, hash := range refs {
		cur, exists := up.refs[name]
		switch {
		case !exists:
			up.refs[name] = hash
		case cur == hash:
			// already there
		default:
			refused = append(refused, name)
		}
	}
	for target, byNS := range notes {
		if up.notes[target] == nil {
			up.notes[target] = map[string][]Note{}
		}
		for ns, list := range byNS {
			up.notes[target][ns] = mergeNotes(up.notes[target][ns], list)
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return fmt.Errorf("%w: upstream has diverged at %s", ErrCAS, strings.Join(refused, " "))
	}
	return nil
}

func (f *Fake) peer(call string) (*Fake, error) {
	f.mu.Lock()
	f.note(call)
	partitioned, up := f.Partitioned, f.Upstream
	f.mu.Unlock()
	if partitioned {
		return nil, fmt.Errorf("%w: %s is partitioned", ErrUnreachable, f.Label)
	}
	if up == nil {
		return nil, fmt.Errorf("%w: %s has no upstream", ErrUnreachable, f.Label)
	}
	return up, nil
}

// HookSet implements Varvig. The Fake cannot compile wasm, so binding by path is
// recorded but the module is a no-op; tests bind behaviour with BindHook.
func (f *Fake) HookSet(event, modulePath string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("HookSet " + event)
	return fakeHash("module:" + modulePath), nil
}

// HookRun implements Varvig.
func (f *Fake) HookRun(_ context.Context, event string, input []byte) ([]HookResult, error) {
	f.mu.Lock()
	modules := append([]func([]byte) HookResult(nil), f.hooks[event]...)
	f.note("HookRun " + event)
	f.mu.Unlock()
	var out []HookResult
	for _, m := range modules {
		out = append(out, m(input))
	}
	return out, nil
}

// TrustList implements Varvig.
func (f *Fake) TrustList() ([]TrustEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TrustEntry(nil), f.trust...), nil
}

// GC implements Varvig.
func (f *Fake) GC(reportExternal bool) (GCReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("GC")
	return GCReport{}, nil
}

// fakeHash mints an object id in varvig's shape: bare lowercase hex, no label.
//
// The shape matters more than it looks. varvig prints an object id as
// multihash.Hex() — hex with the algorithm encoded *inside* the bytes, not as a
// text prefix — and Factory interpolates those ids into ref names, including
// varvig's pin refs, which reject anything that is not hex. A Fake that minted
// Factory-labelled "sha256:…" ids would let a test pass against a ref name the
// real varvig would refuse.
func fakeHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyBytes(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func copyNotes(in map[string]map[string][]Note) map[string]map[string][]Note {
	out := make(map[string]map[string][]Note, len(in))
	for target, byNS := range in {
		out[target] = make(map[string][]Note, len(byNS))
		for ns, list := range byNS {
			out[target][ns] = append([]Note(nil), list...)
		}
	}
	return out
}

func copyTickets(in map[string]*fakeTicket) map[string]*fakeTicket {
	out := make(map[string]*fakeTicket, len(in))
	for k, v := range in {
		c := *v
		out[k] = &c
	}
	return out
}

func copyPools(in map[string][]Proposal) map[string][]Proposal {
	out := make(map[string][]Proposal, len(in))
	for k, v := range in {
		out[k] = append([]Proposal(nil), v...)
	}
	return out
}

// mergeNotes unions two note lists by payload, so a fetch followed by a push
// does not duplicate evidence. Notes are additive and unordered by nature, which
// is why replication needs no conflict rule.
func mergeNotes(into, from []Note) []Note {
	seen := map[string]bool{}
	for _, n := range into {
		seen[n.Namespace+"\x00"+string(n.Payload)] = true
	}
	for _, n := range from {
		k := n.Namespace + "\x00" + string(n.Payload)
		if !seen[k] {
			into = append(into, n)
			seen[k] = true
		}
	}
	return into
}

func hasCandidate(pool []Proposal, change string) bool {
	for _, p := range pool {
		if p.Change == change {
			return true
		}
	}
	return false
}

// Fake must satisfy the interface it stands in for; a divergence here is the
// one bug that would make every test above it meaningless.
var _ Varvig = (*Fake)(nil)
