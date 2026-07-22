#!/usr/bin/env bash
#================================================================
# 多平台视频下载器 - 一键部署脚本 (Go + Docker)
#
# 用法（任选其一）：
#
#   方式 A：curl 一键安装（推荐）
#   curl -fsSL https://raw.githubusercontent.com/rsxbgdurxbjcx-arch/bilibili-downloader/main/bootstrap.sh | bash
#
#   方式 B：wget 一键安装
#   wget -qO- https://raw.githubusercontent.com/rsxbgdurxbjcx-arch/bilibili-downloader/main/bootstrap.sh | bash
#
#   方式 C：手动克隆后运行
#   git clone https://github.com/rsxbgdurxbjcx-arch/bilibili-downloader.git
#   cd bilibili-downloader
#   docker compose up -d --build
#
# 环境变量（可选）：
#   INSTALL_DIR  安装目录，默认 $HOME/bilibili-downloader
#   PORT         服务端口，默认 443（HTTPS，自签名证书）
#   DOMAIN       可选，访问域名（写入证书 SANs）
#   HTTP_ONLY=1  可选，退回纯 HTTP 模式
#================================================================

set -e

REPO_URL="https://github.com/rsxbgdurxbjcx-arch/bilibili-downloader.git"
INSTALL_DIR="${INSTALL_DIR:-$HOME/bilibili-downloader}"
PORT="${PORT:-443}"

# ---------- 颜色 ----------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }
step()  { echo -e "${BLUE}[STEP]${NC}  $*"; }

echo ""
echo "========================================"
echo "  多平台视频下载器 一键部署 (Go + Docker)"
echo "  支持：哔哩哔哩/抖音/快手/小红书/Likee/Instagram/YouTube"
echo "========================================"
echo ""

# ---------- 检测包管理器 ----------
detect_pkg_manager() {
    if command -v apt-get &>/dev/null; then echo "apt"
    elif command -v dnf &>/dev/null; then echo "dnf"
    elif command -v yum &>/dev/null; then echo "yum"
    elif command -v apk &>/dev/null; then echo "apk"
    elif command -v pacman &>/dev/null; then echo "pacman"
    else echo "unknown"
    fi
}

pkg_install() {
    local pm
    pm=$(detect_pkg_manager)
    local pkgs=("$@")
    local SUDO=""
    [ "$(id -u)" -ne 0 ] && SUDO="sudo"
    case "$pm" in
        apt)
            $SUDO apt-get update -qq 2>/dev/null || true
            $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${pkgs[@]}"
            ;;
        dnf)  $SUDO dnf install -y -q "${pkgs[@]}" ;;
        yum)  $SUDO yum install -y -q "${pkgs[@]}" ;;
        apk)  $SUDO apk add --no-cache "${pkgs[@]}" ;;
        pacman) $SUDO pacman -S --noconfirm --needed "${pkgs[@]}" ;;
        *)
            error "未识别的包管理器，请手动安装: ${pkgs[*]}"
            exit 1
            ;;
    esac
}

# ---------- 确保 git ----------
ensure_git() {
    if command -v git &>/dev/null; then return 0; fi
    step "安装 git..."
    pkg_install git
    info "git 安装完成"
}

# ---------- 确保 docker ----------
ensure_docker() {
    if command -v docker &>/dev/null; then return 0; fi
    step "安装 Docker..."
    local pm
    pm=$(detect_pkg_manager)
    case "$pm" in
        apt)
            pkg_install ca-certificates curl gnupg lsb-release
            local SUDO=""
            [ "$(id -u)" -ne 0 ] && SUDO="sudo"
            $SUDO mkdir -p /etc/apt/keyrings
            curl -fsSL https://download.docker.com/linux/$(. /etc/os-release; echo "$ID")/gpg | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg 2>/dev/null || true
            echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$(. /etc/os-release; echo "$ID") $(lsb_release -cs) stable" | $SUDO tee /etc/apt/sources.list.d/docker.list > /dev/null
            $SUDO apt-get update -qq
            $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin
            $SUDO systemctl enable --now docker
            ;;
        *)
            warn "请手动安装 Docker: https://docs.docker.com/engine/install/"
            exit 1
            ;;
    esac
    info "Docker 安装完成"
}

# ---------- 克隆或更新仓库 ----------
clone_or_update() {
    if [ -d "$INSTALL_DIR/.git" ]; then
        step "仓库已存在，拉取最新代码..."
        cd "$INSTALL_DIR"
        git fetch --all -q
        git reset --hard origin/main -q 2>/dev/null || git reset --hard origin/master -q 2>/dev/null || true
        info "代码已更新到最新版本"
    else
        step "克隆仓库到 $INSTALL_DIR ..."
        if [ -d "$INSTALL_DIR" ]; then
            warn "目录已存在但非 git 仓库，备份后重新克隆"
            mv "$INSTALL_DIR" "${INSTALL_DIR}_backup_$(date +%s)"
        fi
        git clone --depth 1 "$REPO_URL" "$INSTALL_DIR"
        cd "$INSTALL_DIR"
        info "仓库克隆完成"
    fi
}

# ---------- Docker 构建并启动 ----------
docker_up() {
    step "Docker 构建并启动..."
    cd "$INSTALL_DIR"

    # 如果有 PORT 环境变量且非默认值，修改 docker-compose.yml 端口映射
    if [ "$PORT" != "443" ]; then
        sed -i "s/\"443:443\"/\"${PORT}:443\"/" docker-compose.yml
        sed -i "s/PORT=443/PORT=${PORT}/" docker-compose.yml
    fi

    # 检查是否需要 sudo
    local DOCKER_CMD="docker"
    if [ "$(id -u)" -ne 0 ] && ! docker ps &>/dev/null; then
        DOCKER_CMD="sudo docker"
    fi

    $DOCKER_CMD compose up -d --build
    info "Docker 容器已启动"
}

# ---------- 等待服务就绪 ----------
wait_for_service() {
    step "等待服务启动..."
    local max_wait=60
    local waited=0
    while [ $waited -lt $max_wait ]; do
        if curl -skf "https://localhost:${PORT}/api/health" >/dev/null 2>&1 || curl -sf "http://localhost:${PORT}/api/health" >/dev/null 2>&1; then
            info "服务已就绪！"
            echo ""
            echo "========================================"
            echo "  ✅ 部署成功！"
            echo "  🌐 访问地址: https://localhost:${PORT}（自签名证书，浏览器提示风险时选择继续访问）"
            echo "  🌐 域名访问: 解析域名到本机后直接访问 https://你的域名"
            echo "  ☁️  Cloudflare 代理: SSL/TLS 模式请设为 Full（源站为自签名证书）"
            echo "  📋 查看日志: cd $INSTALL_DIR && docker compose logs -f"
            echo "  🛑 停止服务: cd $INSTALL_DIR && docker compose down"
            echo "========================================"
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
        echo -ne "\r  等待中... ${waited}s"
    done
    echo ""
    warn "服务未在 ${max_wait}s 内就绪，请检查日志: cd $INSTALL_DIR && docker compose logs"
}

# ---------- 主流程 ----------
main() {
    ensure_git
    clone_or_update
    ensure_docker
    docker_up
    wait_for_service
}

main "$@"
