BIN := cracked
GOFLAGS := CGO_ENABLED=0

.PHONY: build test vet fmt install clean

build:
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -o bin/$(BIN) ./cmd/cracked

test:
	$(GOFLAGS) go test ./...

vet:
	$(GOFLAGS) go vet ./...

fmt:
	gofmt -l -w .

# Only the copy needs root. Keeping `go build` unprivileged matters: sudo resets
# PATH, so a fully-elevated build would not find /usr/local/go/bin/go.
install: build
	sudo install -m 0755 bin/$(BIN) /usr/local/bin/$(BIN)

clean:
	rm -rf bin
