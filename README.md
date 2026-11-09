# muster

**Agree on the work. Define the request. Delegate through MCP.**

muster is an experimental **Agent Proxy MCP** server that owns the agreement
between a caller and an agent worker: **what work can be delegated, how to ask
for it, and what must come back.**

## Why muster?

You want another agent to research a question. Before it can help, someone has
to specify the task: which sources it may use, what counts as evidence, how to
report uncertainty, and what shape the result should take. The caller needs to
know how to ask. The worker needs a request it can understand and act on.
Those expectations need a home that both sides can rely on.

muster puts them in a registered task contract. An operator defines the task's
name, description, input and output schemas, worker instructions, context, and
execution binding. Callers add muster as an MCP server and discover the work
available through `tools/list`. They invoke a named task with structured
arguments; muster assembles the request the worker receives from that contract.

For example, the included `research-brief` contract defines research over
registered local sources. A caller sends:

```json
{
  "name": "research-brief",
  "arguments": {
    "question": "What cancellation guarantees does muster provide?"
  }
}
```

The worker receives the registered research instructions, that question, the
captured sources, and the required result schema. It knows to return a summary,
source-backed evidence, and uncertainties. The caller knows which fields to
expect. muster validates the output and returns the result or an explicit
failure, with execution evidence attached.

The research instructions are registered once. Each caller supplies the question.
Each fresh worker receives the task in the same defined form.

## A shared contract for both sides

This is **dependency inversion for delegation**: the caller and the worker
backend depend on a delegation contract owned by muster. That contract is the
shared point of agreement.

- **The caller can expect:** discoverable tasks, a defined request format, and
  a declared result format. It can delegate without authoring a worker prompt
  or knowing the CLI invocation.
- **The worker can expect:** a request assembled from registered instructions,
  validated arguments, an output schema, and the configured context. It can
  perform the task without understanding each caller's internal representation
  or implementing an MCP server.
- **muster is responsible for:** keeping those expectations aligned, exposing
  the public tool contract, constructing the worker request, and enforcing the
  supported execution and result contracts.

An orchestrator can use this boundary to decide what to delegate and how to use
the result. A worker backend can evolve behind it as long as it can fulfill the
registered contract. Any ordinary MCP client can use the same tasks; muster is
independent of Vessel and Subsea.

Worker pools, CLI adapters, cancellation, result validation, and evidence
recording support that agreement in execution. Today, muster dispatches fresh
Claude or Codex CLI workers. Schema validation checks the result's structure;
the caller still evaluates its substance and decides what to do next.

## Implementation status

