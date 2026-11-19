# Multi-stage build for gv-core. Dependencies are vendored so the
# image builds fully offline (no module proxy inside docker).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -mod=vendor -o /out/gvos-core ./cmd/gvos-core

FROM alpine:3.19
# Spike: root for the TLS bind-mount permission; production uses
# secrets (k8s secret / docker secret) with an unprivileged user.
COPY --from=build /out/gvos-core /usr/local/bin/gvos-core
EXPOSE 8080
ENTRYPOINT ["gvos-core"]
