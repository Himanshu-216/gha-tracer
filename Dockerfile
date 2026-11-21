# syntax=docker/dockerfile:1
# Base images are pinned by digest; Dependabot (docker ecosystem) bumps them.
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
RUN go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/gha-tracer ./cmd/gha-tracer

# Distroless static: no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/gha-tracer /usr/local/bin/gha-tracer
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gha-tracer"]
