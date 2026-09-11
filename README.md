# cpa-vibeguard

Native CLIProxyAPI plugin for request text redaction and response/tool argument restoration. The implementation contract is [PLAN.md](PLAN.md).

Development requires Go 1.26 and a C compiler. The target host is CLIProxyAPI v7.2.157 (`09a29bd345bc44c473abe7fd07859e32df2ea543`), C ABI 1 / RPC schema 6. Dependencies are pinned; no local module replacements are used.

Run `make test` for unit tests. `make check` also runs the race detector, vet, and the Linux shared-library build. `make integration` runs the native/host integration suite when implemented.

Implementation and verification evidence is tracked in [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md).
