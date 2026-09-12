# The Cell Contract

*Normative. Version 10* — adds robes and the responsibilities that carry no authority (§3.4), externally-originated tickets (§3.5) and decision tasks (§5.1); folds the model-runtime and build-sandbox adapters into one executor seam (§6), separates cell class from inference and makes an unreachable runtime a decline rather than a refusal to start (§3), makes budgets optional and `effectful` independent of cost (§8.0), enforces the envelope's quantity and rate ceilings, adds the interface registry and derived reputation, counts money in minor units (§8.1), adds rendezvous sets and the repository split (§2.1), authority (§8.1),
effectful capabilities (§8.2), and the implementation status in §11. Section references in the form §N.N refer
to `FACTORY.md` (Design Notes VIII) unless another document is named.

This is the interoperability surface of a Factory cell — the part that is
expensive to retrofit once cells exist and have written state (§2). It is
written down before the daemon so that a Micro cell built today can join a
federation built later.

Everything here is either an on-disk shape, a ref name, or a hash rule. Nothing
here is a process, an RPC, or a schedule. A cell that honours this document
federates; how it is implemented internally is its own business.

**There is no central Factory.** A peer is a varvig peer, nothing more — it runs
no Factory process, holds no queue, and issues no RPCs (§1.1), and no member of a
factory is designated as its coordinator (§3.0). Every obligation below is
discharged by writing repository state.

An **overseer** (§8.1) is the one asymmetry, and it is an asymmetry of *key
scope*, not of topology: an overseer grants authority to spend, and sits nowhere
in particular on the network.

---

## 1. Identity

A cell has a **cell id**: a short, stable, lowercase name matching
`^[a-z0-9][a-z0-9-]{0,62}$`. It appears in every ref this document names, so it
is chosen once and never changed — renaming a cell orphans its claims and
attempts.

A cell has a **peer keypair** (varvig identity) and an entry in the repository
trust store — `.varvig.d/allowed_keys`, which is core's actual path; `.varvig/`
is skipped by `write-tree` and is not where the trust store lives — scoped by
path and rights:

```
# fingerprint       name          scope              rights
SHA256:fT8kLm…      mini-a        src/                propose
SHA256:fT8kLm…      factory-prod  src/generated/      promote
```

A cell that only proposes needs `propose`. A cell configured for autonomous
promotion needs `promote`, **scoped to the paths it may promote and nothing
more** (§6.1). A factory key with `promote` at `/` is an unattended root
credential; the trust store already has per-path granularity, so there is no
excuse for taking it.

Revoking a cell is deleting its line. That is the federation-wide kill switch
(§6.5) and it must remain sufficient on its own.

---

## 2. Ref and note namespaces

All Factory state lives under these names. They are chosen now, for the same
reason varvig reserved its own namespaces before first run: retrofitting
identity is the one thing that cannot be done afterwards.

| Name | What it holds |
|---|---|
| `refs/factory/cells/<cell-id>/capabilities` | The cell's static capabilities object (§3) |
| `refs/factory/attempts/<cell-id>/<task-id>/<n>` | One immutable attempt, `n` counting from 1 |
| `refs/factory/claims/<cell-id>/<task-id>` | An advisory, TTL'd claim (§5) |
| `refs/pins/<cell-id>/…` | Retention requests — varvig's own pin namespace (`FEDERATION.md` §4) |
| `refs/factory/envelopes/<overseer-id>` | The spend ceilings one overseer set, shared across its cells (§8.1) |
| `refs/factory/leases/<cell-id>/<capability>` | One exclusive allocation drawn from an envelope (§8.1) |
| `refs/factory/reservations/<cell-id>/<idempotency-key>` | An effectful action's reservation, keyed by its derived idempotency key (§8.2) |
| `refs/factory/interfaces/<hex alias>` | The interface registry: an alias pointing at the schema object whose id is the interface hash (§8.2) |
| note namespace `factory/evidence` | Evidence for an attempt (§4) |
| note namespace `factory/environment` | The environment descriptor an evidence record was produced in (§4.2) |
| note namespace `factory/artifact` | *Legacy.* `artifact-ref` records, for a core without `tickets attach-artifact` (§7) |
| note namespace `factory/agreement` | Promotion-agreement observations, per scope (§8) |
| note namespace `factory/effect` | The reservation record for an effectful action taken against a ticket (§8.2) |

`<task-id>` is the varvig ticket id — the genesis intent revision hash, stable
forever (`TICKETS.md` §1.2). Factory does not mint its own task identity.

**Every Factory ref nests under one root, `refs/factory/`.** The reason is that
the *shapes* here are generic and the *semantics* are not. Any multi-worker
system wants something called a claim; a Factory claim is advisory, expiring,
never exclusive, and says nothing at all across a partition. A system that
reasonably made claims *exclusive* would be writing a different meaning under
the same name, and no reader could tell the two apart.

That collision has already happened once between these two projects: varvig's
speculation store calls its candidates "attempt-states", stored as files under
`.varvig/spec/`, while a Factory attempt is a ref with different immutability
rules. Two concepts, one word. Nesting at least says whose.

What makes these semantics reusable is *this document*, not a shared prefix:
another layer implementing leases under its own root has benefited from the
argument in §8.1, while one writing into this root with its own lease model has
created a hazard. varvig's core reserves the same root and asserts the nesting
from its side, so the two agree by construction rather than by someone
remembering.

`refs/pins/` is the one deliberate exception, and it is not Factory's: it is
varvig's federation primitive, acted on by varvig's own GC root walk and pin
handlers. A cell requests retention there without owning the namespace.

Two rules about these names:

- **Attempt refs are immutable.** An attempt ref is created once and never
  moved. A second attempt at the same task is `…/2`, not a new value at `…/1`.
  This is what makes "both attempts survive reconnect" (§9.2) a property of the
  naming rather than of a merge algorithm.
- **Notes attach to the attempt's change hash**, never to a ref. Evidence
  bound to a ref would silently re-target; bound to a hash it stays attached to
  the thing that was actually measured.

---

### 2.1 Two repositories, and which state lives where

A factory holds **one coordination repository** and **N project
repositories**. A cell holds a full replica of the coordination repo plus a
replica of each project it works on — not a private repo of its own, and not a
shared worker against a single repo.

| Repository | Scope | Holds |
|---|---|---|
| **Coordination** | The factory | Membership and `allowed_keys`; cell capability objects; interface schemas; overseer envelopes; per-cell budget leases |
| **Project** | One codebase | Tickets and intents; claims, attempts, evidence, environment descriptors; `artifact-ref` objects; effectful reservations |

A cell replicates exactly one coordination repo, and whichever project repos it
works on.

**Why split.** Envelopes and leases answer "what may this cell spend", and that
question has exactly one authoritative answer per factory. Put them in the
project repos and every project grows its own plausible copy; the sum exceeds
the envelope and nothing in the system is in a position to notice. One factory
repo, N project repos, one place to look.

**Which `allowed_keys`.** Both repositories have one, and they are not the same
list. The coordination repo's says who is a cell in this factory. Each project
repo's says who may promote in that codebase. A cell can be a member in good
standing and still not be trusted to move a particular branch.

**Cross-repo binding.** A reservation in a project repo references its lease in
the coordination repo by hash. A cell must hold both replicas well enough to
resolve that reference before acting. For the lease, "well enough" means
*holding it at all* — leases are exclusive, so a stale one is safe (§6.6), and
requiring freshness here would take back §4.3b's guarantee that effectful action
inside a lease needs no connectivity.

