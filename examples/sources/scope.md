# muster scope notes

Source: the initial implementation of on-the-ground/muster issue #1. This is
registered local source material, not evidence of a remote or current web lookup.

- muster publishes an operator-configured static tool catalog over MCP stdio.
- Each accepted call assembles registered instructions, the arguments JSON,
  the output schema, and context captured when the manifest was loaded.
- Claude and Codex are separate CLI adapters. Every call starts a fresh process
  and session. No call is automatically retried.
- The proxy validates structured results and returns failures with isError.
  Schema validity does not establish factual accuracy.
- Cancellation requests process-group termination. It does not roll back any
  action or establish that a remote model request was cancelled or unbilled.
- This MVP admits one job at a time; concurrent calls receive busy.
- Session reuse, dynamic registration, HTTP, MCP Tasks, worker pools and the
  optional Subsea policy extension are deferred.
