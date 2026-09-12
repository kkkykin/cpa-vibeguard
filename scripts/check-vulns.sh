#!/usr/bin/env bash
set -euo pipefail
vibeguard_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$vibeguard_root"
# Versioned go run otherwise selects its toolchain outside this module.
vibeguard_go_toolchain="$(go env GOVERSION)"
export GOTOOLCHAIN="$vibeguard_go_toolchain"
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -show version ./...
# Check the artifact's embedded Go/dependency versions, including packages
# linked through native exports or interfaces that source analysis may miss.
go version dist/cpa-vibeguard.so
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -mode=binary -scan=package dist/cpa-vibeguard.so
