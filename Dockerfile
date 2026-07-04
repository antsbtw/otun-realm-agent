# 构建阶段
FROM golang:1.25-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# ★egress 内嵌六协议：带 with_utls（reality 借壳握手需要 utls）。无需 sing-box 二进制。
RUN CGO_ENABLED=0 GOOS=linux go build -tags with_utls -ldflags="-s -w" -o /agent ./cmd/agent

# 运行阶段
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata curl

WORKDIR /app
RUN mkdir -p /app/data
COPY --from=builder /agent /app/agent

ENV OTUN_API_URL=https://otun-manager-v3.situstechnologies.com \
    FLEET_API_URL="" \
    NODE_API_KEY="" \
    NODE_ID="realm-default" \
    MANAGEMENT_MODE=remote \
    REALM_ID="" \
    SYNC_INTERVAL=60 \
    STATS_INTERVAL=60 \
    LOG_LEVEL=info

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# 六协议本地 UDP 端口（打洞用，对外无意义）+ 健康检查口。
EXPOSE 51820/udp 51821/udp 51822/udp 51823/udp 51824/udp 51825/udp 8080/tcp

CMD ["/app/agent"]
