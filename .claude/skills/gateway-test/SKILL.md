---
name: gateway-test
description: Run the agent-gateway Go checks — unit tests, Docker-backed integration tests, lint, build, and the real-stack test. Use when asked to test, lint, or verify grooveshop-agent-gateway, or after changing anything under internal/ or cmd/.
argument-hint: "[unit|integration|lint|build|realstack|all]"
arguments: [target]
allowed-tools: Read Grep Glob Bash(go test *) Bash(go build *) Bash(golangci-lint run*) Bash(docker info*) Bash(docker build *)
---

Run the checks for `grooveshop-agent-gateway`. Target: `$target`
(default `unit` when no argument was given).

## Commands

| `target` | Command |
|---|---|
| `unit` | `go test ./internal/...` |
| `integration` | `go test -tags integration ./internal/integration/...` |
| `lint` | `golangci-lint run`, then `go mod tidy -diff` and `go tool govulncheck ./...` |
| `build` | `go build ./...` |
| `realstack` | `go test -tags realstack -count=1 -timeout 40m -v ./internal/realstack/...` (see below) |
| `all` | lint → unit → integration → build, in that order, stopping at the first failure |

CI (`ci.yml`) runs lint (with the tidy and vulnerability checks), the unit
and integration tests, and the build as three parallel jobs; `all` runs
them one after another so the first failure stops the run. `all` leaves
out `realstack`.

## The real-stack test

`internal/realstack` runs the gateway image against a real Django, its
database and its search, including a real sign-in. It takes about 12
minutes and a lot of memory; `.github/workflows/real-stack.yml` runs it
before a release, nightly and on a pull request that changes the test or
the runbook. Locally it needs both images named in the environment:

```bash
docker build -t agent-gateway:real-stack .
REALSTACK_DJANGO_IMAGE=gro0ve/grooveshop-django-api:<release tag> \
REALSTACK_GATEWAY_IMAGE=agent-gateway:real-stack \
  go test -tags realstack -count=1 -timeout 40m -v ./internal/realstack/...
```

Without them the package exits at once (`REALSTACK_DJANGO_IMAGE is
required`): report that, not a failure.

## Do not add `-race` locally

CI runs the tests with `-race`. `-race` requires cgo; the local Go builds
with `CGO_ENABLED=0` and has no C compiler, so `go test -race` fails at once
with `-race requires cgo`. Run it in CI.

## Integration tests need Docker

`-tags integration` tests stand up real dependencies. If Docker is not running
they fail on connection errors rather than assertions. Check Docker first and
say so plainly instead of reporting the failures as code defects.

The `.golangci.yml` sets `build-tags: [integration]`, so lint covers the
integration files even when you only ran the unit tests.

## Reporting

- Give the pass/fail/skip counts.
- For each failure: the test name, the assertion that failed, and the source
  file it points at.
- If `TestDjangoConsumerContract` failed, a DTO or the call registry changed
  without regenerating the contract: run
  `go test ./internal/django -run TestDjangoConsumerContract -update` and
  commit `contract/django-consumer.openapi.json`. That is the designed
  signal, not a flake.
- If a DTO decode test failed, the hand-recorded fixtures under
  `testdata/fixtures/django/` disagree with the DTO.
