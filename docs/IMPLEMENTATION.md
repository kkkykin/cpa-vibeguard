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
   - [x] Real CPA native Claude, Gemini, Interactions, Responses/Codex, both Codex aliases, compact and compatible compact; representative Chat↔Claude/Gemini and Responses→Chat execution. `TestHostNativeAndCrossProtocolMatrix` checks masked upstream fields and restored client fields.
   - [x] Gemini countTokens, Claude first-party HTTPS count_tokens (local CONNECT/TLS fixture), and Claude custom-gateway local estimation. Both counters consume the protected input (`TestHostClaudeNativeCountAndLocalCount`).
   - [ ] Media/search entrypoint host coverage and recorded boundary inventory.
4. Sessions and multi-turn requests
   - [x] Ordered profiles and header/JSON/metadata sources, trusted identity/namespace isolation, leases, history reference registration, generation-aware expiry, capacity, and retry idempotence (`internal/session`, plugin/session tests).
   - [x] Unchanged history prefixes remain stable across source switches, concurrent requests, and rule-disable reconfiguration. Real CPA authenticated multi-turn prefix stability passes `TestHostSessionHistoricalPrefix`; no claim is made about real upstream cache hits.
5. Streaming and WebSocket
   - [x] Bounded text tails, incremental tool JSON values (keys preserved), and SSE/JSON/array framing. `go test -race ./internal/restore` covers every marker/SSE split, UTF-8/escapes, normal and abnormal tails, duplicate tool keys, and buffer limits.
   - [x] Request-owned stream hook for Chat, Responses/Codex, Claude, Gemini/Antigravity, and Interactions: independent fields, tool deltas, snapshot restoration, terminal flushing, response ID registration, one-time degradation, and abort cleanup. `go test -race ./internal/restore ./internal/plugin ./internal/protocol`, vet, native build, and integration pass. Five actual C ABI stream fixtures are recorded in `testdata/streams.json`.
   - [x] Real CPA Chat/legacy streams cover every marker split (49 positions), parallel choices, byte-sized tool argument deltas, usage, normal partial tails, and abnormal EOF (`TestHostProtectedChatStreamsEverySplit`, `TestHostStreamToolsAndNormalPartialTail`).
   - [x] Native Claude/Gemini/Interactions/Responses streams and representative compatible streams pass all 49 marker split positions in the unmodified host. CPA's newline-free SSE data/event records, Interactions status/signature events, and echoed Responses tool descriptions have regression tests.
   - [ ] Cancel/disconnect and WebSocket host tests.
6. Release verification and documentation
   - [ ] Full tests, race, vet, shared-library/host tests, reconfiguration/restart/replacement, benchmarks, and observed coverage matrix.
