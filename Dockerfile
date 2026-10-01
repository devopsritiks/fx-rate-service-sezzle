# syntax=docker/dockerfile:1

# --- build stage ---
# Full Go toolchain image; discarded after build, none of its size or
# tooling ends up in the final image.
FROM golang:1.25-bookworm AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 produces a statically linked binary with no libc
# dependency, which is required for the distroless/static base below (it
# has no libc at all). -ldflags sets the same version var main.go reads
# via -X, and strips debug symbols (-s -w) to shrink the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/fxservice \
    ./cmd/fxservice

# --- final stage ---
# distroless/static: no shell, no package manager, no coreutils — just
# enough (libc-free CA certs + the binary) to run a static Go binary.
# This shrinks the attack surface relative to even a minimal distro image:
# there's no shell for an attacker to get into even if they find a way to
# execute something in the container, and no package manager to pull
# anything else down.
FROM gcr.io/distroless/static-debian12:nonroot

# The nonroot variant's default user. Explicit here so this is still
# correct even if the base image's default ever changes.
USER nonroot:nonroot

COPY --from=build /out/fxservice /fxservice

EXPOSE 8080

ENTRYPOINT ["/fxservice"]
