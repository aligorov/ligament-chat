FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=v0.1.0
ARG BUILD_DATE=2026-11-03
ARG OBFUSCATE=true

WORKDIR /app
COPY go.mod go.sum ./
RUN for i in 1 2 3 4 5; do go mod download && break || sleep 3; done

# git необходим garble для применения патчей к линкеру
RUN apk add --no-cache git && go install mvdan.cc/garble@latest

COPY . .
RUN if [ "$OBFUSCATE" = "true" ]; then \
      echo "==> Сборка с обфускацией Garble (-tiny) под ${TARGETOS}/${TARGETARCH}..." && \
      GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} garble -tiny build -trimpath \
        -ldflags="-w -s -buildid= -X main.Version=${VERSION} -X main.BuildDate=${BUILD_DATE}" \
        -o /app/ligament-chat ./cmd/ligament-chat; \
    else \
      echo "==> Стандартная сборка Go..." && \
      CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags="-w -s -buildid= -X main.Version=${VERSION} -X main.BuildDate=${BUILD_DATE}" \
        -o /app/ligament-chat ./cmd/ligament-chat; \
    fi

FROM alpine:3.21
ARG VERSION=v0.1.0
LABEL org.opencontainers.image.source="https://github.com/aligorov/ligament-chat" \
      org.opencontainers.image.version="${VERSION}"

RUN apk --no-cache add ca-certificates tzdata
RUN addgroup -g 1000 chat && adduser -D -u 1000 -G chat chat
WORKDIR /app
COPY --from=builder /app/ligament-chat /app/ligament-chat
RUN mkdir -p /var/lib/ligament-chat /var/run/postgresql && \
    chown -R chat:chat /var/lib/ligament-chat

VOLUME ["/var/lib/ligament-chat"]
EXPOSE 8097/tcp

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:8097/healthz" || exit 1

USER chat
ENTRYPOINT ["/app/ligament-chat"]
CMD ["-listen", "0.0.0.0:8097", "-data-dir", "/var/lib/ligament-chat", "-db-socket", "/var/run/postgresql", "-db-user", "chat", "-db-name", "ligament_chat"]
