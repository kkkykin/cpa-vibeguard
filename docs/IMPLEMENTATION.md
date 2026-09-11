# Implementation evidence

The six delivery steps in PLAN.md remain the acceptance scope. A checked subtask is evidence for that subtask only.

1. Plugin skeleton, configuration, and entrypoint inventory
   - [x] Strict configuration defaults, types, duplicate members, JSON Pointer syntax, and header conditions (`internal/config`, `internal/document`). Verified with `go test ./internal/document ./internal/config`.
   - [ ] Native ABI, four capabilities, lifecycle, request state, and sticky bypass.
   - [ ] Unmodified host loading and recorded entrypoint fixtures.
2. Rule engine
   - [ ] Unified TOML/YAML/inline documents, defaults, merging, disable lists, templates, cache, and exact Gitleaks spans.
3. Requests and ordinary responses
   - [ ] Protocol adapters, request mappings, text and tool codecs, native wrappers, and specialized text entrypoints.
4. Sessions and multi-turn requests
   - [ ] Ordered sources, identity isolation, leases, history references, capacity, and idempotence.
5. Streaming and WebSocket
   - [ ] Field deltas, tool JSON, snapshots, terminal events, bounded framing, abnormal stream cleanup, and WebSocket host tests.
6. Release verification and documentation
   - [ ] Full tests, race, vet, shared-library/host tests, reconfiguration/restart/replacement, benchmarks, and observed coverage matrix.
