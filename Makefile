.PHONY: build test race vet check integration

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/cpa-vibeguard.so ./cmd/cpa-vibeguard

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: test race vet build

integration: build
	bash scripts/build-host.sh
	go test -tags=integration -count=1 ./tests/integration/...
