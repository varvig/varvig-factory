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

Then run [`cmd/factory-simulator`](./cmd/factory-simulator/main.go) — a
committed program, not a scratch script, and the executable version of the
argument this README makes:

```sh
go run ./cmd/factory-simulator
```

Six phases against in-memory fakes. A Mini cell attempts and a Micro cell
independently verifies; gated promotion evaluates every condition and acts on
none of them; a partition where both cells attempt the same task and both
attempts survive reconnect; autonomous promotion, earned per scope, stopped two
different ways by the kill switch; and finally a disconnected cell refused a
promotion while it goes on spending its lease, reserving, settling above quote,
and being refused a second pending order and a self-authorized one; and last a
ticket driving a real order through the loop — placed by micro-b, the cell with
no model at all, because authority to spend is a lease and not a GPU. No varvig
binary, no GPU, no network.

It is called a simulator because it puts a factory through conditions —
partition, three days offline, an overseer tightening a ceiling mid-run — rather
than illustrating them, and every step is a real write through the real loop,
claim policy and authority arithmetic. It is worth being equally plain about
what it is not: **one scripted run, not a parameter space.** No flags, no
randomness, a clock the script advances itself, and each beat prints the
conclusion it just established. It simulates conditions; it does not let you
vary them.

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

The config it writes is *collapsed*: one repository serving both the
coordination and the project role, which is correct for a factory with a single
codebase. Set `factory_repo` and `factory_rendezvous` when there is a second
project — that is the point at which each project growing its own copy of what
the cell may spend stops being harmless. See "One factory repository, N project
repositories" below.

Run `varvig-factory` with no arguments for the full command list.

## Cell classes, and flat factories

**There are no tiers.** A cell class describes *capacity*; it is not a rank, and
it carries no authority. Scaling means more cells or bigger cells — never a
topology change.

| Cell class | Build and test capacity | Roles the template ships |
|---|---|---|
| **Micro** | small / commodity | `verify`, `build` — *not* `attempt` by default |
| **Mini** | accelerated or high-core single host | `attempt`, `verify`, `build` |
| **Medium** | Mini, plus a rendezvous set to sync with | `attempt`, `verify`, `build` |

**Class describes capacity, not inference.** Inference reaches a cell through an
executor that may be a hosted API, so it is a capability rather than a hardware
fact — which makes the two axes orthogonal, and makes the less obvious
combinations legitimate:

```go
profile.Micro(id).WithInference(hosted)  // a commodity host that authors through an API
profile.Mini(id).WithoutInference()      // a high-core host that only verifies and builds
```

The distinction that matters more than the class is **policy cell** (no model
configured) versus **inference cell** (one is). A policy cell is not degraded: it
verifies, builds, executes effectful capabilities and syncs, and the evidence it
produces is what licenses another cell's attempt to be promoted autonomously.
The cheapest hardware in the factory produces what makes the expensive hardware
trustworthy. Its one limit is that it cannot originate work.

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
only in the field values the config names; `profile.Wire` — the one
function that turns a config into a running cell — never reads the profile name,
and [a test](./profile/profile_test.go) reads its syntax tree to prove it. If a
class ever requires a branch in the code, the abstraction has failed.

The code matches: a cell syncs against a **set** of peers per repository kind,
contacts every member each pass, and shuffles the order so none is
systematically first. See "Rendezvous is a set" below.

### Micro's honest role

A CPU-local model authoring code will lose nearly every selection while still
consuming review attention — net negative. Micro is strong as a **verification
and build cell**: deterministic work, cheap, no model-quality problem, and it
makes the old-hardware story genuinely compelling rather than aspirational.

So Micro ships with `roles: ["build", "verify"]` and attempting is opt-in.

### An unreachable model is a decline, not a breakdown

A cell configured for a model whose runtime stops answering keeps running. It
syncs, verifies other cells' attempts, builds, executes effectful capabilities,
and declines the tickets it would have authored — saying which runtime declined
and why, once per pass. When the runtime comes back it resumes attempting, with
no restart and nothing to reconfigure.

This is worth spelling out because it used to be false: such a cell failed its
startup validation and did nothing at all, so one unavailable capability
disabled every available one. A cell whose model has gone away is not
misconfigured; it is a cell with less to offer this pass. Reachability is
therefore measured per pass and reaches claim policy as an input, where it
declines attempts and touches nothing else.

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

**Budgets are optional, and absence means unenforced.** A factory where nobody
configured spending simply works. That is worth stating plainly because this
code used to do the opposite: an unset cap read as a cap of zero, so a factory
with no budget configured refused every ticket and reported being out of money.
Most of what a cell does — builds, tests, verification, sync, local inference on
hardware already paid for — costs nothing external and should require no
overseer, no envelope and no lease.

