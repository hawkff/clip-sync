.PHONY: check build integration

check:
	@files="$$(gofmt -l *.go)"; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi
	go vet -tags integration ./...
	go test -race ./...

build:
	CGO_ENABLED=0 go build -trimpath -o bin/clip-sync .

integration: build
	CGO_ENABLED=0 go test -c -tags integration -o bin/integration.test .
	CLIP_SYNC_BINARY="$$(pwd)/bin/clip-sync" sh scripts/integration.sh ./bin/integration.test -test.run TestDesktopBridge -test.v -test.timeout 90s
	CLIP_SYNC_BINARY="$$(pwd)/bin/clip-sync" CLIP_SYNC_XWAYLAND=1 sh scripts/integration.sh ./bin/integration.test -test.run TestCompositorBridge -test.v -test.timeout 90s
