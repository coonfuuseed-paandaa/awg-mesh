# Contributing to awg-mesh

## How to Contribute

Contributions are welcome. Before starting work on a non-trivial change, open an issue to discuss
the approach. Check [existing issues](../../issues) first — your problem or idea may already be tracked.

Bug reports should include: OS, Go version, relevant config (redact credentials), and full error output.

## Development Setup

**Prerequisites:**

- Go 1.25+
- `libpcap-dev` (Debian/Ubuntu) or `libpcap-devel` (RHEL/Fedora) — required for CGO
- Docker (for integration tests and image builds)

**Clone and build:**

```bash
git clone https://github.com/coonfuuseed-paandaa/awg-mesh.git
cd awg-mesh

CGO_ENABLED=1 go build -trimpath -o bin/awg-mesh-node ./cmd/awg-mesh-node
CGO_ENABLED=1 go build -trimpath -o bin/mesh-ctl ./cmd/mesh-ctl
```

## Running Tests

**Unit tests (no root required):**

```bash
go test -race ./...
```

**Privileged tests (require root — kernel networking operations):**

```bash
sudo go test -race ./pkg/routing/ ./pkg/node/ -v
```

**Integration tests:**

```bash
go test -tags integration ./tests/integration/ -v
```

Integration tests spin up containers and require Docker to be running.

**Manual end-to-end regression (client ECMP failover + stickiness, US1 + US2):**

```bash
# Linux host with Docker + Compose v2 required
bash tests/client_ecmp/verify.sh
# or via Makefile:
make test-client-ecmp
```

This fixture builds a 4-service stack (2 masters + 1 endpoint + 1 client) on a
user-defined bridge and exercises the failover and session-stickiness paths
described in `.agent/specs/client-ecmp/spec.md`. Not run in CI — requires
privileged Docker. See `tests/client_ecmp/README.md` for the full operator
guide.

## Code Style

Run the linter before committing:

```bash
golangci-lint run ./...
```

Additional conventions:

- Platform-specific code uses file-level build tags: `_linux.go` / `_other.go` suffixes
- Immutable data patterns — never mutate shared structs; return new copies instead
- Errors must be handled explicitly at every level; no silent swallowing
- All input from external sources (topology YAML, gRPC, CLI flags) must be validated at the boundary

## Pull Request Process

1. Fork the repository and create a branch from `main`
2. Keep each PR focused on one feature or fix
3. All CI checks must pass: lint, unit tests, privileged tests, build, Docker image build
4. Update relevant documentation if behavior changes
5. Open the PR against `main` with a clear description of what changed and why

Reviewers may request changes; address them in new commits (do not force-push during review).

## Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/) format:

```
<type>: <short description>

Optional body explaining why, not what.
```

Valid types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`

Examples:
- `feat: add sticky session support to ECMP load balancer`
- `fix: prevent TOCTOU race in cert rotation`
- `docs: document privileged test requirements`

## License

awg-mesh is released under the [MIT License](LICENSE). By contributing, you agree that your
contributions will be licensed under the same terms.