**Two writes, one order.** Because the reservation and the lease live in
different repositories, every settlement is two writes with no shared
transaction — not by oversight but in principle. The lease is written first,
always. §8.2 states the rule and what each failure window leaves behind.

**Rendezvous.** A cell syncs against a *set* of peers per repository kind — not
a role, not a coordinator, not an "upstream" in any privileged sense (§3.0). Any
member may serve, several at once.

Three properties make the set a mesh rather than a fallback list:

- **Every member is contacted each pass**, in both directions. Peer B may hold a
  lease or an attempt that A has never seen; taking A's answer and stopping would
  rely on A to relay the rest, which makes A a coordinator however the config
  describes it.
- **The order is shuffled.** Reserved-ref replication takes what the cell lacks
  and *reports* rather than overwrites when both sides hold a ref at unrelated
  values, so on a contested claim whichever peer is contacted first is the one
  whose version is adopted. A fixed order would hand that to whoever was typed
  into the config first.
- **Reached is not the same as answered.** varvig's head push moves under a
  force-with-lease against one tracking ref, so with several peers at most one
  can accept a head push and the rest are refused as a matter of course. Those
  peers were reached — notes and reserved refs replicated, authority state
  arrived, only the branch disagreed — and counting that as unreachable would
  mark trust stale for a reason that has nothing to do with trust. Only a
  failure to make contact at all counts.

The two kinds have their own sets, with no fallback between them, because they
answer different questions: the project peers decide whether the cell is looking
at current *work* (the offline mode of §5.2), and the coordination peers decide
whether its *trust state* is current (the promotion gate of §4.3b). A cell cut
off from its project peers may still promote what it already holds; a cell cut
off from the coordination peers may not, because it cannot know who is still
allowed to sign. An empty set is a single-cell deployment: nothing to be
disconnected from, and neither offline nor stale.

**The single-project case.** One repository may serve both roles, and for a
factory with one codebase there is nothing to fragment. The two roles remain
two: a call site that says which repository it means keeps saying so if the
factory later grows a second project.

## 3. Capabilities

A capabilities object is **static facts only**. Liveness never goes in the DAG
(§2.1): GPU busy, queue depth and disk pressure are ephemeral, and an
append-only store would accumulate them as permanent garbage. If a field's
value can change without a human changing the configuration, it does not belong
here.

```json
{
  "cell_id": "mini-a",
  "inference": {
    "tier": "large",
    "models": [{ "id": "qwen2.5-coder", "version": "32b-q4", "context": 32768 }]
  },
  "build": ["go", "flutter", "android"],
  "test":  ["unit", "integration", "large-memory"],
  "roles": ["attempt", "verify", "build"],
  "robes": ["monitor"]
}
```

| Field | Rule |
|---|---|
| `cell_id` | Matches the identity of §1. Required. |
| `inference.tier` | `none`, `small`, `large`. `none` is legal and normal — a cell with no model can still verify and build. |
| `inference.models` | Sorted by `(id, version)`. Empty iff tier is `none`. |
| `build`, `test` | Sorted, deduplicated capability tokens. Free-form, matched by equality against a ticket's requirements. |
| `roles` | A non-empty subset of `attempt`, `verify`, `build`, sorted. |
| `effects` | Effectful capabilities this cell has an integration for (§8.2). Each names an alias **and** its interface hash. Optional, and absent for almost every cell. |
| `robes` | Responsibilities this cell wears (§3.4), sorted and deduplicated. A fixed set: `ambassador`, `monitor`, `procurement`, `provisioner`. Optional; a cell with none is an ordinary member. |

`effects` is **not** authority to spend — that is a lease, which an overseer
writes and a cell cannot. It says only that this cell has an integration wired
up. A cell needs both to act, and holding either alone means it declines.

**`roles` is the load-bearing field.** A cell may be a verifier or builder
without ever attempting (§2.1, §3.3). Micro ships `["build", "verify"]`;
attempting is opt-in, because a CPU-local model authoring code loses nearly
every selection while still consuming review attention (§3.1).

#### Cell class and inference are orthogonal

The capacity class — Micro, Mini, Medium — describes **build and test capacity
only.** It says nothing about inference, because inference reaches a cell
through an executor that may perfectly well be a hosted API, which makes it a
*capability* and not a hardware fact (§3).

So all four combinations are legitimate, and the two that are less obvious are
the ones worth naming: a Micro cell reaching a hosted model over HTTP is an
**inference cell**, and a Mini cell with no model configured is a **policy
cell**. Neither is a misconfiguration. If a reader ever has to be told which
class implies a model, the two axes have been welded together again.

A **policy cell** is not a degraded cell. It verifies, builds, executes
effectful capabilities and syncs, and the evidence it produces is what licenses
another cell's attempt to be promoted autonomously (§6.3.1) — the cheapest
hardware in the factory produces what makes the expensive hardware
trustworthy. Its one limit: it cannot originate work. It needs tickets from
somewhere and attempts to verify.

#### A declared model is not a reachable one

The `inference` block is a declaration, and this object holds static facts only.
Whether the runtime answers *right now* is a runtime fact, and it lives nowhere
in here.

The two are checked in different places and refused in different ways:

| Question | Where | On failure |
|---|---|---|
| Does the declaration cohere — does a cell advertising `attempt` declare a model? | this object's validation | the configuration is rejected |
| Does the runtime answer this pass? | the cell loop, once per pass | the cell **declines to attempt** and carries on |

The second row is the one worth stating plainly, because it used to be wrong. A
cell holding the attempt role with an unreachable runtime refused to *start*, so
it did none of the syncing, verifying or building it was still perfectly capable
of — one unavailable capability disabled every available one. A cell whose model
has gone away is not misconfigured; it is a cell with less to offer this pass,
and it says which runtime declined and why. It resumes attempting when the
runtime returns, with no restart.

Encoding is canonical JSON as defined in §4.3, so two cells configured
identically publish byte-identical capabilities.

---

### 3.4 Robes carry no authority

Capability = can do. Robe = responsible for. A robe is a **claim-policy input**,
not a permission, and the obvious implementation — a key per robe, a trust-store
edit whenever one moves, a lifecycle to revoke one — is the wrong one.

Check each against what it actually needs and none of them needs anything more
than an ordinary cell has:

| Robe | What it does | What it needs |
|---|---|---|
| `monitor` | Observes and reports. Never a source of truth, never decides. | Read, which every cell has. |
| `ambassador` | Where untrusted input enters. Turns outside requests into tickets. | Propose, which every cell has. |
| `procurement` | Decides what capability the factory should acquire; proposes, never provisions. | Propose. |
| `provisioner` | Instantiates cells from a Blueprint. | Infrastructure credentials and a lease — **not** elevated repository rights. |

Two consequences follow and both are load-bearing.

**Robe assignment is a derived projection.** Every cell reads the replicated
capabilities objects and computes who wears what locally. There is no registry
to keep in step, no tombstone when a cell goes, and no cleanup job: a cell that
stops publishing stops wearing anything. State that can be derived needs no
lifecycle.

**A compromised cell key reaches its own namespace whatever it claims to wear.**
What a cell may do comes from the one scope granted at enrolment — write its own
`refs/factory/cells/<cell-id>/*`, read its own lease, read the rest of the
coordination repository — and that bundle is identical for a cell wearing every
robe and a cell wearing none.