Where a cap *is* set it is enforced strictly: attempts multiply cost, and a
disconnected cell claiming speculatively can burn budget on work that proves
duplicative. Making absence permissive does not make presence permissive.

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
| Ref | `refs/factory/envelopes/<overseer-id>` | `refs/factory/leases/<cell-id>/<capability>` |
| Which repository | the factory's coordination repo | the factory's coordination repo |
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

A cell needs an executor for a capability, and a lease **only if the capability
declares a cost model**. Holding neither means it declines the ticket rather
than claiming it and discovering the problem afterwards.

### Effectful and expensive are different things

`effectful` means **irreversible**, not costly. Turning on a light, moving an
arm, printing with filament already paid for, posting a message: every one
irreversible, every one free. Requiring a lease for those made an overseer and a
budget the price of admission for a factory that spends nothing — and refused
the whole class of physical work the design exists to reach.

So a capability declaring no cost model needs no lease, and still obeys every
other rule: one attempt, an idempotency key, authorization by a higher
principal, escalation instead of retry. Those rules exist because the action
cannot be undone, which has nothing to do with its price.

Two things keep that from becoming a hole:

- **An undeclared cost is refused as malformed**, never run unmetered. The
  moment "no cost model" means "no lease needed", omitting the cost model is the
  cheapest way to spend money unwatched — so the permission and its guard land
  together.
- **The envelope bounds quantity and rate**, not only spend, with no currency
  invented for the purpose: "at most 20 prints a day" needs none. For a free
  capability those ceilings are the *only* bound, which is why they had to start
  being enforced. Rate is measured from reservation refs, and an unmeasured
  history refuses rather than passing — an unmeasured history is not an empty
  one.

A ticket supplies a capability's identity; the cell's configuration supplies its
terms. Reading the cost model off the request would let a ticket declare a board
order free.

The vendor seam behind an effectful capability is `effect.Executor`, and it is
deliberately not the `cell.Executor` above: that one performs work for a cell,
this one causes something irreversible outside it. The reason for a seam at
all is sharper here than anywhere else — the alternative to a fake is a real board
order. Only
a **refusing** executor exists so far — pointing a cell at it proves the wiring
works, with the ticket claimed, quoted, authorized and reserved, and nothing
ordered.

### A connector is an effect executor that lives somewhere else

Worth stating plainly, because the word gets used as though it were a third
concept and it is not. There are **two** kinds of executor in this design:

| | Does | If it comes out wrong |
|---|---|---|
| `cell.Executor` | authors a change, runs a build, runs a test | run it again |
| `effect.Executor` | a physical or billable outcome | there is nothing to undo |

An `effect.Executor` can sit in either of two places, and that is a deployment
choice rather than a second seam:

| | Where the vendor code runs | How the cell reaches it |
|---|---|---|
| In-process | inside the cell binary | a direct call |
| **Connector** | a separate peer process | repository state — the cell holds no executor for that capability at all |

`loop/effectful.go` is literally that fork: if a connector serves the interface,
the cell reserves, writes an offer and stops; otherwise it takes its own
reservation and calls the executor. A connector is not a plugin, not a third
seam, and not a kind of capability.

**The deciding question is credentials, not physicality.** A capability whose
work happens on this machine — a pin, an arm, a printer — has nothing to leak and
is naturally in-process. One that reaches a service holding an account in your
name should be a connector: compiling it in means a rebuild per vendor *and* that
vendor's credentials in the cell process. That is why the connector shape is
modelled on how core does tracker bridges — it holds its own credentials, is
untrusted, and runs anywhere.

```
cell       offers    a reservation naming a capability
connector  takes     it by compare-and-swap, recording who it is
connector  executes  against the vendor
connector  reports   an order number and what it cost
cell       settles   the lease from that report
```

**The interface is repository state, not an ABI** — nothing to load, nothing to
link, no version to keep in step. Configuration says only `executor:
"connector"`; there is no vendor name in it and there never will be.

The last line is the one that must not move: **a connector reports, only the cell
spends.** That is the containment core applies to a bridge, which may sign a weak
attestation and never a strong one — the untrusted peer states a fact and the
trusted layer applies it under rules. Here the rule is the lease, so a connector
that lies about cost is bounded by an amount the overseer chose: a settlement
beyond the allocation is refused outright, and a test asserts it.

A connector cannot create a reservation, raise a lease, widen an envelope,
settle, or take work another connector holds. Two racing produce one holder and
one refusal, on varvig's ordinary ref CAS — verified against a real core, because
against the in-memory fake that CAS is a map with a mutex and only the real thing
can say they agree.

Passing the take deadline does **not** hand the work to somebody else: by then the
holder may have reached the vendor. A stale claim escalates. And a connector that
does not know **reports nothing** — "we never heard back" is not a report, so a
success with no reference and a rejection with no refusal are both refused.

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
reserve  ->  refs/factory/reservations/<cell-id>/<key>  = pending
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

