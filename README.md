# muster

An experimental **Agent Proxy MCP** server. An operator registers named task
contracts; a generic MCP client calls those tools with structured arguments.
muster assembles the registered task and dispatches a fresh Claude or Codex CLI
worker, validates its structured output, and records execution evidence.

This implements the generic MVP proposed in [issue #1](https://github.com/on-the-ground/muster/issues/1).
The manifest and `muster/evidence` record below are **versioned implementation
proposals**, not a final registration API or a Subsea language contract. Go was
selected to reuse the official MCP Go SDK v1.8.0 used by Vessel while keeping
this server independent of Vessel and Subsea. Native CLI workers avoid building
another general agent framework.

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

Without the opt-in variable this test is explicitly skipped; offline
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
- Claude currently supports **no model tools**: builtin tools and MCP servers
  are disabled by CLI flags. `--safe-mode`, empty settings sources, disabled
  hooks/skills and forced memory controls exclude inherited CLAUDE.md and auto
  memory while retaining native authentication. Admin-managed policy still
  applies. This is not an OS sandbox around the Claude CLI itself.
- **Codex is experimental.** The manifest must opt in with
  `binding.experimental:true` and `execution.resources:"host-filesystem-readable"`.
  Its catalog description warns clients about this scope. A temporary cwd does
  not restrict reading other files accessible to the worker's OS user, including
  credential files. The adapter requests the native read-only sandbox, no
  approvals, disabled search and shell/apply-patch feature settings, but native
  denial scenarios and those feature keys remain behaviorally unverified. It
  does not advertise a confined read scope or a proven no-tools guarantee.
  Neither adapter accepts effectful tasks or arbitrary per-call CLI flags.
- Native CLI control-plane activity (model requests, authentication, caches,
  billing) is separate from the declared read-only **task** effect. The operator
  is responsible for account, filesystem and network isolation around the CLI.
  No restricted read scope, network isolation, rollback, session reuse, or
  permission guarantee beyond the listed native controls is advertised.
- At most one accepted job executes at a time. Overlap returns `busy`; no queue
  or worker pool is implemented. Each job has a finite timeout. Cancellation,
  disconnection and shutdown stop/reap the worker process group on a best-effort
  basis; a cancellation request and observed local exit are distinct evidence.
  Cleanup also runs on normal exit, before the reserved group leader is reaped.
  `parentExited` and confirmed process-group absence are separate facts; a
  lingering group or output pipe produces uncertainty. Remote work may outlive
  local termination. Clients own retry decisions.
- Evidence is private JSONL, mode 0600, containing versions, hashes, job/backend/
  session correlation, resolved executable path/version, context capture times,
  status and cancellation/exit facts. A journal write or sync failure latches
  the journal closed; subsequent calls cannot dispatch workers.
  Prompts, raw worker streams, credentials, input/result bodies and environment
  values are excluded. Results and sources are available in the MCP response;
  local evidence keeps their hashes. See [design](docs/design.md).

No HTTP transport, hot reload, MCP Tasks, session reuse, worker pool, Subsea Host
policy extension, `/N` streaming conversion, Runtime settlement or replanning
logic is implemented. The optional Subsea adapter and Vessel #67 integration
remain separate work.
