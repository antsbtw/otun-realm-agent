# 构建阶段
FROM golang:1.24-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /agent ./cmd/agent

# 运行阶段
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata curl

# sing-box（需含 v2ray_api / quic 支持；生产用 install.sh 拉预编译带 tags 的二进制）
ARG SINGBOX_VERSION=1.10.7
RUN set -ex \
    && ARCH=$(uname -m) \
    && case "$ARCH" in \
        x86_64) ARCH="amd64" ;; \
        aarch64) ARCH="arm64" ;; \
    esac \
    && curl -Lo /tmp/sing-box.tar.gz \
        "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${ARCH}.tar.gz" \
    && tar -xzf /tmp/sing-box.tar.gz -C /tmp \
    && mv /tmp/sing-box-*/sing-box /usr/local/bin/ \
    && chmod +x /usr/local/bin/sing-box \
    && rm -rf /tmp/*

WORKDIR /app
RUN mkdir -p /app/data /etc/sing-box
COPY --from=builder /agent /app/agent

ENV OTUN_API_URL=https://otun-manager.situstechnologies.com \
    NODE_API_KEY="" \
    NODE_ID="realm-default" \
    MANAGEMENT_MODE=remote \
    REALM_ID="" \
    REALM_SERVER_URL=https://situstechnologies.com/realm \
    HY2_PORT=51820 \
    SYNC_INTERVAL=60 \
    STATS_INTERVAL=60 \
    SINGBOX_BIN=/usr/local/bin/sing-box \
    SINGBOX_CONFIG=/etc/sing-box/config.json \
    LOG_LEVEL=info

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# hy2 监听口（UDP/QUIC）+ 健康检查口
EXPOSE 51820/udp 8080/tcp

CMD ["/app/agent"]
