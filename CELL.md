# The Cell Contract

*Normative. Version 1.* Section references in the form §N.N refer to
`FACTORY.md` (Design Notes VIII) unless another document is named.

This is the interoperability surface of a Factory cell — the part that is
expensive to retrofit once cells exist and have written state (§2). It is
written down before the daemon so that a Micro cell built today can join a
federation built later.

Everything here is either an on-disk shape, a ref name, or a hash rule. Nothing
here is a process, an RPC, or a schedule. A cell that honours this document
federates; how it is implemented internally is its own business.

**There is no central Factory.** Upstream is a varvig peer, nothing more — it
runs no Factory process, holds no queue, and issues no RPCs (§1.1). Every
obligation below is discharged by writing repository state.

---

## 1. Identity

A cell has a **cell id**: a short, stable, lowercase name matching
`^[a-z0-9][a-z0-9-]{0,62}$`. It appears in every ref this document names, so it
is chosen once and never changed — renaming a cell orphans its claims and
attempts.

A cell has a **peer keypair** (varvig identity) and an `allowed_keys` entry in
the repository trust store (`AUTH.md` §2), scoped by path and rights:

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
8. **No tier-specific code path.** Micro and Mini differ only in which model
   runtime and budget the configuration names (§1.2). A branch on tier in the
   code means the abstraction has failed.
