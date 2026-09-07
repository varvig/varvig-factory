# The Cell Contract

*Normative. Version 2* — adds authority (§8.1), effectful capabilities (§8.2),
and the implementation status in §11. Section references in the form §N.N refer
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
| `refs/attempts/<cell-id>/<task-id>/<n>` | One immutable attempt, `n` counting from 1 |
| `refs/claims/<cell-id>/<task-id>` | An advisory, TTL'd claim (§5) |
| `refs/pins/<cell-id>/…` | Retention requests — varvig's own pin namespace (`FEDERATION.md` §4) |
| `refs/envelopes/<overseer-id>` | The spend ceilings one overseer set, shared across its cells (§8.1) |
| `refs/leases/<cell-id>/<capability>` | One exclusive allocation drawn from an envelope (§8.1) |
| `refs/reservations/<cell-id>/<idempotency-key>` | An effectful action's reservation, keyed by its derived idempotency key (§8.2) |
| note namespace `factory/evidence` | Evidence for an attempt (§4) |
| note namespace `factory/environment` | The environment descriptor an evidence record was produced in (§4.2) |
| note namespace `factory/artifact` | *Legacy.* `artifact-ref` records, for a core without `tickets attach-artifact` (§7) |
| note namespace `factory/agreement` | Promotion-agreement observations, per scope (§8) |

`<task-id>` is the varvig ticket id — the genesis intent revision hash, stable
forever (`TICKETS.md` §1.2). Factory does not mint its own task identity.

Two rules about these names:

- **Attempt refs are immutable.** An attempt ref is created once and never
  moved. A second attempt at the same task is `…/2`, not a new value at `…/1`.
  This is what makes "both attempts survive reconnect" (§9.2) a property of the
  naming rather than of a merge algorithm.
- **Notes attach to the attempt's change hash**, never to a ref. Evidence
  bound to a ref would silently re-target; bound to a hash it stays attached to
  the thing that was actually measured.

---

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
  "roles": ["attempt", "verify", "build"]
}
```

| Field | Rule |
|---|---|
| `cell_id` | Matches the identity of §1. Required. |
| `inference.tier` | `none`, `small`, `large`. `none` is legal and normal — a cell with no model can still verify and build. |
| `inference.models` | Sorted by `(id, version)`. Empty iff tier is `none`. |
| `build`, `test` | Sorted, deduplicated capability tokens. Free-form, matched by equality against a ticket's requirements. |
| `roles` | A non-empty subset of `attempt`, `verify`, `build`, sorted. |

**`roles` is the load-bearing field.** A cell may be a verifier or builder
without ever attempting (§2.1, §3.3). Micro ships `["build", "verify"]`;
attempting is opt-in, because a CPU-local model authoring code loses nearly
every selection while still consuming review attention (§3.1).

Encoding is canonical JSON as defined in §4.3, so two cells configured
identically publish byte-identical capabilities.

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

A claim is a ref at `refs/claims/<cell-id>/<task-id>` holding:

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

---

## 6. Adapter seams

Three seams, and everything hardware- or vendor-shaped lives behind them, so
that neither varvig nor Factory's core loop ever learns about CUDA,
quantization, or container runtimes (§4).

| Seam | Contributes to the environment |
|---|---|
| Model runtime | `model` and the inference toolchain entry |
| Build sandbox | `platform`, the toolchain versions it exposes, outcome-affecting flags |
| Artifact store | the `container` reference, when the sandbox has one |

Each adapter reports a deterministic fragment (§4.2), and an adapter reports its
fragment **from the environment it will actually run in** — a fragment read from
configuration rather than from the running toolchain is a claim, not a
measurement.

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

A cell declares a spend cap and halts when it is exceeded. This is not optional:
attempts multiply cost, and a disconnected cell claiming speculatively can burn
budget on work that proves duplicative (§7).

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
| Ref | `refs/envelopes/<overseer-id>` | `refs/leases/<cell-id>/<capability>` |
| Enforceable from a stale view? | No — another cell may have spent it | Yes — nobody else can spend it |
| Spendable offline | No | Yes, indefinitely |

The capability is hex-encoded in a lease ref, because a capability alias
contains `@` and a ref path component should not carry an alias's punctuation.

```json
{ "overseer": "overseer-a", "set_at": 1755820800,
  "ceilings": [
    { "capability": "pcb-fabrication@1", "spend": 5000, "unit": "EUR",
      "quantity": 100, "rate_per_day": 4 },
    { "capability": "human-contract@1",  "spend": 2000, "unit": "EUR" }
  ] }
