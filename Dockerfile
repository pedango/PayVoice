# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so a source-only change does not re-download
# the module cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off because the entire media path is pure Go: restricting WebRTC to
# G.711 removed the libopus dependency, which is what makes a static binary and
# a scratch image possible.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/pedango ./cmd/pedango

FROM alpine:3.20

# ca-certificates is needed to reach TTS/STT providers and the webhook endpoint
# over TLS. tzdata keeps log timestamps sane.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 pedango

COPY --from=build /out/pedango /usr/local/bin/pedango

USER pedango

# Control API. SIP and the RTP/WebRTC media ranges are published by the
# orchestrator, since a media server needs host networking or an explicit
# range mapping to work through NAT.
EXPOSE 8080/tcp
EXPOSE 5060/udp

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/pedango"]
