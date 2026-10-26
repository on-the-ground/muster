# Validation record — 2026-10-26

This is current-instance evidence for issue #1 and the PR #2 review corrections,
not approval of a final registration schema or a claim that both real-worker
backends are ready.

## Verified in this cloud instance

- Go 1.25.14 on Linux; MCP Go SDK v1.8.0 and jsonschema-go v0.4.3.
- `go test -race -count=1 -json ./...`: **64 passed, 1 skipped**, including
  subtests. The skipped test is the explicitly opt-in real-worker test.
- `go vet ./...`, `go mod verify`, and `go build -o bin/muster ./cmd/muster`.
- Cross-build for `GOOS=darwin GOARCH=arm64`; macOS runtime execution was not
  available here. Linux uses non-reaping `waitid(WNOWAIT)`; macOS uses `NOTE_EXIT`.
- Both example manifests pass native CLI capability/version checks with Claude
  Code 2.1.310 and Codex CLI 0.163.0. A probe checks actual option declarations,
  records a resolved executable path/version, and does not establish model auth
  or behavioral permission enforcement.
- Generic SDK client and raw MCP stdio tests cover registration/list/call,
  separate fixture processes, exact numeric tokens, invalid inputs/results,
  crashes, deadlines, cancellation, disconnection and blank-line handling.
- Configuration aliases such as `VERSION` and `Executable` are rejected before
  Go struct decoding. General JSON object keys remain case-sensitive, preserving
  valid tool arguments such as distinct `a` and `A` keys.
- Parent Claude/Codex session selectors and an unrelated secret marker are
  absent in the actual fixture child environment. Explicit operator variable
  names are forwarded, forced memory controls cannot be weakened by the parent,
  and fresh fixture sessions differ. Real-native testing now makes two calls
  and asserts that both IDs differ from each other and the parent IDs.
- Backend specifications participate in the contract hash; evidence records
  CLI kind, resolved path and version. Oversized Claude schema arguments are
  refused before launch. Input errors identify the schema field without raw
  values. Journal write/short-write failures latch the journal closed and
  prevent future dispatch.
- Process tests exercise normal-exit descendant cleanup, cancellation of a
  process group, an escaped child holding pipes, and immutable completion facts
  after later cancellation. `ErrWaitDelay` is distinguished as
  `failureKind:worker_io_timeout`; uncertainty returns no successful Value.
  Group absence is required for `workerTerminated:true`. On this instance an
  unreaped orphan zombie can keep a group observable; parent exit alone then
  produces uncertainty instead of a false termination claim.

The fixtures are deterministic subprocesses, **not actual model workers**. Their
flag checks do not prove native model tool or filesystem permission enforcement.

## Claude authentication and memory corrections

The owner selected removal of `--bare` in the
[review](https://github.com/on-the-ground/muster/pull/2#pullrequestreview-5647506094).
The reviewer reported that Claude Code 2.1.310 passed the real registration →
list → call → worker → schema-valid cited result flow on the reviewed baseline
with only `--bare` removed. This is reviewer-reported local evidence, not a
cloud rerun or verification of distinct sessions after the new corrections.

The current adapter uses `--safe-mode` instead: its native help explicitly
states that CLAUDE.md/customizations are disabled while auth works normally.
It retains empty settings sources, disabled hooks, strict empty MCP config,
disabled skills and no session persistence. It also forces
`CLAUDE_CODE_DISABLE_CLAUDE_MDS=1` and `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` in the
filtered worker environment. It no longer forces API-only authentication.

A real 2.1.310 startup probe used an isolated `CLAUDE_CONFIG_DIR` containing a
synthetic user `CLAUDE.md` canary, the production isolation controls and no
credentials. The initialization trace reported `project memory is off`, safe
mode disabling installed plugin hooks and Claude.ai MCP, and did not echo the
canary. This verifies startup controls and observed initialization behavior;
it is not a model-level canary/non-disclosure proof. `--setting-sources ""`
alone is not used as the memory-isolation guarantee. No actual user's memory or
credential files were modified or copied, and raw debug logs are not committed.

The updated native Claude smoke test was attempted in this cloud. Its first
call returned `execution_failed`; native auth status remained `loggedIn:false`.
Evidence recorded Claude 2.1.310, parent exit and confirmed group termination,
without cancellation. Therefore the two-real-session assertion and actual model
memory-denial scenario remain unverified here. Run the opt-in test in an already
authenticated native environment; credentials do not belong in manifests/chat.

## Codex remains experimental

The earlier Codex native attempt reached the real CLI but timed out at 120
seconds. Direct diagnosis observed model connection retries and HTTP 403s with
`chatgpt.com` as destination; no successful structured result was claimed.
The environment draft includes a `chatgpt.com` addition, which alone does not
prove the policy was applied or the endpoint is authorized.

Until real native denial scenarios are observed, the Codex manifest must specify
`binding.experimental:true` and `execution.resources:"host-filesystem-readable"`.
Its MCP catalog description exposes that scope. The OS user may read files
elsewhere on the host, including credential files; a temporary cwd is not a
restricted read boundary. Shell/apply-patch feature keys are requested, not
behaviorally proven. Stronger guarantees require native denial evidence and/or
external filesystem isolation. This follows the review's explicit experimental
alternative rather than describing fixture flags as native enforcement proof.

The optional Subsea adapter and Vessel #67 integration remain separate work.