## Robes carry no authority

Capability = can do. Robe = responsible for. There are four — Ambassador,
Provisioner, Procurement, Monitor — and the obvious implementation is the wrong
one. A robe looks like a permission, and building it that way means a key per
robe, a trust-store edit every time one moves, and a lifecycle to revoke one.

It is a claim-policy input. What a cell may do comes from the single scope it
was granted at enrolment, and that bundle is identical whether it wears
everything or nothing. Check each robe and none needs more: Monitor only reads;
Ambassador creates tickets, which any cell may propose; Procurement proposes
Profiles, which is unprivileged; Provisioner acts, and what it needs is
infrastructure credentials and a lease, not elevated repository rights.

`robe.Grants` exists to be called rather than to be useful — its whole body is a
nil return, and the conformance vector calls it for every combination of robes.
The rule here is easiest to break by addition: each robe has an obvious thing it
"should" be allowed to do, and granting it there looks like a small convenience.

Robe assignment is a **derived projection** over the replicated capabilities
objects. There is no registry, no tombstone and no cleanup job: a cell that
stops publishing stops wearing anything. And no cell can give another a robe —
that would be authoritative assignment, needing consensus this design does not
build.

### The provisioner is the one to be careful about

A provisioned cell's ownership roots to the **owner**, never to the provisioner,
and the assertion is on the key chain rather than on configuration. Configuration
can say anything: a provisioner minting cells loyal to itself would write exactly
the same config as one that did not. What differs is whether its key could have
written the new cell's authority at all — so a provisioner is granted nothing
that reaches another cell's namespace, and a compromised one forks the trust root
only if somebody first handed it rights it has no reason to hold.

### Resolution and procurement are different questions

They divide on time, not on scope:

| | Question | Availability |
|---|---|---|
| Resolution | which existing provider fits | always, answers now |
| Procurement | what should we acquire | its own cadence, needs judgment |

Resolution is a **library, not a service and not a robe**. It runs locally on
every cell over replicated state, which is what makes it compatible with a flat
factory: every cell computes the same answer from the same refs, so there is
nothing to elect and nothing to be unavailable. Matching is on the interface
hash, never the alias — two factories may use one alias for different
interfaces, and a resolver that matched names would quietly pair a requirement
with a provider implementing something else.

**Resolution never blocks on procurement.** A cell that finds no provider
proceeds or declines immediately. Waiting would make a deterministic local
function depend on a robe that may not exist in this factory at all.

Procurement spends money to create things that spend money, which is why it is
build-order last and why its guardrails are refusals rather than advice. It
**caps total cells, not only total spend**: a spend ceiling alone does not bound
growth, because a factory can sit just under its ceiling while the number of
things drawing on it climbs. The failure mode is drift nobody notices, and a
count is the thing that notices. The cap is checked against the cells that exist
now, including any a previous round created, so a factory cannot bootstrap past
its ceiling by creating cells that create cells.

And **procurement proposes; it never provisions.** `Procurement.Provision`
exists and always errors, so the separation is legible in the code rather than
only in prose — the same split drawn between the principal deciding to spend and
the one executing.

### Decision tasks

The main loop calls no model. A decision task is the bounded exception: one
inference call when deterministic policy genuinely cannot decide, down the
ordinary executor path, recorded as an attempt. Wanting one every turn means the
policy layer is underspecified, not that the cell needs intelligence.

Four guardrails, and three of the four fail *open* — a broken one does not
throw, it quietly lets a cell think more, or deeper, or with authority it never
had:

- **A separate budget line.** No meta line configured is a refusal, not a
  fallback to the main one; an exhausted line means the cell decides
  deterministically rather than borrowing from the budget meant for doing the
  work.
- **No nesting.** One level, hard stop. The recursion has no natural floor and
  every level looks locally reasonable.
- **An unreachable executor degrades to policy, never stalls.** Every question
  names a fallback that is one of its own options, so a model that is down,
  refuses, or answers something outside the options still leaves the cell able
  to act.
- **No authority.** A question carries no amount, no principal, no permission,
  and its answers are a closed set policy already ruled in. An open answer would
  let a model propose something nothing authorized.

### External input is marked, never inferred

An Ambassador turns an outside request into a ticket and marks it:

```
factory-origin: external
```

Its own directive rather than a key on `factory-requires:`, because it is not a
requirement — a ticket asking for nothing in particular can still have arrived
from outside, and that is exactly the one worth knowing about.

Whether to act on the marking is an operator's call, so a cell can be configured
to decline external work and by default is not. What is not optional is where
the check runs: **above** the effectful split, so a cell that declines external
work declines all of it. An external request that orders a physical thing is the
case that matters most, not one to fall through to a different branch.

