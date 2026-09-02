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
budget, and the eight things a cell must not do. It is normative, and it was
written before any daemon code — it is what lets a Micro cell built today join a
federation built later, and it is expensive to retrofit once cells exist and have
written state.

Then run the demo:

```sh
go run ./cmd/factory-demo
```

Four phases against in-memory fakes: a Mini cell attempts and a Micro cell
independently verifies; gated promotion evaluates every condition and acts on
none of them; a partition where both cells attempt the same task and both
attempts survive reconnect; and autonomous promotion, earned per scope, stopped
two different ways by the kill switch. No varvig binary, no GPU, no network.

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

## Tiers

| Tier | Inference | Recommended roles |
|---|---|---|
| **Micro** | CPU-local small model | `verify`, `build` — *not* `attempt` by default |
| **Mini** | GPU-local model | `attempt`, `verify`, `build` |
| **Medium** | N cells + upstream peer | Mixed; federation-wide |

Tiers are **configuration profiles, not code paths**. Micro and Mini differ only
in which model runtime and budget the config names; `profile.Wire` — the one
function that turns a config into a running cell — never reads the profile name,
and [a test](./profile/profile_test.go) reads its syntax tree to prove it. If a
tier ever requires a branch in the code, the abstraction has failed.

### Micro's honest role

A CPU-local model authoring code will lose nearly every selection while still
consuming review attention — net negative. Micro is strong as a **verification
and build cell**: deterministic work, cheap, no model-quality problem, and it
makes the old-hardware story genuinely compelling rather than aspirational.

So Micro ships with `roles: ["build", "verify"]` and attempting is opt-in.

### Independent verification is a federation feature

Because a cell can verify without attempting, evidence can be produced by a
**different cell than the one that authored the attempt**. This is what makes
autonomous promotion defensible: a signature proves who asserted a test result,
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

A factory key is an `allowed_keys` entry like any other, scoped by path and
revocable by deleting the line:

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

Two more, which are not additions to the policy but the surrounding facts without
which the five would be checking a promotion that could not happen anyway: the
trust store must actually grant `promote` at that path, and the policy module
must return `promote`. An **unconfigured gate is not an approving gate** — the
promotion rule has to be a reviewed, versioned object, and "there isn't one" is
not consent.

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
claim/               claim policy: should this cell attempt this ticket?
loop/                the ten-step cell loop, and verification of peer attempts
gate/                the wasm promotion-policy module interface
agreement/           the promotion-agreement metric, per scope
promote/             both modes, the five conditions, the kill switch
profile/             micro/mini/medium as configuration, and the wiring
conformance/         the nine numbered tests of the spec's §9
guard/               build-failing guards: no second scheduler, no tier
                     branching, no third-party dependencies
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

The nine numbered tests of the spec's §9 live in
[`conformance/`](./conformance/conformance_test.go), named for the items they
hold:

| # | Test | What it holds |
|---|---|---|
| 1 | `Test01_TierEquivalence` | Micro and Mini configs, one binary, same lifecycle |
| 2 | `Test02_PartitionDuplicates` | two partitioned cells both attempt; both attempts survive reconnect |
| 3 | `Test03_OfflineAttempt` | full lifecycle with upstream unreachable, then reconcile |
| 4 | `Test04_EnvironmentDeterminism` | identical adapters, identical hash; different ground, different hash |
| 5 | `Test05_SelfVerificationRefusal` | self-verified attempts are never autonomously promoted |
| 6 | `Test06_BudgetHalt` | stops claiming at the cap, and does not downgrade the model |
| 7 | `Test07_KillSwitch` | mode flip with no restart; `allowed_keys` revocation |
| 8 | `Test08_AgreementRateGate` | refuses below threshold, with the numbers |
| 9 | `Test09_NoSecondScheduler` | submits the declared scope; never derives one |

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

`varvigcli` also has tests that drive the actual `varvig` binary. They **skip**
when one is not on `PATH`, so CI stays green without it, and they run for anyone
who has one:

```sh
go build -o /usr/local/bin/varvig ./cmd/varvig   # in a varvig/varvig checkout
go test -run Integration ./varvigcli/
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
