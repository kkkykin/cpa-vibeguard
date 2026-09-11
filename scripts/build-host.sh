#!/usr/bin/env bash
set -euo pipefail
vibeguard_root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$vibeguard_root/bin"
cd "$vibeguard_root"
cpa_source_dir="$(go mod download -json github.com/router-for-me/CLIProxyAPI/v7@v7.2.157 | python3 -c 'import json,sys; print(json.load(sys.stdin)["Dir"])')"
cd "$cpa_source_dir"
CGO_ENABLED=1 go build -mod=readonly -trimpath -o "$vibeguard_root/bin/cpa-host" ./cmd/server
