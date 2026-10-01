# syntax=docker/dockerfile:1
# Same multi-stage/distroless/non-root pattern as the main service's
# Dockerfile — this is demo tooling, but there's no reason for it to be
# any less tidy to build or run.
FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/fakevendor \
    ./cmd/fakevendor

FROM gcr.io/distroless/static-debian12:nonroot
USER nonroot:nonroot
COPY --from=build /out/fakevendor /fakevendor
EXPOSE 9090
ENTRYPOINT ["/fakevendor"]
