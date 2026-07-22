# ============ 阶段1: Go 编译 ============
FROM golang:1.23-alpine AS builder

WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
# 静态编译，CGO禁用，适配 alpine
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o bili-downloader .

# ============ 阶段2: 运行时 ============
FROM python:3.12-slim

LABEL maintainer="bili-downloader"
LABEL description="多平台视频下载器 (Go + yt-dlp + ffmpeg)"

WORKDIR /app

# 安装系统依赖：ca-certificates、curl、aria2（多线程下载加速）、ffmpeg（音视频合并，Debian 官方源，避免访问被墙的 johnvansickle.com）
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    aria2 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# 安装 yt-dlp 最新版（官方 pip 安装）
RUN pip install --no-cache-dir -U yt-dlp

# 复制 Go 二进制
COPY --from=builder /build/bili-downloader /app/bili-downloader

# 创建目录
RUN mkdir -p /app/downloads /app/cookies_store

# 环境变量（与 docker-compose.yml 保持一致：默认 443，HTTPS）
ENV PORT=443
ENV TZ=Asia/Shanghai
# DOMAIN 可选：指定访问域名，会写入自签名证书的 SANs
# ENV DOMAIN=example.com
# HTTP_ONLY=1 可选：退回纯 HTTP 模式（Cloudflare Flexible SSL 或内网直连场景）
# ENV HTTP_ONLY=

# 暴露端口
EXPOSE 443

# 健康检查（HTTPS 自签名证书需 -k 跳过校验）
HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
    CMD curl -kf https://localhost:${PORT}/api/health || exit 1

# 启动
CMD ["/app/bili-downloader"]