This implements the generic MVP proposed in [issue #1](https://github.com/on-the-ground/muster/issues/1).
The manifest and `muster/evidence` record below are **versioned implementation
proposals**, not a final registration API or a Subsea language contract. See
[the manifest contract](docs/manifest.md) for registration details. Go was
selected to reuse the official MCP Go SDK v1.8.0 used by Vessel. Native CLI
workers provide agent execution behind the registered contracts.

## Build and test

Requires Go **1.25.14**, Linux or macOS, and the selected native CLI on PATH.
The default tests use separate deterministic fixture processes and require no
model account or API requests. The fixture is not an agent.

```sh
go mod download
go mod verify
go build -o bin/muster ./cmd/muster
go vet ./...
go test -race -count=1 ./...
```

An opt-in real-worker test makes two model requests (normal account charges may
apply). It runs `research-brief` through a generic SDK client and checks source
citations, two distinct native sessions and separation from parent session IDs:

```sh
mkdir -p .muster
MUSTER_NATIVE_MANIFEST="$PWD/examples/claude.json" \
MUSTER_NATIVE_EVIDENCE="$PWD/.muster/native.jsonl" \
go test ./internal/server -run '^TestNativeWorker$' -count=1 -v
```

Two opt-in web-read tests make real requests that reach the live web. The
first runs `web-research` and checks that searches are recorded as effects and
that every cited URL appeared in a recorded tool result. The second restricts
WebFetch to one domain and checks that a fetch elsewhere is refused natively
and recorded as `denied`:

```sh
MUSTER_NATIVE_WEB_MANIFEST="$PWD/examples/claude-web.json" \
go test ./internal/server -run '^TestNativeWebWorker$' -count=1 -v
# A copy of claude-web.json with "webFetchDomains":["gbis.go.kr"]:
MUSTER_NATIVE_WEB_DENY_MANIFEST="$PWD/.muster/deny.json" \
go test ./internal/server -run '^TestNativeWebFetchDomainDenial$' -count=1 -v
```

Without the opt-in variables these tests are explicitly skipped; offline
conformance does not imply real-worker or credential readiness.

The Claude adapter requires the flags provided by Claude Code 2.1.310; the Codex
adapter requires those provided by Codex CLI 0.163.0, notably
`exec --ignore-user-config --ignore-rules --ephemeral`. Startup probes the actual
CLI help using option-declaration boundaries, records the CLI version, and refuses missing flags. It does not silently fall back to weaker
execution settings. A capability check does not validate model authentication.
Use an existing authenticated native CLI; muster never collects keys or logs in.
Claude uses `--safe-mode`, which preserves native OAuth/subscription and API
authentication while disabling user customizations. It also disables memory
discovery explicitly. Codex retains native authentication while ignoring user
configuration. Workers receive a backend-specific environment allowlist, never
the complete proxy environment; parent session identifiers are always excluded.
Additional provider variable names can be registered by the operator through
`backends.<id>.environmentAllowlist` (names only, never secret values).

## Run a registered research tool

The supplied tool researches the **registered local sources**, with explicit
uncertainties. It does not claim a fresh web search. Edit a copy of a manifest
to register your own task, schemas and context; update the instruction SHA-256
whenever its exact UTF-8 text changes.

```sh
mkdir -p .muster
bin/muster -manifest examples/claude.json -check
bin/muster -manifest examples/claude.json -evidence .muster/jobs.jsonl
# Experimental second adapter; the host filesystem remains readable:
bin/muster -manifest examples/codex.json -evidence .muster/jobs.jsonl
```

Configure any MCP stdio client with the absolute path to `bin/muster` and the
arguments `-manifest /absolute/path/examples/claude.json -evidence
/absolute/path/.muster/jobs.jsonl`. Its working directory need not be this
repository. Relative context paths resolve against the manifest's directory.
Use one server process per client connection. Stdout is MCP messages only.

`tools/list` exposes `research-brief`. For `tools/call`, use:

```json
{"name":"research-brief","arguments":{"question":"How does muster handle cancellation and automatic retries?"}}
```

### Web research

`examples/claude-web.json` registers `web-research`, a **web-read** tool: the
worker may use only `WebSearch` and `WebFetch`, and every search and fetch is
recorded (query or URL, times, status, result hash) in the job's evidence. It
also configures a pool of two workers with a queue of four, and records the
`subsea/callRef` request `_meta` value for callers that send one.

```sh
bin/muster -manifest examples/claude-web.json -evidence .muster/jobs.jsonl
```

```json
{"name":"web-research","arguments":{"question":"Which red bus routes run between Suji-gu and Gangnam Station?"}}
```

Web read is an external effect: searches and fetches reach other services,
and the evidence keeps queries and URLs in clear. The host's network policy
still decides which hosts a fetch can reach; a blocked fetch is recorded as an
`error` effect. Restrict fetches with `binding.webFetchDomains`.

Success includes the schema-valid object in `structuredContent`, identical JSON
in text content, and `_meta["muster/evidence"]`. Failures set `isError:true`
and provide a stable failure code without returning a success-shaped object.
Input errors also identify the schema field and expected type without echoing
its raw value.

## Contract and execution limits

- Registration is trusted startup configuration, never worker output or call
  arguments. There is no registration RPC or reload. `tools.listChanged` is not
  advertised; restart and explicitly rediscover after a catalog change.
- Schemas use a deliberately closed profile: explicit types, object properties,
  required fields, `additionalProperties:false`, array items, title/description.
  All output properties are required. Other keywords, references, unions,
  enum/const and numeric bounds are rejected at load time. See
  [the manifest contract](docs/manifest.md) for the exact boundary and rationale.
- Arguments, schemas and results preserve JSON number tokens. Duplicate keys at
  any depth are rejected, including escaped spellings of the same key. Malformed
  JSON-RPC frames terminate the transport before dispatch; blank lines are skipped.
  Configuration keys must match their JSON tags exactly. There is no output
  repair or automatic redispatch.
- Claude tools have one of two profiles. **no-tools**: builtin tools and MCP
  servers are disabled by CLI flags. **web-read**: only `WebSearch` and
  `WebFetch` exist, `dontAsk` refuses uses the allow rules do not cover, and an
  undeclared tool in the session or in a tool use stops the job
  (`permission_violation`). A job records at most 256 tool uses; one more stops
  it (`audit_limit_exceeded`, `auditIncomplete:true`). In both, `--safe-mode`, empty settings sources, disabled
  hooks/skills and forced memory controls exclude inherited CLAUDE.md and auto
  memory while retaining native authentication, and MCP servers stay disabled. Admin-managed policy still
  applies. This is not an OS sandbox around the Claude CLI itself.
- **Codex is experimental.** The manifest must opt in with
  `binding.experimental:true` and `execution.resources:"host-filesystem-readable"`.
  Its catalog description warns clients about this scope. A temporary cwd does
  not restrict reading other files accessible to the worker's OS user, including
  credential files. The adapter requests the native read-only sandbox, no
  approvals, disabled search and shell/apply-patch feature settings, but native
  denial scenarios and those feature keys remain behaviorally unverified. It
  does not advertise a confined read scope or a proven no-tools guarantee.
  Codex has no web profile. No adapter accepts write or state-changing tasks
  yet, or arbitrary per-call CLI flags.
- Native CLI control-plane activity (model requests, authentication, caches,
  billing) is separate from the declared **task** effect. The operator
  is responsible for account, filesystem and network isolation around the CLI.
  No restricted read scope, network isolation, rollback, session reuse, or
  permission guarantee beyond the listed native controls is advertised.
- **Credit-based pool.** `pool.workers` slots (default 1) are shared by all of
  the instance's tools. A free slot takes a call at once; otherwise it waits in
  a FIFO queue of `pool.queueLimit` calls (default 0) and the next released slot
  pulls it. Beyond that the call is refused as `overloaded` before any worker
  starts. There is no wait deadline; `timeoutSeconds` counts from dispatch. A
  queued call that is cancelled ends `cancelled_before_dispatch`: no worker ran.
  Use one muster instance per MCP server you want to expose. Cancellation,
  disconnection and shutdown stop/reap the worker process group on a best-effort
  basis; a cancellation request and observed local exit are distinct evidence.
  Cleanup also runs on normal exit, before the reserved group leader is reaped.
  `parentExited` and confirmed process-group absence are separate facts; a
  lingering group or output pipe produces uncertainty. Remote work may outlive
  local termination. Clients own retry decisions.
- Evidence (version 2) is private JSONL, mode 0600: `job-admitted`,
  `job-dispatched`, `job-finished`, with versions, hashes, job/backend/session
  correlation, configured request `_meta` correlation values
  (`correlation.metaKeys`), resolved executable path/version, context capture
  times, queue position, status, cancellation/exit facts and web-read effects. A journal write or sync failure latches
  the journal closed; subsequent calls cannot dispatch workers.
  Prompts, raw worker streams, credentials, input/result bodies and environment
  values are excluded; web-read queries and URLs are the recorded exception. Results and sources are available in the MCP response;
  local evidence keeps their hashes. See [design](docs/design.md).

No HTTP transport, hot reload, MCP Tasks, session reuse, resident or externally
attached workers, write effects, Subsea Host policy extension, `/N` streaming
conversion, Runtime settlement or replanning logic is implemented. Vessel #67
integration is tracked there.