## Seams

**Two seams, not four.** Everything hardware- or vendor-shaped lives behind them,
so neither varvig nor the cell loop learns about CUDA, quantization or container
runtimes.

| Seam | Package | Implementations |
|---|---|---|
| **`cell.Executor`** — anything that performs work | contract in [`cell/`](./cell), implementations in [`executor/`](./executor) | authoring: `http` (ollama, vLLM, llama.cpp server, hosted APIs), `command` (llama.cpp CLI, any local wrapper), `harness` (Claude Code or anything shaped like it), `none`. checking: `subprocess`, `container`, `nix` |
| **Artifact store** — the only other adapter, a sink | [`artifact/`](./artifact) | local CAS, plus a command-driven remote for OCI registries and S3-compatible stores |

This used to be a model-runtime adapter *and* a build-sandbox adapter. They had
one shape — name yourself, describe your environment, do a unit of work — and
keeping them apart made "how does a cell run a model" a different question from
"how does a cell run a test". A harness would have been a third answer to a
question that should only have one, which is what "two things owning the same
concern" costs.

So a Claude Code harness is an executor that declares `loops` and
`tools_attached`. A CNC machine is an executor that declares `effectful`. Neither
needs an interface, a package, or a config section of its own — and that is the
test of whether the seam is doing its job.

### `cell.Executor` is not `effect.Executor`

Two seams here have a claim to the word, and they are not variations on one
idea:

| | Does | If it goes wrong |
|---|---|---|
| `cell.Executor` | work *for* a cell — authors a change, runs a test | run it again |
| `effect.Executor` | a physical or billable outcome — anything *irreversible* | there is nothing to undo |

That second row is the whole reason the `effect` package exists: the lease, the
reservation, the higher principal, the refusal to speculate. A `cell.Executor`
needs none of it, because getting its work wrong costs another run.

The contract lives in [`cell/`](./cell) rather than in `executor/` so the names
carry the distinction instead of the reader having to. It sits with the
environment fragment it is obliged to produce, and an executor written outside
this module imports one package rather than two. The implementations, and the
`Authoring` / `Checking` interfaces that say which kind of work each one takes,
stay in [`executor/`](./executor).

A guard holds the line: the two interfaces must share no method. The way this
erodes is convergence, not renaming — `effect.Executor` gets a `Name()` for
better log lines, then `Properties()` because it is useful, and one morning
merging them looks reasonable.

Note that an executor may *declare* `effectful` as a property. That is not
authority to act: it says what kind of thing the executor is, while what it may
do still comes from a lease.

### A harness is just an executor, which was the whole bet

`§4.5`'s wiring table has two rows, and until recently only one existed:
everything ran in the cell's process, answered once, and returned text. A
harness does none of those. It gets a directory, edits files in it, loops on
what it sees, and talks to varvig over a socket while it works.

The fold's claim was that adding one should need no new interface, no new
package and no new config kind. It held, with two additions that are rows in a
table rather than concepts:

- `Request` gained `Dir` and `Socket`, because the wiring an executor gets is
  proportional to what it declares and nothing before declared these.
- Properties gained `edits_in_place`, because "my response is a report" and "my
  response is content to apply" are different contracts and a caller cannot
  guess which one it holds.

Neither is a case in a switch. The loop reads properties; it still cannot ask
what an executor *is*, and the guard still fails the build on any attempt to.

**`edits_in_place` is the one that bites silently.** A harness edits `src/a.go`
and then describes the edit in its summary, as any harness reporting its work
naturally would — so a caller parsing that summary as content overwrites the file
with the prose description of it, and the tests then measure something nobody
wrote. There is a vector for exactly this, with a report written in the shape the
parser accepts.

For such an executor, "did anything happen" is answered by asking varvig what
changed. It has to be asked: `varvig commit` on a clean tree succeeds and records
an **empty change**, so committing blind would fill the speculation pool with
empty candidates that other cells would then score. What it changed is held to
the ticket's declared write set exactly as parsed output is.

### The tool channel, and what it bounds

**Authority attaches to the tool channel, never to the model.** An executor
declaring `tools_attached` gets the task's MCP socket — scoped and propose-only,
exactly like the credential it belongs to — so whatever it asks varvig to do
through it is bounded by what varvig already granted the task.

A tool-taking executor handed no socket is **refused**, not run toolless: it
would do its work by guessing at a repository it cannot read, and bill for it.
An executor declaring no tools is handed no socket even when one exists, because
a credential nobody decided to give it is not a convenience.

Core serves a per-task socket only while `varvig daemon` is running, so a cell
configured with a tool-taking executor needs one. A cell whose executors take no
tools needs nothing and notices nothing.

And note what running in a checkout is *not*: a sandbox. The subprocess runs
with the cell's user and the cell's filesystem access, confined to the checkout
by convention and its own behaviour. The confinement that is real is the socket.