The provisioner is where this matters most, and the check is on the key chain
rather than on configuration. A provisioned cell's ownership roots to the
**owner**, never to the provisioner, and the way to be sure is not a field
saying so: a provisioner that could write another cell's authority would, if
compromised, mint cells loyal to itself, and every one would look legitimate. So
a provisioner is granted nothing that reaches another cell's namespace, and
wearing the robe does not change that.

No cell can give another cell a robe. A cell wears what its own configuration
says it wears; pushing one onto a peer would be authoritative assignment, which
needs consensus this design deliberately does not build.

---

### 3.5 Externally-originated tickets

A ticket an Ambassador created from an outside request carries a directive
saying so:

```
factory-origin: external
```

Its own directive rather than a key on `factory-requires:`, because it is not a
requirement — a ticket asking for nothing in particular can still have arrived
from outside, and that is exactly the ticket worth knowing about.

This is the prompt-injection surface, and the marking is the only thing that
makes it visible downstream. It is an **obligation of the robe**: nothing can
infer it, so an Ambassador that fails to mark a ticket produces one
indistinguishable from work the factory set itself.

What a cell does with the marking is an operator's decision, not this contract's.
A factory may want a person to see every external request first; another may run
them like any other work. So claim policy can filter on it and by default does
not. The one rule is that the check runs **above** the effectful split: a cell
that declines external work declines all of it, because an external request that
orders a physical thing is the case that matters most, not one to fall through
to a different branch.

---

## 4. Evidence and environment

### 4.1 Evidence

Evidence is a record of what was checked, by whom, with what result.

```json
{
  "attempt": "<attempt change hash>",
  "task": "<ticket id>",
  "cell_id": "micro-b",
  "environment": "<environment hash>",
  "checks": [
    { "name": "unit",  "status": "pass", "duration_ms": 4120 },
    { "name": "build", "status": "pass", "duration_ms": 18300 }
  ],
  "produced_at": 1755820800
}
```

`status` is one of `pass`, `fail`, `skip`, `error`. `skip` and `error` are not
`pass`: a check that did not run has not passed, and collapsing the two is how
a green wall stops meaning anything.

`cell_id` is **who asserted the result, not proof the run was honest**
(`varvig-design.md` §4b.3). The signature on the note establishes authorship and
nothing stronger. That limit is precisely why §6.3 requires the evidence to come
from a cell other than the attempting one: independence is the only leverage
available, so it is spent rather than assumed.

### 4.2 Environment

The environment descriptor is the answer to *against what*. Its fields mirror
varvig's `TypeEnvironment` object (`FEDERATION.md` §2) so that a native
artifact-ref/environment CLI can later replace the note form without changing
this shape:

```json
{
  "platform": "linux/amd64",
  "toolchains": { "go": "1.24.7", "gotestsum": "1.12.0" },
  "flags": { "CGO_ENABLED": "0" },
  "container": "<artifact content hash>",
  "model": { "id": "qwen2.5-coder", "version": "32b-q4", "params": "temp=0.2" }
}
```

Every adapter (§6) contributes a **fragment** — its own slice of `toolchains`
and `flags` — and the cell merges the fragments into one descriptor. An adapter
that cannot describe itself reproducibly cannot participate in cross-cell
selection, and the cell refuses to use it rather than emitting an environment
that is quietly a lie.

`model` is present **only** when the evidence was produced by inference. Build
and test evidence has no model, and inventing one would make a deterministic
environment look like a sampled one.

### 4.3 Canonical encoding and the environment hash

The environment hash is the identity that cross-cell comparison rests on, so its
computation is fixed here rather than left to an implementation:

1. Encode the descriptor as **canonical JSON**: object keys sorted by byte
   order, no insignificant whitespace, no trailing newline, integers without
   exponent, strings with the shortest legal escaping.
2. Omit empty fields entirely. An absent `model` and a `model` of `null` must
   not both be representable.
3. Hash the resulting bytes with **SHA-256** and render as
   `sha256:<64 lowercase hex>`.

The prefix is deliberate, and it is the reason the algorithm choice is not
load-bearing. It is a self-describing hash label in the same spirit as varvig's
multihash: an environment hash written by a cell built today stays unambiguous
when a second algorithm exists.

SHA-256 rather than the BLAKE3 varvig uses for object identity, because this
module has no third-party dependencies (see `README.md`) and SHA-256 is in the
standard library. Nothing is lost: the Factory environment hash is *Factory's*
identity for a descriptor — it names a note and is compared against other
Factory environment hashes. It is not, and does not claim to be, the varvig
object id of the equivalent `TypeEnvironment` object. When varvig grows a native
environment CLI, a cell will carry both: the varvig object id as the reference
and this hash as the comparison key, or it will migrate to the object id behind
a new label prefix. Either way no written state becomes ambiguous.

Two invocations of the same adapter set must produce the same hash (§9.4). An
environment that embeds a timestamp, a hostname, a PID, or a working directory
has failed this and is a bug, not a variation.

### 4.4 Environment class

Two environments are the **same class** when their `platform` matches and their
`toolchains` agree on every key both declare. Anything else — a differing
platform, a conflicting toolchain version, or a missing environment — is
**cross-class**.

Cross-class comparison is not an error and is not equality. It defers to a human
(§6.3 condition 2). Evidence with no environment is *unknown class* and never
matches anything, including itself.

---

## 5. Claims

A claim is a ref at `refs/factory/claims/<cell-id>/<task-id>` holding:

```json
{ "cell_id": "mini-a", "task": "<ticket id>", "not_after": 1755824400, "attempt": 1 }
```

`not_after` is mandatory. A claim past `not_after` is **stale**: it stops being
a reason for another cell to skip, and the cell that wrote it has no further
standing from it.

Three rules, and they are the whole protocol:

- **Claims are advisory.** They cannot be exclusive across a partition
  (`varvig-design.md` §4b.3). Two cells may each CAS successfully against their
  own view of the repository, and both are correct.
- **Duplicate attempts are normal and are the point.** Branching is search
  (§1.5). A cell must not add consensus, leader election, or a lock service to
  prevent them.
- **A cell may claim and attempt while disconnected from upstream.** This is
  required, not tolerated (§5.1): local-first operation is the property that
  makes the cell model worth having. The cost is budget spent on work that may
  prove duplicative, which is why offline speculation carries its own tighter
  cap (§9).

Because claims are per-cell refs, two cells never contend for the same ref name.
The CAS that matters is varvig's on the *attempt* and on the promoted branch,
where it fails safely rather than overwriting.

### 5.1 Decision tasks

The main loop calls no model (§10.1). A **decision task** is the bounded
exception: what a cell submits when deterministic policy genuinely cannot
decide, as one inference call down the ordinary executor path, recorded as an
attempt so the claim can be audited later.

Wanting one every turn is a signal the policy layer is underspecified, not a
reason to add intelligence. Four guardrails, each enforced rather than
documented, and three of the four fail *open* — a broken one does not throw, it
quietly lets a cell think more, or deeper, or with authority it was never given:

1. **A separate budget line.** Otherwise deciding consumes the lease meant for
   doing, invisibly, because it looks like ordinary spend. A cell that thought
   its way through its whole budget would have nothing left to think *about*. No
   meta line configured is a refusal, never a fallback to the main one.
