# Implementation evidence

The six delivery steps in PLAN.md remain the acceptance scope. A checked subtask is evidence for that subtask only.

1. Plugin skeleton, configuration, and entrypoint inventory
   - [x] Strict configuration defaults, types, duplicate members, JSON Pointer syntax, and header conditions (`internal/config`, `internal/document`). Verified with `go test ./internal/document ./internal/config`.
   - [x] Native ABI, four capabilities, lifecycle, bounded request state, and sticky bypass. Verified with `go test -race ./...`, `go vet ./...`, `make build`, and `TestNativeABI`.
   - [x] Unmodified pinned CPA loads and registers the shared library; Chat JSON/SSE bypass round trips and successful-envelope business rejection are verified by `TestHostLifecycle` (including asynchronous capacity release).
   - [ ] Full entrypoint inventory and recorded fixtures.
2. Rule engine
   - [x] Unified TOML/YAML/inline documents, official defaults, field-preserving merges, disable unions, literal variable binding, bounded compilation cache, dynamic allowlists, and exact Gitleaks spans (`internal/rules`). `go test -race ./internal/rules ./internal/plugin`, `go vet ./...`, native build, and host integration tests pass.
   - [x] Regressions for capture-group occurrence selection, UTF-8/CRLF/multiline positions, crossing spans, gitleaks:allow comments, generic-rule suppression, invalid/disabled rules, and immutable concurrent bindings.
3. Requests and ordinary responses
   - [x] Transactional random-marker store and exact restoration (`internal/token`): 128-bit random IDs, stable concurrent reuse, scope isolation, rollback, bounded entries/bytes, idle TTL, active leases, and single-pass restoration. Verified with `go test -race ./internal/token`.
   - [x] SourceFormat adapters for Chat/legacy, Responses/compact, Claude/count, Gemini/count, Interactions, Codex/Antigravity, image/video, and SDK search text. Paired fixtures are in `testdata/protocols.json`; selected-field, opaque-data, precision, JSON tool codec, and hint tests pass.
   - [x] Request scope integration, atomic request-error rollback, sticky retries, ordinary-response restoration, and response degradation. All paired fixtures pass through the real C ABI; unmodified host Chat and legacy Completions redact before upstream and restore after execution (`TestHostProtectedChatAndLegacy`).
   - [ ] Host coverage for the remaining formats and native/cross-format routes; session/stream coverage follows below.
4. Sessions and multi-turn requests
   - [ ] Ordered sources, identity isolation, leases, history references, capacity, and idempotence.
5. Streaming and WebSocket
   - [ ] Field deltas, tool JSON, snapshots, terminal events, bounded framing, abnormal stream cleanup, and WebSocket host tests.
6. Release verification and documentation
   - [ ] Full tests, race, vet, shared-library/host tests, reconfiguration/restart/replacement, benchmarks, and observed coverage matrix.
