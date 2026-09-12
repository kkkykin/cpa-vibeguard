#!/usr/bin/env bash
set -euo pipefail
vibeguard_root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$vibeguard_root/bin"
cd "$vibeguard_root"
vibeguard_go_toolchain="$(go env GOVERSION)"
export GOTOOLCHAIN="$vibeguard_go_toolchain"
if [[ -x "$vibeguard_root/bin/caddy" ]] \
    && [[ "$(go version "$vibeguard_root/bin/caddy")" == "$vibeguard_root/bin/caddy: $vibeguard_go_toolchain" ]] \
    && "$vibeguard_root/bin/caddy" version | rg -q '^v2\.10\.2 '; then
    exit 0
fi
GOBIN="$vibeguard_root/bin" go install github.com/caddyserver/caddy/v2/cmd/caddy@v2.10.2
