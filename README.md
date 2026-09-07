# varvig-factory

The autonomous cell that turns tickets into verified, promotable changes on top
of [varvig](https://github.com/varvig/varvig).

A **cell** watches a varvig repository, decides whether it should attempt a
ticket, submits the work to varvig, builds and tests the result, publishes
evidence and an environment descriptor, and — by default — leaves the decision
to promote with a human.

This repository is separate from `varvig/varvig` for the reason varvig's design
gives: Factory is the volatile layer — model runtimes, hardware, scheduling
policy — and must remain replaceable. Inside the core repo it would become de
facto part of the product, and varvig's format neutrality would stop being real.

## The two-sentence version of the boundary

**Factory decides whether this cell should attempt this ticket.** That is claim
policy.

**varvig's scheduler decides how concurrent work inside a cell interleaves** —
read/write sets, serialization, regeneration on CAS failure. That is task
scheduling.

Conflating them means reimplementing affected-set logic badly, in the layer
least equipped to do it. So Factory calls `varvig task` and lets varvig handle
concurrency, and a [guard test](./guard) fails the build on any declaration that
would compute scheduling here.

## There is no central Factory

Upstream is a varvig peer, nothing more. It runs no Factory process, holds no
queue, and issues no RPCs. Coordination happens by exchanging repository state.

The practical consequence is the one worth having: a disconnected cell keeps
working. Its useful state already exists locally as immutable objects, and on
reconnect it exchanges missing objects and attempts compare-and-swap, which fails
safely rather than overwriting. Losing upstream does not strand work, which is
what separates this from a worker queue.

## Start here

[**`CELL.md`**](./CELL.md) is the cell contract: identity, ref and note
namespaces, the capabilities object, evidence and the environment hash, claims,
budget, authority, and the thirteen things a cell must not do. It is normative,
and it was written before any daemon code — it is what lets a Micro cell built
today join a federation built later, and it is expensive to retrofit once cells
exist and have written state.

Then run [`cmd/factory-demo`](./cmd/factory-demo/main.go) — a committed program,
not a scratch script, and the executable version of the argument this README
makes:

```sh
go run ./cmd/factory-demo
```

Six phases against in-memory fakes. A Mini cell attempts and a Micro cell
independently verifies; gated promotion evaluates every condition and acts on
none of them; a partition where both cells attempt the same task and both
attempts survive reconnect; autonomous promotion, earned per scope, stopped two
different ways by the kill switch; and finally a disconnected cell refused a
promotion while it goes on spending its lease, reserving, settling above quote,
and being refused a second pending order and a self-authorized one; and finally
a ticket driving a real order through the loop — placed by micro-b, the cell
with no model at all, because authority to spend is a lease and not a GPU. No
varvig binary, no GPU, no network.

## Quick start against a real repository

```sh
go build -o varvig-factory ./cmd/varvig-factory   # single static binary; CGO not required

cd /path/to/your/varvig/repo
varvig-factory init --profile micro --cell-id micro-a
varvig-factory capabilities          # print and publish what this cell advertises
varvig-factory once                  # one pass of the loop, with a report
varvig-factory run                   # loop until interrupted
```

`init --profile micro` writes a cell that **verifies and builds but does not
attempt**. That is the recommended default and the subject of the next section.

Run `varvig-factory` with no arguments for the full command list.

## Cell classes, and flat factories

**There are no tiers.** A cell class describes *capacity*; it is not a rank, and
it carries no authority. Scaling means more cells or bigger cells — never a
topology change.

| Cell class | Inference | Recommended roles |
|---|---|---|
| **Micro** | CPU-local small model | `verify`, `build` — *not* `attempt` by default |
| **Mini** | GPU-local model | `attempt`, `verify`, `build` |
| **Medium** | heavy / multi-GPU / distributed local execution | `attempt`, `verify`, `build` |

A factory is one or more cooperating cells, flat:

```
Factory Alpha
├── Marble [Micro]
├── Fox    [Micro]
└── Raven  [Mini]
```

There is **no designated upstream peer and no hierarchy**. Designating a
coordinator would reintroduce the central controller the whole model exists to
avoid — and authority hierarchy is not topology hierarchy: an overseer sits above
other principals in *key scope* and nowhere in particular on the network. Don't
let the authority chain leak into the connection graph. Factories of factories
are explicitly not in this design; flat membership plus scoped authority covers
the same ground with less machinery.

Classes are **configuration profiles, not code paths**. Micro and Mini differ
only in which model runtime and budget the config names; `profile.Wire` — the one
function that turns a config into a running cell — never reads the profile name,
and [a test](./profile/profile_test.go) reads its syntax tree to prove it. If a
class ever requires a branch in the code, the abstraction has failed.

> One gap between this and the code: the loop still takes a single `upstream`
> address to sync against. It is not a coordinator — nothing reads from it that a
> peer could not serve — but "any member may act as a rendezvous, several at
> once" is not implemented yet. See [CELL.md §11](./CELL.md).

### Micro's honest role

A CPU-local model authoring code will lose nearly every selection while still
consuming review attention — net negative. Micro is strong as a **verification
and build cell**: deterministic work, cheap, no model-quality problem, and it
makes the old-hardware story genuinely compelling rather than aspirational.

So Micro ships with `roles: ["build", "verify"]` and attempting is opt-in.

### Independent verification is a property of flat membership

Because a cell can verify without attempting, evidence can be produced by a
**different cell than the one that authored the attempt**. This is what makes
autonomous promotion defensible — and it comes from flat membership, not from any
hierarchy: a signature proves who asserted a test result,
not that the run was honest. Independence is the only leverage available, so it
is spent rather than assumed — and an attempt whose only evidence comes from its
own cell is never autonomously promotable.

## Promotion

Both modes work and **neither is privileged in the code**.

| Mode | Who signs | Default |
|---|---|---|
| `gated` | A human, via CLI or the Flutter app | **Yes** |
| `autonomous` | A factory key with `promote` rights, driven by a policy module | Opt-in |

There is one evaluation path. Mode is an input to it, checked alongside the other
conditions, not a branch that selects between two implementations — which is
what keeps the autonomous path exercised by the gated majority. A gated cell runs
the policy module, evaluates every condition, logs the verdict, and does not act.

### Autonomous promotion needs no new auth model

A factory key is an entry in `.varvig.d/allowed_keys` like any other, scoped by
path and revocable by deleting the line:

```
SHA256:fT8kLm…    factory-prod    src/generated/    promote
```

**Scope it narrowly.** The trust store already gives per-path granularity; use
it. A factory key with `promote` at `/` is the same risk as an unattended root
credential, and `EnableAutonomous("/")` is refused outright for that reason.

### The five conditions, enforced in the binary

Every one is a function call with a test, and a refusal names which one failed —
because "not promotable" without a reason sends an operator looking for the
override.

1. **Evidence must come from a cell other than the attempting cell.**
2. **Environment class must match** the declared baseline for that path.
   Cross-class comparison defers to a human; a *missing* environment is unknown
   class and never matches.
3. **Re-verification before promotion**, not merely evidence replay. Replaying a
   stored record proves a cell once said the tests passed; re-verification proves
   they pass against the state about to be promoted. Only the second is worth
   anything at the moment of promotion, and only the second costs anything —
   which is why an implementation drifts toward the first, and why it is a
   separate interface here.
4. **Path scope must be explicitly enabled.** Per-path, never global.
5. **A promotion-agreement metric must exist** for that scope, above threshold.

Three more, which are not additions to the policy but the surrounding facts
without which the five would be checking a promotion that could not happen
anyway: the trust store must actually grant `promote` at that path; the cell's
view of that trust store must be current ([authority](#authority-spend-that-cannot-be-regenerated));
and the policy module must return `promote`. An **unconfigured gate is not an
approving gate** — the promotion rule has to be a reviewed, versioned object, and
"there isn't one" is not consent.

### The gate is a wasm policy module

Promotion policy is a content-addressed wasm veto module, versioned in the repo
alongside the code it guards. It receives the attempt, its evidence and its
environment, and returns promote / refuse / defer-to-human. So **the promotion
rule is itself reviewed, versioned and auditable** — the minimum bar for letting
a machine promote.

```sh
varvig-factory gate --bind policy.wasm
```

The module runs **inside varvig**, not inside Factory: varvig already has a
closed WASI sandbox for hooks and a content-addressed module store to go with it,
so Factory binds its module to a Factory-specific event and runs it there.
Embedding a second wasm runtime here would mean two sandboxes with two sets of
escape bugs, and a policy whose behaviour depended on which binary loaded it.

Exit codes: `0` promote, `1` refuse, `2` defer to a human. Anything else — an
unrecognised code, a crash, no module — defers. A gate that cannot be understood
must never read as consent.

### The measurement that licenses autonomy

While running gated, a cell records for each promoted ticket whether the
highest-scoring attempt was the one the human promoted. That agreement rate is
the only honest basis for enabling autonomous promotion, and it is **per-module,
not global** — a cell may be trustworthy at generating serializers and useless at
touching billing, and an average hides exactly that.

```sh
varvig-factory agreement            # every scope, worst first
varvig-factory agreement --scope src/generated/
```

Below 80%, autonomous mode refuses to enable and says why.

**One judgement beyond the spec, stated rather than buried:** the spec names a
threshold but not a sample size. A threshold alone is not enough — one ticket
promoted in agreement is a rate of 100%, and it would unlock a scope on a single
observation. So there is also a minimum sample, defaulting to 20 and
configurable. The spec's own argument, that autonomy is *earned with evidence*,
is what requires it.

The risk being managed is varvig's design §5: the moment promotion is automatic,
the test suite silently becomes the real source of truth, and speculation scoring
will find whatever it does not check. Autonomy is not forbidden — it is earned
per scope, with evidence.

### Kill switch, two ways

```sh
varvig-factory promote --mode gated
```

takes effect on the next promotion decision anywhere in the cell, **including in
an already-running loop**. The switch is a small file that is re-read on every
decision — no signal, no socket, no IPC, and no window in which the switch has
been thrown and the cell is still promoting. That window is the entire thing the
kill switch exists to eliminate.

And reverting the `allowed_keys` line is sufficient on its own to stop autonomous
promotion federation-wide. Both paths are tested; see §9.7 below.

## Budget

A cell declares a spend cap and halts when it is exceeded. Not optional:
attempts multiply cost, and a disconnected cell claiming speculatively can burn
budget on work that proves duplicative.

```json
{
  "inference_daily":   50.0,
  "verify_concurrent": 4,
  "storage_gb":        200,
  "attempts_default":  3,
  "per_call_cost":     0.02
}
```

- **Halt, do not degrade.** A cell out of budget stops claiming and says so. It
  does not switch to a smaller model: that produces attempts that pollute
  selection, and the failure is invisible in the output — it shows up weeks later
  as a selection statistic nobody can explain.
- **Offline speculation is capped separately and more tightly** (a quarter of the
  daily cap by default), since a disconnected cell cannot check whether another
  cell already succeeded. A *looser* offline cap is rejected at startup as the
  typo it almost certainly is.
- **Storage pressure releases local artifacts, then pins, then sweeps** — pin
  release before GC, so a cell drops its own retention obligations deliberately
  rather than collecting state another cell is evaluating until something breaks.
- A cap with no price configured is rejected at startup: a cell that can spend
  but cannot price what it spends has no cap at all.

```sh
varvig-factory budget
```

## Authority: spend that cannot be regenerated

The budget above bounds compute, which is regenerable — exceeding it wastes money
and nothing else. Ordering PCB fabrication, contracting a human, shipping
something, moving money: those cannot be re-run, discarded, or regenerated, and
they get a different mechanism.

**The load-bearing distinction is shared versus exclusive authority, not fresh
versus stale.**

| | Envelope | Lease |
|---|---|---|
| What it is | a **shared ceiling** across every cell under one overseer | an **exclusive allocation** to one cell |
| Ref | `refs/envelopes/<overseer-id>` | `refs/leases/<cell-id>/<capability>` |
| Enforceable from a stale view? | no — another cell may have spent it | yes — nobody else can spend it |
| Spendable offline | no | **yes, indefinitely** |

That table is why a disconnected cell can keep doing real work. The amount was
committed when the lease was issued, so there is no connectivity in the spend
path and no renewal protocol to add. Grouped by reversibility rather than by
"is it a write":

| Act | With a stale view of trust state |
|---|---|
| Propose | allowed — append-only bounds the damage |
| Promote | **refused** — it moves a ref, and a shared ceiling cannot be enforced locally |
| Effectful | allowed **within an outstanding lease**; refused with no lease |

Beyond the lease **escalates and never falls back to the envelope**: a lease that
can be exceeded by drawing on the shared ceiling is advisory, and an advisory
exclusive allocation is a shared one.

### Tightening bites; loosening does not

An overseer who narrows an envelope mid-run has the tighter ceiling honoured
before the cell's next effectful action — with **no fresh sync**, because
adopting a tighter ceiling can only reduce spend, and being wrong there means
spending less than authorized, which nobody has to undo. A *wider* envelope
grants a cell nothing: the effective allocation is `min(lease, ceiling)`, so more
headroom requires a **new lease**, which only the overseer can write.

The minimum is the whole mechanism, and the asymmetry the spec asks for falls out
of it for free — which is what lets this path keep §4.3b's property that
effectful action inside a lease needs no connectivity. A staleness check here
would have taken that back.

Two things the cap does **not** buy, stated because a cap is easy to mistake for
a guarantee:

- **The ceiling is shared, so local capping does not bound the sum.** Three cells
  each capping themselves at one ceiling still permits their total to exceed it.
  Tightening is enforced by the overseer **not replenishing**; this is a floor of
  safety underneath that.
- **It cannot claw back an unspent lease without reaching the cell.** A
  partitioned cell has not seen the tightening and will spend its lease —
  acceptable rather than a hole, because exposure was bounded by the lease amount
  when it was issued. `varvig-factory authority` therefore reports exposure from
  the leases *as issued*, not as tightened.

A tightening never un-spends money, and the bounded lease is a view for deciding,
never a value to store — writing it back would rewrite the record of what the
overseer actually committed to. `reclaim_after` is a signal to the
overseer, not an expiry — it does not stop the holder spending, and a lease with
any recorded spend is never reclaimed, because the cell may have placed an order
it has not yet reported.

```sh
varvig-factory authority          # leases, exposure, and the ceilings they draw on
```

That report reads **every** cell's leases, not only the local one, because the
sum is the number that matters and a cell can only see its own share. It also
re-checks the exclusivity invariant against the envelope and says so loudly if
outstanding leases have drifted past the ceiling — because if they have, the
exposure figure above it is wrong.

Two defaults worth stating, since both are places where a plausible choice is
wrong:

- **An envelope with no ceilings is malformed, not unlimited**, and an unlisted
  capability has no ceiling to be under. Silence is not permission.
- **There is no staleness clock by default.** A successful sync establishes
  current state; adding an age threshold on top would be inventing policy. An
  operator who wants one — for a sync loop that has stalled without failing —
  configures `max_trust_age`.

### Why freshness is Factory's job and not varvig's

Not a division of labour, a structural fact: varvig's ref-update verification
checks a signer against the trust file *as the verifying peer holds it*, and a
partitioned peer holds a stale file it has every reason to believe is current.
Nothing inside varvig can tell the difference. The loop, which ran the sync, can
— so it records `Reachable` and `At`, and promotion reads them.

None of this asks varvig to change. Envelopes, leases and reservations are refs
under new prefixes, signed and CAS-updated like any other.

### Effectful capabilities are built as refusals

An effectful capability looks exactly like an ordinary one until the invoice
arrives, so [`effect/`](./effect/effect.go) is the guards first and the happy
path last:

1. **`attempts` is 1 — by rejection, not by clamping.** `--attempts 3` on a
   board order means three orders and three invoices. A clamp would execute
   something other than what was asked for, on the one class of action where
   that means a wrong order rather than a wasted GPU-hour.
2. **A mandatory idempotency key**, *derived* from `(task-id, capability alias,
   interface hash, canonical payload)` with each component length-prefixed. A
   retry after a mid-flight network failure recomputes the same key from the same
   intent, so "the same action" and "the same key" cannot drift apart.
3. **A cell never authorizes its own effectful action** — not even holding a
   factory key with `promote`. Promote rights move refs; they are not a licence
   to spend money, and conflating the two turns a scoped repository credential
   into a purchasing credential. The authorizing principal need not be human: an
   overseer agent satisfies this fully.
4. **Bounded by the envelope, spent from the acting cell's own lease.** Another
   cell's lease is not spendable, however much headroom it has.
5. **Never auto-retried, never regenerated.** A failure escalates; retry is an
   authorized decision, not a loop behaviour.

A capability reference names the **interface hash**, not only the alias — two
factories may hold the same alias without agreeing who owns the name, so matching
is on the hash, and an alias match with a hash mismatch is the collision the
binding exists to catch. The hash is in the idempotency key too, so re-pointing
an alias cannot make a new action look like an old one.

### A ticket can now order a thing

Until this landed, `authority` and `effect` were a well-tested library with no
path from a ticket to an order. A ticket names an effectful capability in the
same directive that carries build and test requirements:

```
factory-requires: effect=pcb-fabrication@1 interface=1220a1b2…
factory-effect: {"gerber":"rev-c","quantity":5}
```

That takes the ticket off the speculation path entirely — it is not attempted,
scored or promoted, because none of those mean anything for an action that
happens once in the physical world. Two gates it is *not* subject to:

- **Not the `attempt` role.** Authority to spend is a lease. Tying it to holding
  a model would make the right to spend money depend on the presence of a GPU, and
  a Micro cell with a lease is as entitled to place an order as a Mini one.
- **Not the inference budget.** An order is paid from its lease; coupling the
  two would surface as an order that silently did not happen.

A cell needs **both** a lease and an executor for a capability. Holding either
alone means it declines the ticket rather than claiming it and discovering the
problem afterwards.

The executor is a seam like the model runtime and the build sandbox, and for a
sharper reason than either: the alternative to a fake is a real board order. Only
a **refusing** executor is built in — pointing a cell at it proves the wiring
works, with the ticket claimed, quoted, authorized and reserved, and nothing
ordered. Real integrations are compiled in by whoever operates the factory,
because a plugin loader reached by name from a config file, in the one path that
spends money, is a worse idea than a rebuild.

### What the cell does after it acts

    quote → reserve → authorize → execute → settle

Authorization is checked **before** the reservation, not between it and
execution: a refusal that arrives after the key is claimed has already consumed
the cell's one chance to act on that ticket.

What happens next is decided by what the cell *knows*, not by what it hopes:

| Executor returns | Meaning | Lease | Reservation |
|---|---|---|---|
| success | the effect happened | hold becomes spend, at the price actually charged | `done`, with the far end's reference |
| a definite rejection | **no** effect occurred | hold released | `failed`; the key stays claimed |
| anything else | **unknown** | hold stands | `pending`; escalates |

The third row is the one implementations get wrong. A timeout, a dropped
connection and a 500 are all consistent with the order having been placed, so
only an executor that was *definitely told no* may claim nothing happened — and
the hold is deliberately not released on an unknown outcome, because the money
may be gone and returning it would let the cell spend it twice.

If the effect succeeds and recording it fails, that is the one error that stops
the pass rather than being counted: the lease still holds rather than spends and
the reservation still reads pending, so an operator sees an unresolved action
rather than a clean slate.

### Reserve, execute, settle

A derived key says what "the same action" means; it does not stop the action
happening twice. The key is claimed in a ref — create-only, so whoever creates it
executes and everyone else is refused — **before** the effect is attempted, and
the same step holds the lease headroom for it:

```
reserve  ->  refs/reservations/<cell-id>/<key>  = pending
            +  the lease holds the quoted amount
execute  ->  the external effect
settle   ->  done, with the far end's own order number
            +  the hold becomes spend, at the price actually charged
```

**The hold is why a derived key is not enough on its own.** Without it, two
pending actions each check the same headroom, each pass, and together exceed the
lease — the first order's money looks available right until its invoice arrives.
So headroom has three states: allocated, held, spent. The hold is taken *before*
the key is claimed, and released if the claim then fails; leaking headroom is
recoverable and a double-spend is not, so the two writes fail in the recoverable
direction.

**Expiry releases the hold, never the key.** A lost external response must not
consume budget for good, so the money comes back on a timer. It must also not let
the action be submitted again, because it may well have happened — so the key
stays claimed permanently. Two resources, two rules. A reservation whose hold has
lapsed is still pending, still reported, and still waiting on a principal; if
that principal finds the order was real, the spend lands on the lease with no
hold left to convert.

A crash in the middle leaves `pending`, which is the honest record of the state
that matters: the cell does not know whether the order was placed. It escalates.
It is **not** retried, and the reservation is **not** deleted to clear the way —
deleting it is exactly how the second invoice arrives. A timeout stays pending
rather than becoming `failed`, because "we never heard back" and "it did not
happen" are different claims. And a cell cannot resolve its own pending
reservation: checking the far end and asserting what is true there is the same
class of act as authorizing the spend was.

`varvig-factory authority` leads with those unresolved actions, before the
leases, because they are the only line on that page that might be an order nobody
knows about.

Every unmet rule is reported at once. Elsewhere an early exit saves an expensive
re-verification; nothing here is expensive, and an operator about to spend money
should see the whole list rather than one round trip per broken rule.

## Adapters

Three seams. Everything hardware- or vendor-shaped lives behind them, so neither
varvig nor Factory's core loop learns about CUDA, quantization or container
runtimes.

| Seam | Package | Implementations |
|---|---|---|
| Model runtime | [`inference/`](./inference) | `http` (ollama, vLLM, llama.cpp server, hosted APIs), `command` (llama.cpp CLI, any local wrapper), `none` |
| Build sandbox | [`sandbox/`](./sandbox) | `subprocess`, `container`, `nix` |
| Artifact store | [`artifact/`](./artifact) | local CAS, plus a command-driven remote for OCI registries and S3-compatible stores |

Each adapter reports a **deterministic environment fragment**, and the fragment
is a *measurement*, not a configured claim: the HTTP runtime probes the server
for its version, the sandbox runs its version probes **through its own wrapper**
so a container cell reports the toolchain inside the container rather than the
host's. An adapter that cannot describe itself reproducibly returns
`ErrIndescribable` and the cell refuses to start — emitting a guessed
environment would make every downstream cross-cell comparison a comparison of
guesses.

Two adapters disagreeing about the machine they both run on is a hard error
rather than a last-writer-wins merge. If the sandbox says Go 1.24.7 and the model
runtime says Go 1.22, one of them is wrong, and either value produces an
environment hash that certifies a fiction.

The three sandbox profiles are **one type with a different wrapper**, not three
implementations — `Subprocess`, `Container` and `Nix` all return the same
`*sandbox.Exec`. A container image must be digest-pinned, and a tag is refused:
a tag is mutable, so a tag-pinned sandbox publishes a stable environment hash
while the ground under it moves.

## Artifacts

Binary outputs are referenced by `artifact-ref` objects, never stored in varvig.
A cell records one with varvig's own verb, `tickets attach-artifact`, which stores
a real `TypeArtifactRef` and pins it for reachability.

That last part is the reason to use the verb rather than a note with the same
fields: only a real artifact-ref reaches `varvig gc --report-external`, so only
that form lets a cell ever learn its registry bytes are no longer needed. A note
is invisible to GC's mark phase, and the symptom is a report that stays
permanently empty. `varvigcli`'s integration test proves the difference by making
an artifact unreachable and checking which form comes back.

Factory hashes artifact bytes with SHA-256 and converts to varvig's multihash on
the way out — `sha256:<hex>` → `1220<hex>`. Lossless, no rehash, no dependency,
because SHA2-256 is a registered multihash code. Reading back, a cell also accepts
BLAKE3, since a peer may have hashed with varvig's default; a hash in an algorithm
it cannot name is an error rather than a value passed along unlabelled.

The attachment is ticket-anchored, because that is the anchor the verb takes, so
`produced_by` carries the attempt and is what distinguishes two attempts' outputs
under one ticket.

**Speculative artifacts stay cell-local.** Replicate on promotion, not on attempt
— a federation that replicated every attempt's build output has turned speculation
into a bandwidth bill. The seam encodes that structurally rather than by
convention: `Put` is always local and `Replicate` is the only method that can send
bytes, so the attempt path has no method that could leak. Recording a reference is
not replicating bytes.

Factory owns registry credentials; **varvig must never acquire them**. varvig
reports unreachable artifact hashes; deletion is Factory's or the operator's
action.

A cell running against a core without the verb falls back to a `factory/artifact`
note and **says so every time**, naming what is lost. Degrading is acceptable;
degrading quietly is not.

## Layout

```
CELL.md              the cell contract — normative, read this first
cell/                the contract in code: names, capabilities, evidence,
                     environment + its hash, claims. No dependencies on anything.
varvigcli/           the Varvig interface + an Exec adapter over the public CLI,
                     and an in-memory Fake that models refs-with-CAS, notes,
                     the speculation pool and a partitionable upstream
inference/           model-runtime seam
sandbox/             build-sandbox seam
artifact/            artifact-store seam
budget/              spend caps, halt behaviour, storage-pressure relief
authority/           envelopes and leases: shared ceilings versus exclusive
                     allocations, what a stale view still permits, and the refs
                     they live in
effect/              effectful, non-regenerable capabilities — the refusals,
                     reserve/execute/settle over a reservation ref, and the
                     executor seam (with a refusing default and a counting fake)
claim/               claim policy: should this cell attempt this ticket?
loop/                the ten-step cell loop, and verification of peer attempts
gate/                the wasm promotion-policy module interface
agreement/           the promotion-agreement metric, per scope
promote/             both modes, the five conditions, the kill switch
profile/             micro/mini/medium as configuration, and the wiring
conformance/         the spec's §9 tests for the single-cell contract (the
                     authority ones live with the packages they constrain)
guard/               build-failing guards: no second scheduler, no branching
                     on cell class, no third-party dependencies
cmd/varvig-factory/  the cell binary
cmd/factory-demo/    the runnable Medium prototype
```

Nothing here imports varvig's Go packages. The binding is the CLI and the wire
protocol — Factory is a peer, and a peer that linked against the core's
internals would be part of the core.

The module has **no third-party dependencies**. Every adapter is `net/http`,
`os/exec` and `encoding/json`, and [a guard](./guard/guard_test.go) fails the
build if a `require` directive or a `go.sum` ever appears.

### Two consequences of being dependency-free, stated plainly

**Configuration is JSON, not the YAML the design notes use in their examples.**
The standard library has no YAML parser. Every field maps one-to-one onto the
spec's keys, so a snippet from the design notes translates mechanically. Unknown
fields are a hard error: a typo'd `inference_daily` that quietly means "no cap"
is the single worst misconfiguration this file can carry.

**The environment hash is SHA-256, labelled `sha256:`, not the BLAKE3 varvig uses
for object identity.** Nothing is lost. It is *Factory's* identity for a
descriptor — it names a note and is compared against other Factory environment
hashes — and it does not claim to be the varvig object id of the equivalent
`TypeEnvironment` object. The label is what makes the choice non-load-bearing:
written state stays unambiguous when a second algorithm exists. See `CELL.md`
§4.3.

## How Factory talks to varvig

Where varvig has a JSON plumbing command, [`varvigcli`](./varvigcli) uses it:
`read refs`, `read proposals`, `read blob`. Where it does not, the adapter parses
porcelain, and each such method names the exact format it depends on, so a change
in varvig's human output surfaces here as a failing parser test rather than as a
cell that silently sees no tickets.

One decision makes that parsing safe rather than merely tolerable: **every
payload Factory writes is canonical JSON, and therefore newline-free.** `varvig
note list` prints a header line followed by the payload indented by two spaces,
so a newline-free payload is recoverable exactly. A pretty-printed one would not
be — and the Fake refuses a multiline payload so a test cannot pass against
behaviour the real client would break on.

Two notes on places where the two layers meet:

- **Artifact-refs now go through `varvig tickets attach-artifact`**, which stores a
  real object. This replaced an earlier note-based form — the promise that the
  shapes mirrored `TypeArtifactRef` "so a native verb can replace the note form
  without changing this contract" held: the field names were already identical, so
  the switch touched the write path and nothing else.
- **Environment descriptors** still have no CLI, so they remain notes in
  `factory/environment`, mirroring `TypeEnvironment` field for field on the same
  bet. Unlike artifacts nothing is lost by the note form here: an environment is
  compared, not garbage-collected.
- **Pins** use varvig's own ref naming — `refs/pins/{hex peer id}/{16 hex
  not_after}/{object hash}` — rather than a shape of Factory's own. A pin written
  in a shape varvig cannot parse would occupy the namespace while failing to be
  recognised as a pin.

## Requirements on a ticket

varvig has no field for build/test capability tokens, and giving it one would
teach the core about toolchains. So they travel in the ticket's spec as a
directive line a human can type:

```
factory-requires: build=go,flutter test=unit,large-memory attempts=5
```

A ticket with no directive requires nothing, which is the right default — most
tickets are ordinary code changes, and demanding an annotation on each would make
the mechanism something people work around. Unknown keys are ignored rather than
rejected, so a ticket written for a newer Factory is still attemptable by an
older cell.

A ticket may **instead** name an effectful capability, which takes it off the
speculation path entirely (see [above](#a-ticket-can-now-order-a-thing)):

```
factory-requires: effect=pcb-fabrication@1 interface=1220a1b2…
factory-effect: {"gerber":"rev-c","quantity":5}
```

Here the tolerance above is inverted, and every rule is a refusal rather than a
correction — this is the class of work where "we assumed you meant X" buys a
wrong order. The interface hash is required, not just the alias. The parameters
are required, must be valid JSON, and are kept **verbatim**, because they are
hashed into the idempotency key and re-encoding them (even correctly) risks two
readings of one ticket producing two keys and therefore two orders. `attempts`
may only be 1. And a ticket does one kind of work: `build`/`test` and `effect`
are mutually exclusive.

**A malformed effect never falls through to the attempt path.** Answering "order
me a circuit board" by writing code is the failure mode that rule exists to
prevent, so the requirement survives with the reason attached and the ticket is
skipped rather than reinterpreted.

## Claims

A claim is a TTL'd ref at `refs/claims/<cell-id>/<task-id>`. Three rules, and
they are the whole protocol:

- **Claims are advisory.** They cannot be exclusive across a partition. Two cells
  may each compare-and-swap successfully against their own view, and both are
  correct.
- **Duplicate attempts are normal and are the point** — branching is search. A
  cell must not add consensus, leader election, or a lock service to prevent
  them. Skipping a task another cell has freshly claimed is *budget politeness*,
  configurable, and inert across a partition by construction.
- **A cell may claim and attempt while disconnected.** Required, not tolerated:
  local-first operation is the property that makes the cell model worth having.

`not_after` is mandatory. A claim without an expiry is a lock, and a lock held by
a partitioned cell is a task nobody may ever attempt again.

## Testing

The spec's §9 items are named tests. The first nine live in
[`conformance/`](./conformance/conformance_test.go):

| # | Test | What it holds |
|---|---|---|
| 1 | `Test01_TierEquivalence` | Micro and Mini configs, one binary, same lifecycle (the spec's name for it; classes, not tiers) |
| 2 | `Test02_PartitionDuplicates` | two partitioned cells both attempt; both attempts survive reconnect |
| 3 | `Test03_OfflineAttempt` | full lifecycle with upstream unreachable, then reconcile |
| 4 | `Test04_EnvironmentDeterminism` | identical adapters, identical hash; different ground, different hash |
| 5 | `Test05_SelfVerificationRefusal` | self-verified attempts are never autonomously promoted |
| 6 | `Test06_BudgetHalt` | stops claiming at the cap, and does not downgrade the model |
| 7 | `Test07_KillSwitch` | mode flip with no restart; `allowed_keys` revocation |
| 8 | `Test08_AgreementRateGate` | refuses below threshold, with the numbers |
| 9 | `Test09_NoSecondScheduler` | submits the declared scope; never derives one |

The authority model's numbered items live with the code they constrain, in
[`authority/`](./authority/authority_test.go) and
[`effect/`](./effect/effect_test.go):

| # | Test | What it holds |
|---|---|---|
| 10 | `Test10_EffectfulSpeculationIsRejected` | `--attempts 3` on an effectful capability is rejected, **not clamped** |
| 11 | `Test11_Idempotency` | a retry after a mid-flight network failure derives the same key, so the action happens once |
| 11b | `Test11b_ReservationExecutesOnce` | the key claimed in a ref before executing; a repeat is refused and told the outcome |
| 11c | `Test11c_HoldsPreventTwoPendingOrdersExceedingTheLease` | a hold is counted against headroom, so two pending orders cannot both pass |
| 12 | `Test12_EnvelopeTightening` | a tightened ceiling binds before the next action, with no sync; loosening grants nothing |
| 12b | `Test12b_TighteningBelowWhatIsAlreadySpent` | a ceiling under the spend leaves zero headroom, and does not un-spend |
| 12c | `Test12c_RemovingACapabilityIsTheSharpestTightening` | a capability dropped from the envelope refuses rather than reading as unbounded |
| 12d | `Test12d_TighteningIsNotTheEnforcementMechanism` | records what local capping does *not* bound, so nobody mistakes it for a guarantee |
| 13 | `Test13_StaleStateBehaviour` | offline: propose yes, promote no, effectful yes within a lease |
| 13b | `Test13b_LeaseExclusivity` | leases never overlap and never sum past the envelope |
| 13c | `Test13c_StrandedLease` | reclaim is provisional; a lease with recorded spend is never reclaimed |
| 14 | `Test14_ReservationExpiry` | an unsettled hold releases headroom on a timer — and never releases the key |
| 15 | `Test15_NoSelfAuthorization` | a promote key is not a purchasing credential |
| 16 | `Test16_InterfaceHashBinding` | alias-only references are refused; same alias, different hash does not match |

Every numbered item in the spec's §9 is now covered.

Several of these exist, in the spec's words, "to keep it that way": the behaviour
is already correct, and the test is there so a later refactor cannot quietly make
it not. §9.2 in particular guards against somebody "fixing" duplicate attempts.

Test 5 is written to prove the *right* thing: after asserting that a
self-verified attempt is refused, it adds evidence from a different cell and
asserts the same promotion then succeeds — otherwise the refusal might have been
about something else quietly blocking it.

```sh
go test ./...
go test -race ./...
go test -coverpkg=./... ./...
```

### Integration tests against a real core

`varvigcli` and `authority` also have tests that drive the actual `varvig`
binary. They **skip** when one is not on `PATH`, so CI stays green without it,
and they run for anyone who has one:

```sh
go build -o /usr/local/bin/varvig ./cmd/varvig   # in a varvig/varvig checkout
go test -run Integration ./varvigcli/ ./authority/ ./effect/
```

These earn their keep. Every other test in that package pins a CLI format by
asserting against a fixture string, which proves the parser matches the fixture
and nothing about whether the fixture matches varvig. The first run of the
integration suite found a real bug the whole unit suite had passed over:
`UpdateRef` omitted its expected-old argument, which varvig reads as *set
unconditionally* rather than *must not exist* — so a real cell would have silently
overwritten attempt refs, the one thing the contract forbids. The Fake enforced
create-only correctly, and that is exactly how a fake stricter than reality hides
a bug.

The `authority` and `effect` integration tests answer the question no fake can:
whether a real core accepts `refs/envelopes/`, `refs/leases/` and
`refs/reservations/` at all, and whether create-only really is create-only there.
It does, and it is — they are ordinary refs under unreserved prefixes, which is
what makes the whole spend model deployable against today's core with no changes
to it.

## Build order

The spec's §10, and what shipped for each step:

| Step | Where |
|---|---|
| 1. Cell contract before any daemon code | `CELL.md`, `cell/` |
| 2. Single-cell loop, gated only, one model + one sandbox adapter | `loop/`, `inference/`, `sandbox/` |
| 3. Micro profile with `roles: [verify, build]` | `profile.Micro`, `loop.verifyPeerAttempts` |
| 4. Mini profile: attempting enabled, config only | `profile.Mini` |
| 5. Budget enforcement and halt behaviour | `budget/` |
| 6. `artifact-ref` production, cell-local retention | `artifact/`, `loop.recordArtifacts` |
| 7. Upstream sync, claims, pins — two cells, one upstream | `claim/`, `cmd/factory-demo` |
| 8. Partition and offline suite before anything depends on claim semantics | `conformance/` §9.2, §9.3 |
| 9. Promotion policy module, still gated — runs and logs without acting | `gate/`, `promote/` |
| 10. Agreement-rate measurement, per scope | `agreement/` |
| 11. `autonomous`, per-path, gated on the conditions, kill switch tested first | `promote/`, §9.7 |
| 12. Overseers, envelopes and leases | `authority/` |
| 13. Effectful capabilities, reservations, idempotency | `effect/` |

## Known gaps

The design notes are ahead of this code in three places, and one representation
choice here is worth flagging. They are listed rather than left to be
discovered, because a README that quietly omits something reads exactly like one
describing a decision against it. [CELL.md §11](./CELL.md) carries the same list
with the contract-level detail.

| Gap | What is missing |
|---|---|
| **No rendezvous set** (§3.0) | The loop takes a single `upstream` address. It is not a coordinator, but "any member may act as a rendezvous, several at once" is not implemented, so a factory does not yet keep working when that particular member is unreachable. |
| **No interface registry** (§2.1) | A capability reference already binds to the interface *hash*, which is the part that matters for safety — a ticket, a cell's configuration and a lease must all name the same hash before anything is ordered. What is missing is the registry the hash points into: interfaces published as varvig objects and resolvable by hash. |
| **No derived reputation** (§2.2) | Capability claims are advisory and standing should be derived from promotion history. Only the agreement-rate metric is derived today, and it is per scope rather than per cell. |
| **Money is a `float64`** | Amounts accumulate representation error — 1000 − 320 − 355.40 is 44.60000000000002 — and refusals round for display. No decision compares amounts for equality, so nothing turns on it today, but minor units are the right representation for a system that spends money. |

The first three are scope. The fourth is a representation choice worth fixing
before real money moves through it.

## Repository name

The design notes call this repo `varvig/factory`; it lives at
`varvig/varvig-factory`, matching the `varvig-connectors` convention. The Go
module is `github.com/varvig/varvig-factory`.

## License

Free software, licensed under the **GNU General Public License v3.0** — the same
license as varvig. See [`LICENSE`](./LICENSE) for the full text.

```
Copyright (C) 2026 varvig contributors

This program is free software: you can redistribute it and/or modify it under
the terms of the GNU General Public License as published by the Free Software
Foundation, either version 3 of the License, or (at your option) any later
version. This program is distributed in the hope that it will be useful, but
WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for more
details.
```