2. **No nesting.** A decision task cannot spawn a decision task. One level, hard
   stop — the recursion has no natural floor and every level looks locally
   reasonable.
3. **An unreachable executor degrades to policy, never stalls.** Policy must
   always suffice to at least safely do nothing. A question therefore names a
   fallback that is one of its own options, and an executor that is down,
   refuses, or answers something outside those options produces that fallback.
4. **No authority.** A decision informs a choice within already-granted bounds.
   A question carries no amount, no principal and no permission, and its answers
   are a closed set policy has already ruled in — an open answer would let a
   model propose something nothing authorized. Break this and §8.2's separation
   collapses into a cell authorizing its own spending by thinking harder.

---

## 6. Seams

**Two seams, not four** (§4). Everything hardware- or vendor-shaped lives behind
them, so that neither varvig nor the cell loop ever learns about CUDA,
quantization, or container runtimes.

| Seam | Contributes to the environment |
|---|---|
| **Executor** — anything that performs work | `model` and the inference toolchain entry when it authors; `platform`, toolchain versions and outcome-affecting flags when it checks |
| **Artifact store** — the only other adapter, a sink | the `container` reference, when the executor has one |

There is no model-runtime seam separate from a build-sandbox seam. They were two
adapters with one shape — name yourself, describe your environment, do a unit of
work — and keeping them apart made "how does a cell run a model" a different
question from "how does a cell run a test". A harness would then have been a
third answer to a question that should only have one.

The rule that replaces them: **a new executor must never require a new
concept.** A Claude Code harness is an executor whose properties say it loops and
takes tools. A CNC machine is an executor whose properties say it is effectful.
Neither needs an interface, a package, or a config section of its own.

### 6.1 Properties, not kinds

An executor declares what it is *like*, never what it *is* (§4.1):

| Property | Meaning |
|---|---|
| `loops` | Iterates on feedback, or answers once |
| `tools_attached` | Receives a task MCP socket — the line between thinking and doing |
| `deterministic` | Results are evidence-comparable and cacheable |
| `consumes_lease` | Running it spends budget |
| `effectful` | Real-world side effects — the §8.2 class, here just a property |

Nothing branches on which executor it holds, and a guard fails the build if
anything starts to: the wiring an executor gets is proportional to the
properties it declares, so a new shape of executor changes a table of behaviour
rather than a list of cases.

`deterministic` is the load-bearing one. A checking executor is deterministic,
which is what lets one cell's evidence license another cell's attempt (§6.3.1)
and why a transaction may re-run it; an authoring executor is not, which is why
one must never run inside anything that retries (§8.2 rule 7).

### 6.2 Describing the environment

Every executor reports a deterministic fragment (§4.2), **from the environment
it will actually run in** — a fragment read from configuration rather than from
the running toolchain is a claim, not a measurement.

An executor that cannot describe itself reproducibly is refused, and the two
roles are refused differently, which §3 explains: a cell whose *checking*
executor cannot describe itself has nothing left to do, so it does not start; a
cell whose *authoring* executor cannot answer declines to attempt and carries on
with everything else.

---

## 7. Artifacts

Binary outputs are referenced, never stored in varvig (§8). A reference is
recorded with varvig's own verb:

```
varvig tickets attach-artifact <ticket> --content-hash <multihash> \
  [--media-type M] [--size N] [--locator U ...] [--produced-by <change>]
```

