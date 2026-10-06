# Multi-stage build for gv-core.
# Build context: repo root (see deploy/docker-compose.yml).

FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /out/gvos-core ./cmd/gvos-core

FROM alpine:3.19
RUN adduser -D -H -u 10001 gvos
USER gvos
COPY --from=build /out/gvos-core /usr/local/bin/gvos-core
EXPOSE 8080
ENTRYPOINT ["gvos-core"]
