---
name: gateway-test
description: Run the agent-gateway Go checks — unit tests, Docker-backed integration tests, lint, and build. Use when asked to test, lint, or verify grooveshop-agent-gateway, or after changing anything under internal/ or cmd/.
argument-hint: "[unit|integration|lint|build|all]"
arguments: [target]
allowed-tools: Read, Grep, Glob, Bash
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

CI runs the tests with `-race`, but the local Windows Go toolchain is 386 and
**cannot** race-detect. Adding `-race` here fails with a toolchain error that
looks like a test failure. Run it in CI, not locally.

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
- If a DTO decode test failed, suspect drift against
  `grooveshop-django-api/schema.yml` and check whether the fixtures under
  `testdata/fixtures/django/` need refreshing — that is the designed signal,
  not a flake.