This stores a real `artifact-ref` object and pins it for reachability, which is
the whole point: when the artifact later goes unreachable it appears in
`varvig gc --report-external`, and a cell can learn that its registry bytes are
no longer needed. **A JSON note carrying the same fields does not.** It is not an
`artifact-ref`, so GC's mark phase never sees it and the report is silently always
empty — the exact failure `varvig-federation-spec.md` §1 exists to prevent
("either deleted while a live change still needs it, or orphaned forever because
nothing knew it went unreachable").

The record's fields:

```json
{
  "content_hash": "sha256:…",
  "media_type": "application/vnd.oci.image.manifest.v1+json",
  "size": 41283910,
  "locators": ["oci://registry.internal/varvig/app@sha256:…"],
  "produced_by": "<attempt change hash>"
}
```

`content_hash` is identity; `locators` are hints, sorted and deduplicated so an
equal locator set encodes identically. A locator changing is not a new artifact —
that distinction is why the same image reachable from three registries is one
record with three locators rather than three records.

### 7.1 Hash encoding across the boundary

A cell computes `content_hash` in the labelled form of §4.3 and converts it to a
multihash on the way to varvig. The conversion is lossless and involves no second
hash: varvig's multihash is `<uvarint code><uvarint length><digest>`, SHA2-256 is
a registered code (`0x12`), and both forms carry the same 32 digest bytes. So
`sha256:<hex>` becomes `1220<hex>`.

This is the payoff of §4.3's argument that the algorithm choice was never
load-bearing. Reading back, a cell accepts BLAKE3 (`0x1e`) as well, because a
content hash may have been written by a peer that hashed with varvig's default —
and refusing to read it would make a cell blind to artifacts it did not produce.
A hash in an algorithm the cell cannot name is an error, never a value passed
along unlabelled.

### 7.2 Anchoring

The attachment is **ticket-anchored**, because that is the anchor varvig's verb
takes. With several attempts at one ticket, `produced_by` is therefore the only
thing that says which attempt built which output, and a cell must always set it.

varvig's own reader already unions the per-ticket index with a change's
`Change.Artifacts` "for the day a materialization producer names on the change the
artifacts it built" — which is exactly a cell's case. When a verb exists to write
that field, a cell should move to it and drop the ticket-anchored form; until then
ticket + `produced_by` is complete, and writing both would be two records that can
disagree.

### 7.3 Retention

- **Speculative artifacts stay cell-local.** Replicate on promotion, not on
  attempt. A federation that replicates every attempt's build output has turned
  speculation into a bandwidth bill. Recording a reference is not replicating
  bytes: what leaves the cell is identity and locators.
- **Factory owns registry credentials; varvig must never acquire them.** varvig
  reports unreachable artifact hashes; deletion is Factory's or the operator's
  action.

### 7.4 The legacy note form

A cell running against a core without `tickets attach-artifact` falls back to a
note in `factory/artifact` carrying the fields above. The fallback must be
**announced every time**, naming what is lost: the artifact will not appear in
`varvig gc --report-external`, and the symptom of not saying so is a report that
stays empty rather than an error anybody sees.

## 8. Budget

### 8.0 Budget is optional, and absent means unenforced

**A factory with no budgets configured must simply work.** Absence of a cap
means *no enforcement*, never *zero budget* — and the difference is not
academic, because this contract used to have it backwards. An unset
`inference_daily` read as a cap of zero, so the ledger refused every spend and
claim policy skipped every ticket citing budget: a factory nobody had
configured did nothing and reported being out of money.

Most of what a cell does costs nothing external — builds, tests, verification,
sync, local inference on hardware already paid for — so an unconfigured budget
is the ordinary case for a factory that spends nothing, not an edge case being
tolerated.

**Leases gate spend, not action:**

| Capability declares | Requires |
|---|---|
| No cost model | **Nothing.** Runs freely. |
| A cost model (`fixed` or `quoted`) | A lease with headroom |

One guard keeps that honest, and it has to arrive in the same change as the
permission or the permission is a hole: **a capability that incurs cost must
declare a cost model.** An action that reports a cost against a capability
declaring none is refused as *malformed*, not run unmetered. Omitting the cost
model must never be the cheapest way to spend money unwatched.

**`effectful` and `costs money` are orthogonal.** Effectful means
*irreversible*. Turning on a light, moving an arm, printing with filament
already paid for, posting a message: all irreversible, all free. Such a
capability declares `effectful`, no cost model, and needs no lease — and still
obeys every §6.7 rule, because every one of those rules exists because of
irreversibility rather than because of cost.

Where a free effect still needs bounding, the envelope's **quantity and rate**
ceilings do it, with no currency invented for the purpose. For a free
capability those are the *only* bound there is, which is why they had to start
being enforced for this to be safe rather than merely permissive. A rate
ceiling is measured from reservation refs; an unmeasured history refuses rather
than passing, because an unmeasured history is not an empty one.

Where nothing is configured, nothing is bounded.

#### Where a budget *is* configured

A cell declares a spend cap and halts when it is exceeded. Where one is set it
is enforced strictly: attempts multiply cost, and a disconnected cell claiming
speculatively can burn budget on work that proves duplicative (§7). Making
absence permissive must not make presence permissive.

```json
{
  "inference_daily":    50.0,
  "verify_concurrent":  4,
  "storage_gb":         200,
  "attempts_default":   3,
  "offline_inference_daily": 10.0
}
```

- **Halt, do not degrade.** A cell out of budget stops claiming and says so.
  Silently switching to a worse model produces attempts that pollute selection —
  the failure is invisible in the output and visible only in the selection
  statistics weeks later.
- **Offline speculation is capped separately and more tightly**, since a
  disconnected cell cannot check whether another cell already succeeded.
- **Storage pressure triggers pin release before GC** (`FEDERATION.md` §4), so a
  cell drops its own retention obligations deliberately rather than collecting
  state another cell is still evaluating.

§8 bounds compute, which is regenerable: exceeding it wastes money and nothing
else. The two subsections below bound spend that cannot be regenerated, and they
are a different mechanism for that reason.

### 8.1 Authority: envelopes and leases

An overseer grants a cell authority to spend in two shapes, and the difference
between them is the whole design:

| | Envelope | Lease |
|---|---|---|
| What it is | A **shared ceiling** across every cell under one overseer | An **exclusive allocation** to one cell |
| Ref | `refs/factory/envelopes/<overseer-id>` | `refs/factory/leases/<cell-id>/<capability>` |
| Which repository | Coordination (§2.1) | Coordination (§2.1) |
| Enforceable from a stale view? | No — another cell may have spent it | Yes — nobody else can spend it |
| Spendable offline | No | Yes, indefinitely |

The capability is hex-encoded in a lease ref, because a capability alias
contains `@` and a ref path component should not carry an alias's punctuation.

```json
{ "overseer": "overseer-a", "set_at": 1755820800,
  "ceilings": [
    { "capability": "pcb-fabrication@1", "spend_minor": 500000, "unit": "EUR",
      "quantity": 100, "rate_per_day": 4 },
    { "capability": "human-contract@1",  "spend_minor": 200000, "unit": "EUR" }
  ] }
```

```json
{ "cell_id": "mini-a", "capability": "pcb-fabrication@1",
  "overseer": "overseer-a", "envelope": "<envelope object hash>",
  "amount_minor": 100000, "unit": "EUR", "quantity": 20,
  "spent_minor": 32000, "ordered": 5,
  "reserved_minor": 40000, "reserved_units": 6,
  "issued_at": 1755820800, "reclaim_after": 1755907200 }
```

Four rules:

- **An envelope with no ceilings is malformed, not unlimited**, and a capability
  the envelope does not list has no ceiling to be under. Silence is not
  permission.
- **Leases never overlap and never sum past their ceiling.** One cell holds at
  most one lease per capability; the sum of outstanding leases for a capability
  is bounded by the envelope's ceiling for it. The overseer's maximum exposure is
  therefore the sum of outstanding lease *headroom*, which is the number to
  reason about — not the envelope, which is only what could be allocated.
- **Headroom is the allocation less what is spent and less what is held.**
  `reserved` and `reserved_units` are held by reservations that have not settled
  (§8.2). Held is not spent — a hold can come back and a spend cannot — but it is
  equally unavailable, because a pending reservation may already be a real order
  at the far end.
- **A lease is spendable from a stale view, indefinitely.** The amount was
  committed when the lease was issued, so no connectivity is in the spend path.
  This is the autonomy that matters: a disconnected cell keeps working.
- **Beyond the lease escalates; it never falls back to the envelope.** A lease
  that can be exceeded by drawing on the shared ceiling is advisory, and an
  advisory exclusive allocation is a shared one.

**Tightening bites; loosening does not.** An overseer who narrows an envelope
mid-run has the tighter ceiling honoured before the cell's next effectful
action, and with *no fresh sync*, because adopting a tighter ceiling can only
reduce spend — being wrong means spending less than authorized, which nobody has
to undo. A *wider* envelope grants a cell nothing: the effective allocation is
the minimum of the lease and the ceiling, so more headroom requires a **new
lease**, which only the overseer can write and which the cell therefore cannot
see without syncing.

The minimum is the whole mechanism, and it is why there is no freshness check in
this path: §4.3b's point is that effectful action inside a lease needs no
connectivity, and a staleness test here would take that back.

Two things this does *not* buy, stated because a cap is easy to mistake for a
guarantee:

- **The ceiling is shared, so local capping does not bound the sum.** Three cells
  each capping themselves at one ceiling still permits their total to exceed it.
  Tightening is enforced by the overseer **not replenishing**; the cell-side cap
  is a floor of safety underneath that.
- **It cannot claw back an unspent lease without reaching the cell.** A
  partitioned cell has not seen the tightening and will spend its lease. That is
  acceptable rather than a hole, because exposure was already bounded by the
  lease amount when it was issued.

A tightening never un-spends money. A ceiling below what is already spent means
nothing further is spendable, not that spend is reversed.

The bounded lease is a **view for deciding, never a value to store.** Writing it
back would rewrite the record of what the overseer committed to, which is the
evidence for every later question about the spend.

`reclaim_after` is a **signal to the overseer, not an expiry**. It does not stop
the holder spending. A lease with any recorded spend is never reclaimed, because
the cell may have placed an order it has not yet reported, and reclaiming there
is exactly how a double-spend happens.

The same split decides what a cell may do while its view of trust state is stale
(`varvig-auth-and-api.md` §4.3b) — grouped by reversibility, not by whether it
is a write:

| Act | Stale view |
|---|---|
| Propose | Allowed. Append-only bounds the damage: a revoked principal that has not heard yet wastes compute. |
| Promote | **Refused.** It moves a ref, and a shared ceiling cannot be enforced locally. |
| Effectful, priced | Allowed **within an outstanding lease**, offline, indefinitely. Refused with no lease. |
| Effectful, free | Allowed with no lease at all; bounded only by the envelope's quantity and rate ceilings (§8.0). |

Freshness is Factory's to enforce, not varvig's, and not by choice: varvig's
ref-update verification checks a signer against the trust file *as the verifying
peer holds it*, and a partitioned peer holds a stale file it has every reason to
believe is current. Nothing inside varvig can tell the difference. The loop,
which ran the sync, can.

There is no age threshold by default. A successful sync establishes current
state; inventing a staleness clock on top of that would be inventing policy. An
operator who wants one — for a sync loop that has stalled without failing —
configures it.

### 8.2 Effectful capabilities

An effectful capability has real-world side effects that cannot be re-run,
discarded, or regenerated: ordering fabrication, contracting a human, shipping,
moving money. Every other assumption in Factory inverts here. Speculation is
search, so attempts are normally cheap and duplicates across a partition are the
point. `--attempts 3` on a board order means three orders and three invoices.

The hazard is that an effectful capability looks exactly like an ordinary one
until the invoice arrives, so the marking is explicit and the rules are
refusals:

1. **`attempts` is 1, by rejection and not by clamping.** A clamp would execute
   something other than what was asked for, on the one class of action where
   that means a wrong order rather than a wasted GPU-hour. The task is wrong and
   its author is told.
2. **An idempotency key is mandatory**, in both promotion modes. It is *derived*
   from `(task-id, capability alias, interface hash, canonical payload)`, each
   component length-prefixed, so a retry after a mid-flight network failure
   computes the same key from the same intent without having to remember one.
   The key is a function of what the action is, so "the same action" and "the
   same key" cannot drift apart.
3. **A higher principal authorizes, and a cell never authorizes its own
   effectful action** — not even holding a factory key with `promote`. Promote
   rights move refs; they are not a licence to spend money, and conflating the
   two turns a scoped repository credential into a purchasing credential. The
   higher principal need not be human: an overseer agent satisfies this fully.
4. **Bounded by the envelope** — spend, quantity *and* rate. This applies to
   every effectful action, including one that costs nothing, and for a free
   capability it is the only bound there is. All three dimensions matter
   because they fail differently: a spend cap stops one expensive mistake, a
   quantity cap stops a units-confusion mistake, and a rate cap stops a loop
   that is individually within both and runs all night.
5. **Spent from the acting cell's own lease — if the capability declares a cost
   model.** Another cell's lease is not spendable here, however much headroom
   it has. A capability declaring no cost model needs no lease at all (§8.0),
   and every rule above still applies to it: they exist because the action is
   irreversible, not because it is expensive.
6. **Never auto-retried, never regenerated.** A failed effectful action
   escalates; retry is an authorized decision, not a loop behaviour. A
   conflicting effectful attempt does not re-run, because the external world has
   already moved.
7. **Never inside anything that retries.** A cell runs its executor outside any
   transaction, so a ref-contention retry cannot re-place an order. Today this
   holds trivially — nothing here constructs a transaction at all — and it is
   written down so it keeps holding when a landing surface exists.

#### How a ticket asks for one

A ticket names an effectful capability in the same directive that carries build
and test requirements, with the parameters on a line of their own.

**A ticket supplies the identity; the cell's configuration supplies the terms.**
The interface hash and the cost model come from the cell's own capabilities
object, never from the request. That direction is load-bearing: reading the cost
model off the ticket would let a ticket declare a priced capability free and
walk straight past the lease check, so what a ticket can ask for is *what* to
do and never *what it costs*.

```
factory-requires: effect=pcb-fabrication@1 interface=1220a1b2…
factory-effect: {"gerber":"rev-c","quantity":5}
```

One line for the parameters is enough because canonical JSON contains no
newlines (§4.3) — the same property that makes note payloads parseable.

Five rules, each a refusal rather than a correction, because this is the class
of work where "we assumed you meant X" buys a wrong order:

1. **The interface hash is required**, not just the alias.
2. **The parameters are required and must be valid JSON.** They are kept
   verbatim and hashed into the idempotency key: re-encoding them, even
   correctly, risks two readings of one ticket producing two keys and therefore
   two orders.
3. **`attempts` may only be 1.** A ticket asking for more is rejected here, at
   the earliest point it can be caught.
4. **A ticket does one kind of work.** `build`/`test` and `effect` are mutually
   exclusive: one describes a change that is attempted, scored and promoted, the
   other an order placed once.
5. **A malformed effect never falls through to the attempt path.** Answering
   "order me a circuit board" by writing code is the failure mode this exists to
   prevent, so the requirement survives with the reason attached.

An effectful ticket is **not gated by the attempt role**: authority to spend
comes from a lease, and tying it to holding a model would make the right to
spend money depend on the presence of a GPU. Nor is it gated by the inference
budget — an order is paid from its lease, and coupling the two would surface as
an order that silently did not happen.

#### The lifecycle

    quote → reserve → authorize → execute → settle

Authorization is checked **before** the reservation rather than between it and
execution: a refusal arriving after the key is claimed has already consumed the
cell's one chance to act on that ticket. Quoting comes first because a hold
needs an amount, and for a quoted capability the amount is not known until the
service is asked.

What happens after execution is decided by *what the cell knows*, not by what it
hopes:

| Executor returns | Meaning | Lease | Reservation |
|---|---|---|---|
| success | the effect happened | hold becomes spend, at the actual price | `done`, with the far end's reference |
| a definite rejection | **no** effect occurred | hold released | `failed`; the key stays claimed |
| anything else | **unknown** | hold stands | `pending`; escalates |

#### The lease is written first, always

The Lease and Reservation columns above are two writes to **two repositories**
(§2.1), so no row is atomic and none can be made so. The order is fixed: the
lease write lands before the reservation record moves, whether it takes
headroom, converts it to spend, or gives it back. What that buys is a specific
set of survivable failures:

| Second write never lands | State left behind | Recovery |
|---|---|---|
| after a hold is taken | headroom held for a key nobody claimed | the expiry returns it (§9.14) |
| after spend is recorded | the lease over-reports what this cell spent | an overseer reading the lease corrects it |
| after a hold is released | a record still reading open against a lease that no longer holds for it | a retry hits the double-release guard and errors loudly; a principal resolves it |

The state never reachable is a reservation saying an irreversible action is
settled against a lease with no record of paying for it. That is the one failure
here nobody can undo, and the ordering exists to buy exactly it.

Reversing the order for the release rows would trade a visibly stuck reservation
for a narrow double-spend window — a released hold plus a record still open
invites a principal to resolve it as *happened*, spending headroom that was
already given back. The guard makes that loud rather than silent, and the fixed
order keeps it out of reach.

The third row is the one implementations get wrong. A timeout, a dropped
connection and a 500 are all consistent with the order having been placed, so
only an executor that was *definitely told no* may claim nothing happened. The
hold is deliberately **not** released on an unknown outcome: the money may be
gone, and returning it would let the cell spend it twice.

If the effect succeeds and recording it fails, that is the one error that stops
the pass rather than being counted. The lease still holds rather than spends and
the reservation still reads pending, so the operator sees an unresolved action
rather than a clean slate.

#### Connectors: how a vendor gets added without a rebuild

A capability is performed either **in-process** or by a **connector peer**. The
second is the one that matters for anything real, and it is modelled on how core
does tracker bridges: a separate peer, holding its own credentials, that core
"never learns the vendor's name" of, untrusted, and therefore able to run
anywhere.

The reason to prefer it here is sharper than for bridges. An executor holds
credentials to a service that charges money. Compiling vendors into the cell
binary would mean a rebuild to add one and those credentials living in the
cell's process; a connector holds its own and needs neither.

**The interface is repository state, not an ABI.** There is nothing to load,
nothing to link, and no version to keep in step:

| | |
|---|---|
| cell | **offers** a reservation naming a capability |
| connector | **takes** it by compare-and-swap, recording who it is |
| connector | **executes** against the vendor |
| connector | **reports** the outcome — an order number and what it cost |
| cell | **settles** the lease from that report |

The last line must not move. A connector reports; only the cell spends. That is
the containment core applies to a bridge, which may sign a weak attestation and
never a strong one: the untrusted peer states a fact, and the trusted layer
applies it under rules. Here the rule is the lease, so **a connector that lies
about cost is bounded by an amount the overseer chose deliberately** — a
settlement beyond the allocation is refused outright.

A connector cannot create a reservation, so it cannot invent work for itself; it
cannot raise a lease or widen an envelope; it cannot settle; and it cannot take
a reservation another connector holds. What it can do is claim an outcome for an
action a cell reserved and an overseer authorized — and be wrong about it, within
that lease.

Two connectors racing produce one holder and one refusal, using varvig's
ordinary ref CAS rather than a lock, a lease or a queue. Passing the take
deadline does **not** hand the work to somebody else: by then the holder may
have reached the vendor, and a second connector acting is exactly the double
order this exists to prevent. A stale claim escalates.

**A connector that does not know reports nothing.** "We never heard back" is not
a report; the reservation stands as `pending` and escalates. A connector that
guesses is worse than one that goes quiet, so the shapes that would let it guess
— a success with no reference, a rejection with no refusal — are refused.

#### The reservation is what actually stops the second order

Deriving a stable key says what "the same action" means. It does not by itself
stop the action happening twice — for that, the key has to be claimed somewhere
that survives the process, **before** the effect is attempted. That place is
`refs/factory/reservations/<cell-id>/<idempotency-key>`, and the claim is create-only:
whoever creates the ref executes, and everyone else finds it already there. That
is varvig's ordinary ref CAS doing the work; nothing here needs a lock.

The order is deliberately the pessimistic one — reserve, execute, settle:

| State | Meaning | Who may clear it |
|---|---|---|
| `offered` | reserved and waiting for something to act on it — **nothing has happened yet** | the expiry, which returns the headroom |
| `pending` | taken, and **possibly executed** — nobody knows which | a higher principal, after checking the external system |
| `reported` | an executor claims an outcome the lease has not been settled from yet | the cell, on its next pass |
| `done` | the effect is confirmed to have happened; carries the far end's own reference | — |
| `failed` | the external service is confirmed to have **rejected** it, so no effect occurred | — |

The line that matters runs between `offered` and `pending`. Before anything
takes the reservation nothing has happened and the headroom is safe to return;
from the moment something takes it the outcome is unknown and the key is claimed
for good. Collapsing the two would make a reservation nobody touched look like an
order in flight — or, worse, the reverse. `reported` is separate for a different
reason: a connector reports and only the cell spends, and those are two writes
that cannot be one.

A reservation also **holds lease headroom** for as long as it is outstanding
(§7.1). Without that, two pending actions would each check the same headroom,
each pass, and together exceed the lease — the first order's money still looks
available right up until its invoice arrives. So headroom has three states, not
two: allocated, **held**, and spent. Only settlement turns a hold into spend,
and it settles the price actually charged rather than the one quoted; a
divergence is recorded rather than absorbed, because one is noise and a pattern
of them is a capability whose quotes cannot be trusted.

The hold is taken **before** the key is claimed. If the claim then fails the
hold is released, and if that release fails the headroom leaks until it expires.
That order is chosen deliberately: leaking headroom is recoverable and a
double-spend is not, so the two writes fail in the recoverable direction.

A crash between reserve and settle leaves `pending`, which is the honest record
of the one state that matters. Three rules follow, and each of them forbids
something that would otherwise look like a reasonable clean-up:

- **A timeout is not a failure.** "We never heard back" and "it did not happen"
  are different claims and only one is safe to act on, so only a definite
  rejection may be recorded as `failed`.
- **A pending reservation is never retried and never deleted.** Deleting it to
  clear the way is precisely how the second invoice arrives.
- **A cell cannot resolve its own pending reservation.** Resolving it means
  checking the far end and asserting what is true there, which is the same class
  of act as authorizing the spend in the first place.

A settled reservation must carry the external system's own identifier. The next
question about an unexpected invoice is "which order was it", and the answer has
to be in the record.

**Expiry releases the hold, never the key.** A lost external response must not
consume budget for good, so the held headroom comes back on a timer. It must
also not let the same action be submitted again, because it may well have
happened — so the key stays claimed permanently. Two different resources,
released on two different rules: money on a timer, the right to act not at all.
A reservation whose hold has lapsed is still `pending`, still reported, and
still waiting on a principal; if that principal finds the order was real, the
spend is applied to the lease with no hold left to convert.

A capability reference names the **interface hash**, not only the alias. Two
factories may hold the same alias without agreeing who owns the name, so
matching is on the hash: an alias match with a hash mismatch is precisely the
collision the binding exists to catch. The hash is in the idempotency key too,
so re-pointing an alias cannot make a new action look like an old one.

A capability whose price is only known by asking an external service **requires
connectivity by its nature, not by policy**. There is no rule here forbidding a
quoted action offline — only the observation, surfaced in the refusal, that it
cannot happen.

Every unmet rule is reported at once. Elsewhere an early exit saves an expensive
re-verification; nothing here is expensive, and an operator about to spend money
should see the whole list rather than one round trip per broken rule.

---

## 9. Promotion agreement

While running gated, a cell records for each promoted ticket whether the
highest-scoring attempt was the one the human promoted (§6.4):

```json
{ "scope": "src/generated/", "task": "<ticket id>",
  "top_attempt": "<hash>", "promoted_attempt": "<hash>",
  "agreed": true, "observed_at": 1755820800 }
```

The agreement rate over these observations, **per scope and never global**, is
the only honest basis for enabling autonomous promotion. Below the configured
threshold (default 0.80) autonomous mode refuses to enable and says why.

The risk being managed is `varvig-design.md` §5: the moment promotion is
automatic, the test suite silently becomes the real source of truth, and
speculation scoring will find whatever it does not check. Autonomy is not
forbidden — it is earned per scope, with evidence.

---

## 10. What a cell must not do

Stated as prohibitions because each one is a mistake with an attractive
rationale.

1. **No second scheduler.** A cell must not compute affected sets, assign
   read/write sets, order concurrent work, or decide serialization. Nor may it
   invent its own ticket ordering: which work matters most is learned by core
   from recorded decisions (`tickets rank`), and a cell asks rather than
   deciding — its own ordering would be one machine's opinion where core has the
   whole history. It submits
   work to varvig and lets varvig serialize (§1). Conflating claim policy with
   task scheduling means reimplementing affected-set logic badly, in the layer
   least equipped to do it.
2. **No liveness in the DAG** (§2.1).
3. **No consensus on claims** (§5).
4. **No silent model downgrade under budget pressure** (§8).
5. **No self-verified autonomous promotion** (§6.3 condition 1).
6. **No cross-class comparison treated as equality** (§4.4).
7. **No registry credentials handed to varvig** (§7).
8. **No class-specific code path.** Micro, Mini and Medium are cell *classes*
   describing capacity, and they differ only in which model runtime and budget
   the configuration names (§1.2). A branch on the class name in the code means
   the abstraction has failed. (The design notes called these tiers; the word
   changed because a tier implies a rank, and there is none — see §11.)
9. **No self-authorized effectful action** (§8.2 rule 3), whatever rights the
   cell's own key carries.
10. **No speculation on an effectful capability**, and no silent clamp to one
    attempt instead (§8.2 rule 1).
11. **No promotion from a stale view of trust state** (§8.1). A cell that cannot
    confirm trust state is current keeps proposing and stops promoting.
12. **No borrowing against the envelope** when a lease runs out (§8.1). The cell
    stops and says so; a higher principal decides whether to raise the lease.
13. **No clearing a pending reservation to get unstuck** (§8.2). Not by retrying,
    not by deleting it, and not by the cell resolving its own unknown state.
14. **No lease outside the coordination repository** (§2.1). A copy in a project
    repo is not a cache, it is a second authority: every project would carry its
    own plausible answer to what the cell may spend, and the sum would exceed
    the envelope with nothing able to notice.
15. **No reservation settled before its lease** (§8.2). The two writes are to two
    repositories and cannot be atomic; a record saying an irreversible action
    was paid for against a lease with no record of paying is the one state here
    that nobody can undo.
16. **No unset budget read as a zero budget** (§8.0). Absence of a cap means no
    enforcement. Reading it as a ceiling of zero makes a factory nobody
    configured refuse to work and report being out of money, which looks like
    caution and is a fault.
17. **No cost taken on a capability that declares no cost model** (§8.0). The
    action is refused as malformed rather than run unmetered, because the moment
    "no cost model" means "no lease needed", omitting it becomes the cheapest
    way to spend money with nothing watching.
18. **No cost model read off a request** (§8.2). A ticket names what to do; an
    operator's configuration says what doing it costs. The other direction lets
    a ticket declare a board order free.
19. **No authority attached to a robe** (§3.4). A robe is a claim-policy input.
    Granting one the rights it obviously "should" have looks like a small
    convenience and is a design change: it reintroduces a key per robe, a
    trust-store edit whenever one moves, and a lifecycle to revoke one.
20. **No robe assigned by another cell** (§3.4). A cell wears what its own
    configuration says. Pushing a robe onto a peer is authoritative assignment
    and needs consensus this design does not build.
21. **No external origin inferred** (§3.5). A ticket is externally originated
    only because an Ambassador marked it. Guessing would decline ordinary work
    at random; guessing the other way is worse.
22. **No decision task inside a decision task, and none on the main budget
    line** (§5.1). Both failures are silent: the first recurses with every level
    looking reasonable, the second spends the budget meant for doing the work.

---

## 11. What this contract does not yet cover

The design notes moved ahead of this implementation in three places, and one
representation choice here is worth flagging. They are listed rather than left
to be discovered, because a contract that quietly omits a rule reads exactly
like one that has decided against it.

**Cell classes, not tiers, and factories are flat.** Micro, Mini and Medium
describe *capacity*; a factory is one or more cooperating cells with no
hierarchy. The terminology is corrected throughout this document, the
implementation has no branch on the class name, and rendezvous is now a set per
repository kind (§2.1) rather than a single address — so a factory keeps working
when any particular member is unreachable.

Two varvig changes were needed to make that work rather than merely exist: a
refused branch no longer suppresses the notes and reserved refs travelling
alongside it (`FEDERATION.md` §6), and remote-tracking refs are per peer, so a
push leases against the peer it is pushing to rather than whichever peer was
fetched last (§7). Without the first, authority reached exactly one member of a
set; without the second, only one member could accept a head push.

**Interfaces are varvig objects.** A capability reference binds to the interface
*hash*, and that hash is now the id of a stored schema object: publishing writes
the canonical schema and the id that comes back *is* the hash. So a resolve is an
ordinary blob read at the same id the capability names, and a registry cannot
disagree with the hash it is keyed by.

`refs/factory/interfaces/<alias>` points at the schema so a person can type a
name. Nothing in the effectful path reads the alias — re-pointing one changes
what a human types and nothing about what a lease bounds or what an idempotency
key covers, because the hash is in the key. The registry lives in the
coordination replica (§2.1): what an interface requires is a fact about the
factory, and a per-project registry would let two projects disagree about one
hash's meaning while both looked correct.

**An interface the registry does not hold is refused before the money.** A hash
is enough to tell two interfaces apart, which is what matching needs, and not
enough to say what an action requires. Acting on a hash nobody published means
ordering a shape no one in the factory can describe.

**Reputation is derived.** A cell's standing is: of the attempts it made at tasks
that were later promoted, how many were the attempt that moved. Both halves come
from state a cell cannot write on another's behalf — attempt refs are namespaced
by cell id and immutable, and a promotion observation names the change that
actually moved, so credit follows the change rather than whoever moved the ref.
That matters in a flat factory, where the cell that promotes is usually not the
cell that produced the work.

Attempts at tasks nobody has promoted yet are excluded: unfinished is not failed.
A cell with no record has no rate rather than a rate of zero, because "no record"
and "a record of losing" are opposite answers and a newcomer reading as the worst
possible cell is how a metric becomes a barrier to entry.

**It is reported and never acted on.** Nothing in the loop reads it. A cell
consulting reputation to decide whether to attempt would be ordering work by a
quality judgement, which is varvig's job (§10.1) — and it would compound, since a
cell that attempts less has less record. The number is for whoever can see the
whole picture: an overseer sizing a lease, a human deciding what to keep running.

**Money is counted in minor units.** Every amount is an integer of the named
unit's minor units — `"amount_minor": 100000` is €1000.00 — and the keys say
`_minor` so a reader that has never heard of the encoding cannot silently read
100000 as a hundred thousand euros. It sees no key, gets zero, and validation
refuses: loudly wrong beats quietly wrong by a factor of a hundred.

This replaced `float64`, which was not merely imprecise but *incorrect*. Three
things here are hostile to binary floating point at once: amounts accumulate (a
lease's spend grows one settlement at a time), round-trips must be exact (a hold
released must return the lease to precisely where it started), and refusals are
decided on comparisons (`Lease.Release` refuses when the amount exceeds what is
held). Holding 0.30 and releasing 0.10 three times had the third release refused
as a double release — reported as *"releasing 0.1 EUR ... which holds only
0.1"*, because the formatter rounded both sides to the same string. The mirror
case left 5.5e-17 reserved: a phantom hold that any "is anything outstanding"
check reads as yes, permanently.

Rendering and parsing take the unit, because an amount is a count of minor units
and does not know its own currency. ¥1000 prints as `1000` and €10.00 as `10.00`;
parsing "1000" in JPY gives a thousand yen rather than ten. The exponents are
ISO 4217's minor-unit column for the currencies that are *not* two — the
exceptions, not the world, because a full table would be data this contract has
no way to keep current and a stale one leaves a wrong answer that looks
authoritative. Anything unlisted is assumed to have two, which is the common
case and the safe reading of a unit nobody here has heard of.

**The compute budget counts in minor units too.** It bounds regenerable spend, so
no refusal there is irreversible — which was the argument for leaving it in
`float64`, and is not an argument for keeping two arithmetics for money. Prices
are computed per thousand tokens in integers, rounding to nearest with ties away
from zero: over-charging halts a cell early and under-charging lets it run
slightly long, and consistently rounding up would compound across thousands of
small calls into a cap tighter than the one configured. The derived offline cap
truncates rather than rounds, because rounding a *cap* up hands out headroom
nobody set.
