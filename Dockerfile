# syntax=docker/dockerfile:1

# Keep this minor in step with the ``go`` directive in go.mod. The
# toolchain here runs with GOTOOLCHAIN=local, so a base image older than
# go.mod's requirement fails at ``go mod download`` with
# "go.mod requires go >= X (running go Y)" — which is how every image
# build broke silently after 0920e45 bumped go.mod to 1.27.0 and left
# this line at 1.26. CI's other jobs use ``go-version-file: go.mod`` and
# adapt on their own; this is the one place the version is hardcoded.
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gateway ./cmd/gateway

# distroless/static runs as nonroot (uid 65532) with a read-only rootfs —
# matching the Deployment's securityContext.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
EXPOSE 8080
ENTRYPOINT ["/gateway"]
