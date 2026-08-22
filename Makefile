BIN := cracked
GOFLAGS := CGO_ENABLED=0

.PHONY: build build-cracked build-chat test vet fmt install install-chat hashpw clean

build: build-cracked build-chat

build-cracked:
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -o bin/$(BIN) ./cmd/cracked

build-chat:
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -o bin/$(BIN)-chat ./cmd/chat

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

install-chat: build-chat
	sudo install -m 0755 bin/$(BIN)-chat /usr/local/bin/$(BIN)-chat

# Prints a users-file line. Password is read from stdin with echo off.
hashpw:
	@test -n "$(USER_NAME)" || { echo "usage: make hashpw USER_NAME=alice"; exit 1; }
	@stty -echo 2>/dev/null; printf "password: " >&2; read p; stty echo 2>/dev/null; echo >&2; \
	 printf '%s\n' "$$p" | go run ./cmd/chat -hashpw $(USER_NAME)

clean:
	rm -rf bin
