.PHONY: build test race vet check integration bench

VERSION ?= 0.1.0-dev

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-X github.com/kkkykin/cpa-vibeguard/internal/plugin.Version=$(VERSION)" -o dist/cpa-vibeguard.so ./cmd/cpa-vibeguard

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: test race vet build

integration: build
	bash scripts/build-host.sh
	bash scripts/build-caddy.sh
	go test -tags=integration -count=1 ./tests/integration/...

bench:
	go test ./internal/rules ./internal/restore -run '^$$' -bench . -benchmem -benchtime=200ms -cpu=1