### Properties, not kinds

An executor declares what it is *like*, never what it *is*: `loops`,
`tools_attached`, `deterministic`, `consumes_lease`, `effectful`. The wiring an
executor gets is proportional to what it declares, so a new shape changes a
table of behaviour rather than a list of cases.

[A guard](./guard/guard_test.go) fails the build if anything outside the package
starts asking. The mistake it catches arrives with the first harness: writing
`if h, ok := e.(*executor.Harness)` works for the executor in front of you and
not for the one somebody adds next, and a list of cases growing one entry per
vendor is the four-adapter model returning under a different name.

`deterministic` is the load-bearing property. A checking executor is
deterministic, which is what lets one cell's evidence license another's attempt
and why a transaction may safely re-run it; an authoring executor is not, which
is why one must never run inside anything that retries.

### Describing the environment

Every executor reports a **deterministic environment fragment**, and the fragment
is a *measurement*, not a configured claim: the HTTP executor probes the server
for its version, the checking executor runs its version probes **through its own
wrapper** so a container cell reports the toolchain inside the container rather
than the host's. One that cannot describe itself reproducibly returns
`cell.ErrIndescribable` — emitting a guessed environment would make every downstream
cross-cell comparison a comparison of guesses.

Two executors disagreeing about the machine they both run on is a hard error
rather than a last-writer-wins merge. If the checking executor says Go 1.24.7 and
the authoring one says Go 1.22, one of them is wrong, and either value produces
an environment hash that certifies a fiction.

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
CELL.md                 the cell contract — normative, read this first
cell/                   the contract in code: names, capabilities, evidence,
                        environment + its hash, claims, robes, and the
                        cell.Executor seam. No dependencies on anything.
varvigcli/              the Varvig interface + an Exec adapter over the public CLI,
                        the FactoryRepo/ProjectRepo handles that keep the two
                        repository kinds apart, and an in-memory Fake that models
                        refs-with-CAS, notes, the speculation pool and a
                        partitionable upstream
executor/               implementations of cell.Executor — authoring (model) and
                        checking (build, test), folded into one seam. The
                        contract itself lives in cell/, so that cell.Executor is
                        visibly not effect.Executor
artifact/               artifact-store seam
budget/                 spend caps, halt behaviour, storage-pressure relief
authority/              envelopes and leases: shared ceilings versus exclusive
                        allocations, what a stale view still permits, and the refs
                        they live in — all of it in the coordination repo
iface/                  the interface registry: schemas as objects, resolvable by
                        the hash a capability names
reputation/             per-cell standing, derived from what was promoted
effect/                 effectful, non-regenerable capabilities — the refusals,
                        reserve/execute/settle over a reservation ref, and the
                        effect.Executor seam in both its locations: in-process
                        (a refusing default and a counting fake) and the
                        connector exchange, where the executor is another process
robe/                   responsibilities a cell wears, the projection that derives
                        who wears what, resolution (which provider fits) and
                        procurement (what to acquire) — none of it authority
decide/                 the decision task: one bounded inference call when policy
                        genuinely cannot decide, with its four guardrails
claim/                  claim policy: should this cell attempt this ticket?
loop/                   the ten-step cell loop, and verification of peer attempts
gate/                   the wasm promotion-policy module interface
agreement/              the promotion-agreement metric, per scope
promote/                both modes, the five conditions, the kill switch
profile/                micro/mini/medium as configuration, and the wiring
conformance/            the spec's §9 tests for the single-cell contract (the
                        authority ones live with the packages they constrain)
guard/                  build-failing guards: no second scheduler, no branching
                        on cell class, no third-party dependencies
cmd/varvig-factory/     the cell binary
cmd/factory-simulator/  a factory put through six conditions, end to end
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

## A connector is any process that can run the binary