```

```json
{ "cell_id": "mini-a", "capability": "pcb-fabrication@1",
  "overseer": "overseer-a", "envelope": "<envelope object hash>",
  "amount": 1000, "unit": "EUR", "quantity": 20,
  "spent": 320, "ordered": 5,
  "reserved": 400, "reserved_units": 6,
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
| Effectful | Allowed **within an outstanding lease**, offline, indefinitely. Refused with no lease. |

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
4. **Bounded by the envelope, spent from the acting cell's own lease.** Another
   cell's lease is not spendable here, however much headroom it has.
5. **Never auto-retried, never regenerated.** A failed effectful action
   escalates; retry is an authorized decision, not a loop behaviour. A
   conflicting effectful attempt does not re-run, because the external world has
   already moved.

#### The reservation is what actually stops the second order

Deriving a stable key says what "the same action" means. It does not by itself
stop the action happening twice — for that, the key has to be claimed somewhere
that survives the process, **before** the effect is attempted. That place is
`refs/reservations/<cell-id>/<idempotency-key>`, and the claim is create-only:
whoever creates the ref executes, and everyone else finds it already there. That
is varvig's ordinary ref CAS doing the work; nothing here needs a lock.

The order is deliberately the pessimistic one — reserve, execute, settle:

| State | Meaning | Who may clear it |
|---|---|---|
| `pending` | reserved, and **possibly executed** — the cell does not know which | a higher principal, after checking the external system |
| `done` | the effect is confirmed to have happened; carries the far end's own reference | — |
| `failed` | the external service is confirmed to have **rejected** it, so no effect occurred | — |

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
   read/write sets, order concurrent work, or decide serialization. It submits
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

---

## 11. What this contract does not yet cover

The design notes moved ahead of this implementation in three places, and one
representation choice here is worth flagging. They are listed rather than left
to be discovered, because a contract that quietly omits a rule reads exactly
like one that has decided against it.

**Cell classes, not tiers, and factories are flat.** Micro, Mini and Medium
describe *capacity*; a factory is one or more cooperating cells with no
hierarchy. The terminology is corrected throughout this document and the
implementation has no branch on the class name — but one behavioural gap
remains: **there is no designated upstream peer** in the design, and any member
may act as a rendezvous, several at once. The loop still takes a single
`upstream` address. That is enough for a cell to sync and it is not a
coordinator — nothing reads from it that a peer could not serve — but it is not
yet the "any member, several at once" the design asks for, and a factory should
keep working when any particular member is unreachable.

**Interfaces are not yet varvig objects.** A capability reference already binds
to the interface *hash* rather than the alias (§8.2), which is the part that
matters for safety. What is missing is the registry the hash points into:
interfaces published as objects, resolvable by hash, with the alias as a
convenience over the top.

**Reputation is not derived.** Capability claims are advisory, and the design
derives a cell's standing from its promotion history rather than from what it
declares about itself. Today only the agreement-rate metric (§9) is derived, and
it is per scope rather than per cell.

**Money is a float64.** Amounts are `float64` throughout, so ordinary
arithmetic accumulates representation error: 1000 − 320 − 355.40 is 44.600000000
00002, and refusal messages round for display rather than being exact. Nothing
here compares amounts for equality, so no decision turns on it today — but minor
units (integer cents) are the right representation for a system that spends
money, and the display rounding is a patch over the symptom.
