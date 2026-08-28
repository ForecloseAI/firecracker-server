# Repository Guidelines

## Project Structure & Module Organization

This Go 1.27 repository builds a Firecracker microVM control plane and its guest agent. Executables live under `cmd/`: `cracked` manages VMs, `chat` serves the user-facing chat API, and `agentd` runs inside guests. Keep implementation details in the matching `internal/` package—for example, VM lifecycle code belongs in `internal/vm`, Firecracker socket calls in `internal/fc`, HTTP handlers in `internal/api`, and chat behavior in `internal/chat`. Guest-image inputs and systemd units are under `rootfs/`; host provisioning scripts are in `scripts/`; deployment units and Caddy configuration are in `deploy/`. Static HTML is colocated with its server package under `internal/*/static`.

## Build, Test, and Development Commands

- `make build` cross-compiles the `cracked` and `cracked-chat` binaries for Linux/amd64 into `bin/`.
- `make build-agentd` builds the guest daemon into `rootfs/files/agentd`.
- `make test` runs all Go tests with `go test ./...`.
- `make vet` runs the standard Go static analyzer across all packages.
- `make fmt` applies `gofmt` to the repository.
- `scripts/build-rootfs.sh` builds the Docker-based guest filesystem; it requires Docker and privileged filesystem tools.

Run `make test vet` before submitting changes. Use `make install` or `make install-chat` only when intentionally updating host binaries under `/usr/local/bin`.

## Coding Style & Naming Conventions

Follow idiomatic Go and accept `gofmt` output (tabs for indentation). Use short, lowercase package names and descriptive exported identifiers. Keep platform-specific behavior behind focused packages rather than expanding command entrypoints. Shell scripts should use Bash, quote expansions, and retain strict mode (`set -euo pipefail`) where present.

## Testing Guidelines

Tests use Go's `testing` package and sit beside production code as `*_test.go`. Name tests `TestBehavior` or `TestComponent_Behavior`; prefer table-driven cases for input variations. Add regression tests in the package that owns the changed behavior. There is no stated coverage threshold, but new paths and error handling should be exercised.

## Commit & Pull Request Guidelines

Recent commits use concise, imperative subjects, often scoped by subsystem, such as `agentd: ...` or `chat: ...`. Keep each commit focused. Pull requests should explain the behavior change, list verification commands, link relevant issues, and call out deployment or rootfs impacts. Include screenshots for dashboard or chat UI changes and document any new environment variables or security implications.

## Security & Configuration

`cracked-chat` verifies Supabase access tokens against the project's public keys
(`SUPABASE_URL`, fetched from its JWKS endpoint at startup). It holds no user
store and no signing secret: identity lives in Supabase, and a user's VM id is
derived from their Supabase user id rather than recorded anywhere. Do not
reintroduce a local list of logins.

Never commit bearer tokens, credentials, workspace images, or generated binaries. Preserve hostname-based cookie isolation described in `README.md`; in particular, do not introduce cookies scoped to `.usetypeo.com`.
