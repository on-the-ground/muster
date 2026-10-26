# Validation record — 2026-10-26

This is current-instance evidence for the initial issue #1 implementation,
not approval of a final registration schema or a claim of real-worker readiness.

## Verified

- Go 1.25.14 on Linux; official MCP Go SDK v1.8.0 and jsonschema-go v0.4.3.
- `go test -race -count=1 -json ./...`: **49 passed, 1 skipped** (including
  subtests). The skipped test is the explicitly opt-in real-worker test.
- `go vet ./...`, `go mod verify`, and `go build -o bin/muster ./cmd/muster`.
- Both example manifests load and pass native CLI capability checks with
  Claude Code 2.1.310 and Codex CLI 0.163.0. Authentication is not part of `-check`.
- Raw MCP stdio and an unmodified official generic SDK client exercise
  registration/list/call through separate fixture processes for both bindings.
- Exact large integer and decimal/exponent tokens, duplicate JSON keys, invalid
  inputs, invalid outputs, backend crashes, timeouts, cancellation, disconnection,
  fresh sessions, fail-closed evidence writes, no redispatch and private stderr.
- A Linux cancellation test observes that a spawned descendant also terminates
  with the native process group. Fixture isolation-flag checks are separate from
  any claim about real model tool enforcement.

The fixtures exercise the native CLI protocols; they are deterministic test
programs, **not actual Claude or Codex model executions**.

## Real worker: blocked, not passed

The opt-in test was separately attempted with the registered `research-brief`
tool and existing Codex ChatGPT login. `tools/list` and `tools/call` reached the
actual separate CLI worker. It did not produce a result within the manifest's
120-second deadline. Evidence recorded `timeout`, `cancelRequested:true` and
`workerTerminated:true`. No successful structured result is claimed.

A shorter direct CLI diagnosis using the same read-only controls observed
`thread.started`, model connection retries/errors and HTTP 403 responses with
`chatgpt.com` as the destination. The running environment's allowed destinations
did not include that host. The environment draft now preserves the existing
`storage.googleapis.com` rule and adds **`chatgpt.com`**. Draft saving alone
does not apply the policy or publish the environment. Review/save the network
setting before retrying the opt-in test documented in the README.

Claude native authentication reports no login, and no Anthropic credential
binding was present. The selected `--bare` profile needs a supported API/provider
credential through the worker's normal secure setup; do not place it in a
manifest, prompt, evidence file or chat. A real Claude result and actual native
permission-denial scenarios remain unverified.

The optional Subsea policy adapter, Vessel #67 integration, live web research,
worker pools and remote publication were not run or implemented. Issue #1's
real-worker completion evidence therefore remains outstanding; the offline
MVP and its local lifecycle tests are reviewable independently.
