# Manifest v1 (implementation proposal)

`examples/claude.json` and `examples/codex.json` are complete working shapes.
Unknown configuration fields, duplicate JSON names, invalid schemas and
unsupported execution guarantees fail startup. There is no caller registration
authority. Keep manifests and context files operator-owned; credentials belong
in the native worker's authentication setup, never manifest instructions/context.

| Field | Meaning |
| --- | --- |
| `version` | Exactly `1`. |
| `backends` | Operator IDs mapped to `kind` (`claude` or `codex`) and `executable` (PATH basename or absolute path). |
| `tools` | 1–100 unique tools. |
| `tools[].name` | Exact case-sensitive name, 1–128 ASCII letters, digits, `_`, `-`, `.`. |
| `description`, `version` | Public task description and nonempty task contract revision. |
| `inputSchema`, `outputSchema` | Object schemas in the closed profile below. Output schema is required in this MVP. |
| `instructions` | Nonempty `version`, `text`, and lowercase `sha256` of exactly the UTF-8 text. |
| `binding.backend` | One configured backend ID. |
| `binding.extraction` | `claude-structured-output` or `codex-output-file`, matching the backend. |
| `binding.context` | Named `{name,path}` text files captured once at startup. Names must be unique. Relative paths use the manifest directory. |
| `execution.permissions` | Claude: `no-tools`; Codex: `read-only-sandbox`. Other claims are refused. |
| `execution.effect` | Exactly `read-only`, referring to task effects, not the CLI control plane. |
| `execution.cancellation` | Exactly `terminate-process-group`; best-effort, without rollback. |
| `execution.reuseSession` | False. Each dispatch uses a fresh process/session. |
| `execution.resources` | Claude: `captured-context`; Codex: `live-workspace`. Captured context is fixed; Codex's host resources are not a fixed revision. Its cwd is a fresh temporary directory, not a checkout snapshot. |
| `execution.timeoutSeconds` | Integer 1–3600. |

The instruction text defines role, goals, evidence needs, data handling and
return semantics. Its version and hash must be maintained together. Matching
tool descriptions and worker instructions remains the operator's semantic
review responsibility; a schema cannot establish that they mean the same thing.

## Closed schema profile

Each schema node has exactly one explicit `type`: `object`, `array`, `string`,
`number`, `integer`, `boolean`, or `null`. Object nodes require `properties` and
`additionalProperties:false`; `required` is a unique subset of the property
names. Every property is required in output schemas, at every object depth.
Array nodes require one `items` schema. `title` and `description` are optional
strings. Container keywords on scalar nodes are rejected.

Other JSON Schema keywords fail registration, rather than being silently
ignored or passed to a backend that may not support them. In particular:
no `$ref`/remote resolution, unions, regexes, numeric bounds, enum or const.
Extending this subset requires validation and backend compatibility evidence.

The Google JSON Schema library validates schema definitions. At runtime,
v0.4.3 classifies `json.Number` as a string. muster therefore checks instances
against this closed profile itself, using token-based exact integer detection;
it never converts a numeric instance or bound to binary64. Large integers,
arbitrary precision decimals and exponent spellings remain unchanged in the
worker request and structured MCP result. This is a profile implementation,
not a claim to implement all of JSON Schema.

## Request and extraction

One deterministic JSON envelope contains `tool`, `version`, `instructions`,
`arguments`, `outputSchema`, and `contextData`. Context includes its source name,
text, SHA-256 and startup observation time. The envelope hash records precisely
what was sent; the proxy does not rewrite the task using another model.
Whitespace may be compacted by JSON marshaling; numeric tokens are preserved.

Claude runs `claude -p --bare --output-format json --json-schema ...` with its native
tools disabled, strict empty MCP configuration, settings sources disabled,
hooks disabled, skills disabled and session persistence disabled. Bare mode also
prevents inherited CLAUDE.md/memory/plugin context and requires API/provider auth
instead of OAuth/keychain login. Its envelope
must have `is_error:false` and `structured_output`; the latter's raw JSON is
extracted. Unexpected/malformed output is a contract violation, never repaired.

Codex runs `codex exec --output-schema FILE --output-last-message FILE --json`
with the controls listed in the README. JSONL events supply the native thread
ID and explicit execution failures; the final output file supplies the raw
result. Progress events and tokens never become output Values. A fresh private
temporary directory owns the schema and result files and is removed after wait.
CLI output streams, each file, inbound JSON frame and assembled request are bounded at
4 MiB; JSON nesting is limited to 128. This is not a whole-process memory quota.
