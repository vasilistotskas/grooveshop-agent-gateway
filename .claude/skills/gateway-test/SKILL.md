---
name: gateway-test
description: Run the agent-gateway Go checks — unit tests, Docker-backed integration tests, lint, and build. Use when asked to test, lint, or verify grooveshop-agent-gateway, or after changing anything under internal/ or cmd/.
argument-hint: "[unit|integration|lint|build|all]"
arguments: [target]
allowed-tools: Read Grep Glob Bash(go test *) Bash(go build *) Bash(golangci-lint run*) Bash(docker info*)
---

Run the checks for `grooveshop-agent-gateway`. Target: `$target`
(default `unit` when no argument was given).

## Commands

| `target` | Command |
|---|---|
| `unit` | `go test ./internal/...` |
| `integration` | `go test -tags integration ./internal/integration/...` |
| `lint` | `golangci-lint run` |
| `build` | `go build ./...` |
| `all` | lint → unit → integration → build, in that order, stopping at the first failure |

CI runs the same sequence: lint → unit + integration → build.

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