Which is the point of the shape: an effect executor that lives outside the cell
(see [above](#a-connector-is-an-effect-executor-that-lives-somewhere-else)) needs
no Go, no plugin ABI and no rebuild — only these three verbs.

The connector protocol is repository state: the cell offers, a connector takes by
compare-and-swap, executes, and reports; the cell settles.

```sh
varvig-factory connector awaiting --alias pcb-fabrication@1     # what could I serve?
varvig-factory connector take --cell mini-a --key $KEY --connector fab-a
# ... do the work ...
varvig-factory connector report --cell mini-a --key $KEY --connector fab-a \
    --happened true --ref PO-90210 --actual 355.40
```

JSON in, JSON out, so the process on the other end can be a shell script. Until
these existed a vendor had to be compiled into the factory binary — which is the
rebuild the protocol was built to avoid, so the protocol was not yet delivering
the thing it was for.

**A connector reports; only the cell spends.** Converting a hold into settled
spend needs the lease, the lease lives in the coordination replica, and a
connector is never handed one — a vendor that could write the lease could write
its own payment. `take` and `report` need only the project replica, and the cell
settles against the lease on its next pass. That asymmetry is the design, not a
missing feature.

**Exactly one connector wins a take.** It is varvig's ordinary ref
compare-and-swap and nothing else — no lock, no lease, no coordinator. A
connector that loses is told so and must not execute, and that refusal is the
whole mechanism standing between two connectors and two identical orders.

**`--happened` has no default.** The two answers are not near-misses of each
other: one converts a hold into spend and the other gives it back. A flag whose
absence meant either would make the most consequential field in the protocol the
easiest one to leave out. A timeout is neither — a connector that did not hear
back reports nothing and lets the reservation stand as pending, which is the
honest record of an unknown outcome.

## The interface registry

A capability reference binds to the interface **hash**, and that hash is now the
id of a stored schema object. Publishing writes the canonical schema; the id that
comes back *is* the hash. So resolving is an ordinary blob read at the same id
the capability names, and the registry cannot disagree with the hash it is keyed
by.

```sh
varvig-factory interfaces publish --alias pcb-fabrication@1 --schema board.json
varvig-factory interfaces list
varvig-factory interfaces show --alias pcb-fabrication@1
```

**An interface the registry does not hold is refused before the money.** A hash
is enough to tell two interfaces apart, which is what matching needs, and not
enough to say what an action requires. Acting on a hash nobody published means
ordering a shape no one in the factory can describe — and the moment to discover
that is before the order, not in the invoice.

The alias is a convenience and never authority. Re-pointing one changes what a
human types and nothing about what a lease bounds or what an idempotency key
covers, because the hash is in the key.

## Reputation is derived, and deliberately not acted on

A cell's capabilities object is a *claim* — nothing checks it, and that is fine,
because a cell that lies about its model produces worse work and the work is what
gets scored. What was missing was anyone scoring per cell.

```
varvig-factory reputation
mini-a     2 of 2 attempts promoted across 2 task(s)  (100%)
micro-b    0 of 2 attempts promoted across 2 task(s)  (0%)
```

Standing is: of the attempts a cell made at tasks later promoted, how many were
the attempt that moved. Credit follows the **change**, not whoever moved the ref
— in a flat factory the cell that promotes is usually not the cell that produced
the work. Attempts at tasks nobody has promoted are excluded, because unfinished
is not failed. A cell with no record has no rate rather than a rate of zero:
those are opposite answers, and a newcomer reading as the worst possible cell is
how a metric becomes a barrier to entry.

**Nothing in the loop reads it.** A cell consulting reputation to decide whether
to attempt would be ordering work by a quality judgement, which is varvig's job
— and it would compound, since a cell that attempts less has less record. The
number is for whoever can see the whole picture.

## One factory repository, N project repositories

A cell holds full replicas of **two kinds** of repository — not a private repo
of its own, and not a shared worker against one repo.

| Repository | Scope | Holds |
|---|---|---|
| **Coordination**, one per factory | the factory | membership and `allowed_keys`, cell capability objects, interface schemas, overseer envelopes, per-cell leases |
| **Project**, one per codebase | one codebase | tickets, claims, attempts, evidence, environment descriptors, `artifact-ref` objects, effectful reservations |

The split is about authority, not tidiness. Envelopes and leases answer "what may
this cell spend", and that question has exactly one authoritative answer per
factory. Put them in the project repos and every project grows its own plausible
copy: each looks correct on its own, the sum exceeds the envelope, and nothing in
the system is positioned to notice.

**The two kinds are distinct Go types**, and the functions that write a lease
take the coordination one. Both are reached through the same `Varvig` surface, so
nothing but the type system stops a caller passing either — and the one mistake
that matters here is discovered while reconciling a bill. Handing a project
replica to something that writes a lease is a compile error.

For a factory with a single codebase, one repository may serve both roles
(`varvigcli.Collapsed`). The roles stay two even then, so a call site that says
which repository it means keeps saying so when a second project shows up.

### Two writes, and the order is the safety property

A reservation lives in a project repo and the lease it spends from lives in the
coordination repo, so every settlement is two writes to two repositories with no
shared transaction — not by oversight, but because two independent
compare-and-swaps cannot have one. The lease is written first, always:

| Second write never lands | What is left | Recovery |
|---|---|---|
| after a hold is taken | headroom held for a key nobody claimed | the expiry returns it |
| after spend is recorded | the lease over-reports this cell's spend | an overseer reading the lease corrects it |
| after a hold is released | a record still open against a lease that no longer holds for it | the double-release guard makes a retry loud; a principal resolves it |

The unreachable state is a reservation saying an irreversible action is settled
against a lease with no record of paying for it. Every other failure here is
recoverable; that one is not, so the ordering exists to buy exactly it. Three
tests take one replica's writes away mid-settlement and assert the surviving
state — reversing the order makes two of them fail with the double-spend
condition named in the message.

### Reachability now answers two questions

One repository made one reachability answer serve both. Two make them separate,
and they were never the same question:

- **The project peers** decide whether the cell is looking at current *work*.
  Unreachable is the offline mode: a tighter budget, claims marked offline, work
  continuing from the view it has.
- **The coordination peers** decide whether the cell's *trust state* is current.
  Membership and `allowed_keys` live there, so it is those peers, and only
  those, which the promotion gate reads.

A cell cut off from its project peers may still promote what it already holds. A
cell cut off from the coordination peers may not — it cannot know who is still
allowed to sign.

## Rendezvous is a set, and that is what makes a factory live

A cell syncs against a **set** of peers per repository kind. Any member may
serve, several at once — not a role, not a coordinator, not an upstream (§3.0).
An empty set is a single-cell deployment: nothing to be disconnected from, and
so neither offline nor stale.

Three properties separate a mesh from a fallback list:

| Property | Why |
|---|---|
| **Every member is contacted each pass**, in both directions | Peer B may hold a lease or an attempt A has never seen. Taking A's answer and stopping relies on A to relay the rest — which makes A a coordinator however the config describes it. |
| **The order is shuffled** | Reserved-ref replication takes what the cell lacks and *reports* rather than overwrites on a conflict, so on a contested claim the peer contacted first is the one whose version is adopted. A fixed order hands that to whoever was typed in first. |
| **Reached ≠ answered** | A peer can be reached and still refuse something: a head push whose compare-and-swap loses a race, a namespace that would not transfer. Those peers were reached — authority and evidence arrived — and counting that as unreachable would mark trust stale for a reason unrelated to trust. |

The test that matters most runs three real `varvig serve` processes, each
holding a ticket only it knows about, and asserts the cell ends the pass holding
all three and having reported its spend to every one of them. Making the
implementation a fallback list makes it fail with *"a set that stops at the
first answer is a fallback list, not a mesh"*.

Two varvig changes make the set work rather than merely exist: a refused branch
no longer suppresses the notes and reserved refs travelling alongside it
(`FEDERATION.md` §6), and remote-tracking refs are per peer, so a push leases
against the peer it is pushing to rather than whichever peer was fetched last
(§7). Without the first, authority reached exactly one member; without the
second, only one member could accept a head push.

## One namespace root

Every Factory ref nests under `refs/factory/` — capabilities, attempts, claims,
envelopes, leases, reservations. The reason is that the *shapes* are generic and
the *semantics* are not: any multi-worker system wants something called a claim,
but a Factory claim is advisory, expiring, never exclusive, and says nothing at
all across a partition. A system that reasonably made claims exclusive would be
writing a different meaning under the same name, and no reader could tell them
apart.

That collision has already happened once between these two projects: varvig's
speculation store calls its candidates "attempt-states", stored as files under
`.varvig/spec/`, while a Factory attempt is a ref with different immutability
rules. Two concepts, one word.

**What makes these semantics reusable is the contract being written down, not
the prefix being shared.** Another layer implementing leases under its own root
has benefited from [CELL.md §8.1](./CELL.md); one writing into this root with its
own lease model has created a hazard. varvig's core reserves the same root and
asserts the nesting from its side, and a test here asserts it from this one — so
a Factory concern added later cannot land outside the root by accident.

`refs/pins/` is the one exception, and it is not Factory's: it is varvig's
federation primitive, acted on by varvig's GC root walk and pin handlers. A cell
requests retention there without owning the namespace.

## Core decides the order

Which work is most valuable is a repository-wide judgement made from recorded
decisions, and core already learns it: `internal/score` fits a linear scorer
from approve/veto history, backtests it, and reports the agreement a reviewer
reads before promoting the scorer. `varvig tickets rank` is that ordering.

So the cell asks rather than deciding. A cell computing its own ordering would
be a second scheduler with a strictly worse view — one machine's opinion where
core sees the whole history.

Two properties of the wiring, both deliberate:

- **It reorders and never filters.** `tickets rank` covers only *scoped*
  tickets, so anything core does not mention keeps its place behind the ranked
  ones. A ticket becoming invisible because it was unscoped would silently change
  what the cell does, and the claim policy already has a clear refusal for that.
- **A failure to rank is not a failure to work.** The cell logs it and proceeds
  in listed order. An ordering hint that cannot be fetched should cost
  throughput and nothing else — including against a core too old to have the
  verb.

Ranked ids come back in core's short form, so matching them to full ticket ids
is the adapter's job, and an integration test pins that against the real binary.
If core changes how it abbreviates, the loop would otherwise stop reordering
silently — which looks exactly like a correctly ordered cell.

## Claims

A claim is a TTL'd ref at `refs/factory/claims/<cell-id>/<task-id>`. Three rules, and
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
| 24 | `Test24_UndeclaredCostRejected` | a capability with a cost and no cost model is refused as malformed, never run unmetered |
| 25 | `Test25_FreeEffectfulCapability` | no cost model means no lease, and still irreversible |
| 26 | `Test26_NonMonetaryCeilings` | an envelope bounds quantity and rate, not only spend |

The rest sit with the behaviour they describe — the model-free loop in
[`conformance/`](./conformance/conformance_test.go), robes and growth in
[`robe/`](./robe/robe_test.go), decision tasks in
[`decide/`](./decide/decide_test.go), external origin in
[`claim/`](./claim/claim_test.go):

| # | Test | What it holds |
|---|---|---|
| 17 | `Test17_ModelFreeLoop` | a cell with no model is first-class: it declines attempts and does everything else |
| 18 | `Test18_DecisionTaskIsolation` | all four guardrails — separate line, no nesting, degrades to policy, no authority |
| 19 | `Test19_ResolutionDeterminism` | every cell resolves the same providers, in the same order, from the same state |
| 20 | `Test20_ProvisionedOwnership` | a provisioned cell roots to the owner — asserted on the key chain, not on configuration |
| 20b | `Test20b_RobesCarryNoAuthority` | no robe, and no combination of them, widens what a cell may do |
| 21 | `Test21_GrowthBound` | procurement cannot pass the cell cap or the envelope, including via cells that create cells |
| 22 | `Test22_ExternallyOriginatedMarking` | the Ambassador's marking survives the round trip, is never inferred, and is read above the effectful split |
| 23 | `Test23_NoBudgetFactory` | a factory with no budget configured works; absence is unenforced, not zero |

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
whether a real core accepts `refs/factory/envelopes/`, `refs/factory/leases/` and
`refs/factory/reservations/` at all, and whether create-only really is create-only there.
It does, and it is — they are ordinary refs under unreserved prefixes, which is
what makes the whole spend model deployable against today's core with no changes
to it.

## Build order

The spec's §10, and what shipped for each step:

| Step | Where |
|---|---|
| 1. Cell contract before any daemon code | `CELL.md`, `cell/` |
| 2. Single-cell loop, gated only, one model + one sandbox adapter | `loop/`, `executor/` (then two packages, now one) |
| 3. Micro profile with `roles: [verify, build]` | `profile.Micro`, `loop.verifyPeerAttempts` |
| 4. Mini profile: attempting enabled, config only | `profile.Mini` |
| 5. Budget enforcement and halt behaviour | `budget/` |
| 6. `artifact-ref` production, cell-local retention | `artifact/`, `loop.recordArtifacts` |
| 7. Upstream sync, claims, pins — two cells, one upstream | `claim/`, `cmd/factory-simulator` |
| 8. Partition and offline suite before anything depends on claim semantics | `conformance/` §9.2, §9.3 |
| 9. Promotion policy module, still gated — runs and logs without acting | `gate/`, `promote/` |
| 10. Agreement-rate measurement, per scope | `agreement/` |
| 11. `autonomous`, per-path, gated on the conditions, kill switch tested first | `promote/`, §9.7 |
| 12. Overseers, envelopes and leases | `authority/` |
| 13. Effectful capabilities, reservations, idempotency | `effect/` |
| 14. Freshness in promotion; the higher principal need not be human | `promote/`, `authority/` |
| 15. One executor seam, properties rather than kinds | `executor/` |
| 16. Robes, and the externally-originated marking | `robe/`, `cell.Robe`, `claim/` |
| 17. Resolution: which existing provider fits | `robe/resolve.go` |
| 18. Decision tasks | `decide/` |
| 19. Procurement: what the factory should acquire | `robe/procure.go` |
| 20. The harness executor — §4.5's second wiring row | `executor/harness.go`, `cell.ExecutorProperties.EditsInPlace` |

## Known gaps

The design notes are ahead of this code in three places, and one representation
choice here is worth flagging. They are listed rather than left to be
discovered, because a README that quietly omits something reads exactly like one
describing a decision against it. [CELL.md §11](./CELL.md) carries the same list
with the contract-level detail.

| Gap | What is missing |
|---|---|
| **No connector implementations** | The protocol is reachable now — `connector awaiting`/`take`/`report` mean a vendor is any process that can run the binary — but nothing ships that speaks it, and every effectful path is still exercised against a counting fake. |
| **`varvig update-ref` accepts a dangling object** (varvig) | A ref can be pointed at an object the repository does not have. Caught downstream by a loud transfer failure; better refused at the write. |

The first is the honest state of the vendor side: the door exists and nobody has
walked through it yet. The second is varvig's to fix.

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
