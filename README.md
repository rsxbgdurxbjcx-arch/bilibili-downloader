# 📺 多平台视频下载器

> 基于 **Go + yt-dlp + ffmpeg** 的多平台视频下载器，Docker 一键部署。
> **全平台统一采用 YT-Downloader 的解析逻辑与格式选择策略**，自动下载最高分辨率、最高帧率、最高码率的视频，支持 Cookie 持久化与无感链接提取。

![Go](https://img.shields.io/badge/Go-1.23-00ADD8)
![yt-dlp](https://img.shields.io/badge/yt--dlp-latest-red)
![ffmpeg](https://img.shields.io/badge/ffmpeg-7.0-static-orange)
![Docker](https://img.shields.io/badge/Docker-ready-2496ED)

## 🌐 支持平台

| 平台 | 域名 | 高清需 Cookie | 说明 |
|------|------|:---:|------|
| 哔哩哔哩 | bilibili.com / b23.tv / bili2233.cn | ✅ | DASH 流，1080P+/4K 需登录 |
| 抖音 | douyin.com / v.douyin.com | ✅ | 桌面端 UA 伪装 |
| 快手 | kuaishou.com / chenzhongtech.com / kwai.com | ✅ | generic 提取器 + 移动端 UA |
| 小红书 | xiaohongshu.com / xhslink.com | ✅ | 移动端 UA 伪装 |
| Likee | likee.video | ❌ | 无需登录 |
| Instagram | instagram.com / instagr.am | ✅ | 需登录态获取高清 |
| YouTube | youtube.com / youtu.be | ✅ | 默认 web client，支持 4K/8K |

**以上 7 个平台全部使用统一的 YT-Downloader 解析逻辑与选择策略**（见下方「统一最高画质策略」）。

## ✨ 功能特性

- 🎬 **全平台统一最高画质策略（复制自 [YT-Downloader](https://github.com/rsxbgdurxbjcx-arch/YT-Downloader)）**：
  - `bestvideo+bestaudio/best` 选择器 + `--format-sort res,fps,tbr` 排序
  - 按 `最高分辨率 → 最高帧率 → 最高码率` 选流，一次调用 yt-dlp 完成选流、下载、ffmpeg 合并（mkv 容器）
- 🚀 **哔哩哔哩 aria2 多线程加速**：B 站视频流/音频流下载接入 aria2c（`-x 16 -s 16` 16 线程分块），突破 CDN 单连接限速，下载速度提升数倍
- 🎯 **抖音自研解析器（1080p 原画）**：解析分享页 SSR 数据（`window._ROUTER_DATA`），提取 `video_id` 与原始分辨率，按官方 play API 的 `ratio` 画质档（540p/720p/1080p/2k/4k）构造直链，**实测可拿 1920x1080 原画**（yt-dlp 默认仅 720p）；解析失败自动回退 yt-dlp 兜底
- 🎯 **快手自研解析器（1080p 原画）**：通过桌面站 GraphQL `visionVideoDetail` 接口获取 `manifest.adaptationSet[].representation[]` 多档流，自动选择最高分辨率；支持 `fw/photo`、`short-video`、`f/w-`、`v.kuaishou.com` 短链等全部链接形态
- ⚡ **实时下载进度**：流式解析 yt-dlp / aria2c 输出，进度条、速度、剩余时间即时可见（视频流/音频流分段显示）
- 🆕 **"新建任务"按钮常驻**：下载过程中与下载完成后均可随时点击，一键取消/重置当前任务
- 📱 **移动端一屏适配**：页面高度恰好适配手机一屏（`100dvh` + Flex 布局），内容增多时自动延展，无多余空白
- 🧾 **日志自动换行**：超长错误日志/链接在卡片内自动换行，不再越过卡片边界
- 🔀 **音视频分离下载 + ffmpeg 自动合并**：mkv 容器兼容所有编码组合，避免合并失败
- 📥 **浏览器自动下载**：合并完成后自动触发浏览器下载，支持中文文件名（RFC 5987）
- 🛡️ **大文件拉取保护（参考 YT-Downloader）**：文件推送采用 `io.Copy` 流式传输 + 显式 `Content-Length` + `X-Accel-Buffering: no` 禁用代理缓冲；拉取期间任务受原子计数保护，任何清理路径都不会删除传输中的文件，4K 大文件慢速拉取不再中断（ERR_INVALID_RESPONSE）；最后一次拉取结束后 60 秒宽限（允许浏览器重试）再自动清理，定时清理仅移除 30 分钟以上的过期残留
- 🔐 **HTTPS 支持（参考 YT-Downloader）**：启动时自动生成自签名证书（RSA 2048，有效期 10 年，`certs` 目录持久化复用），默认端口 **443**，支持域名直接访问；`DOMAIN` 环境变量可将域名写入证书 SANs；**兼容 Cloudflare 代理**（SSL/TLS 模式设为 **Full** 即可）；`HTTP_ONLY=1` 可退回纯 HTTP 模式
- 🍪 **Cookie 管理（位于下载按钮下方）**：
  - **永久保存**：Cookie 注入后持久化存储（`cookies_store` 目录挂载到宿主机），重启容器不丢失
  - **自动识别平台**：粘贴 Cookie 自动识别所属平台（哔哩哔哩/抖音/快手/小红书/Likee/Instagram/YouTube），识别后自动保存并以「平台名」标签展示；识别失败时可手动选择平台保存
  - **标签化管理**：每个平台 Cookie 显示为标签，点击平台名即可查看/修改（修改后自动保存）
  - **灵活删除**：支持单个删除（标签 ✕）、勾选删除、全选删除所有 Cookie
  - 下载时自动携带对应平台已保存的 Cookie，无需重复输入
- 🪄 **无感链接提取**：粘贴分享文案自动提取链接（前后端双重提取）
- 🧹 **自动清理缓存**：
  - 下载失败时自动清理所有缓存
  - 浏览器拉取完成后 30 秒自动清理（全平台）
  - 后台每 10 分钟定时清理下载缓存（自动跳过进行中的任务）
  - 容器重启时自动清理所有下载文件
- ⚡ **Go 单二进制**：编译为单一可执行文件，启动快、占用低
- 🐳 **Docker 一键部署**：2 分钟内完成部署

## 🚀 部署方式

### 方式一：一键部署脚本（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/rsxbgdurxbjcx-arch/bilibili-downloader/main/bootstrap.sh | bash
```

脚本自动完成：安装 git → 克隆仓库 → 安装 Docker → 构建镜像 → 启动服务。**2 分钟内完成**。

### 方式二：手动 git clone + Docker

```bash
git clone https://github.com/rsxbgdurxbjcx-arch/bilibili-downloader.git
cd bilibili-downloader
docker compose up -d --build
```

访问：**https://服务器IP**（默认 443 端口 HTTPS，自签名证书，浏览器提示风险时选择"继续访问"）

### 域名访问 + Cloudflare 代理

1. 将域名 A 记录解析到服务器 IP，直接访问 `https://你的域名`
2. 若开启 Cloudflare 代理（橙云）：SSL/TLS 加密模式设为 **Full**（源站为自签名证书，CF 在 Full 模式下接受）
3. 可选环境变量：`DOMAIN=你的域名`（写入证书 SANs）、`HTTP_ONLY=1`（退回纯 HTTP）

### 自定义端口

```bash
PORT=9000 curl -fsSL https://raw.githubusercontent.com/rsxbgdurxbjcx-arch/bilibili-downloader/main/bootstrap.sh | bash
```

### 常用命令

```bash
cd ~/bilibili-downloader

docker compose logs -f      # 查看日志
docker compose restart      # 重启
docker compose down         # 停止
git pull && docker compose up -d --build  # 更新代码
```

## 🔄 一键更新容器内 yt-dlp 与 ffmpeg

容器运行后，如需更新 yt-dlp 和 ffmpeg 到最新版（无需重新构建镜像），执行以下命令：

```bash
# 进入项目目录
cd ~/bilibili-downloader

# 一键更新 yt-dlp + ffmpeg 并重启容器
docker compose exec bili-downloader sh -c '
  echo "=== 更新 yt-dlp ===" &&
  pip install --no-cache-dir -U yt-dlp &&
  echo "=== 更新 ffmpeg ===" &&
  ARCH=$(uname -m) &&
  case "$ARCH" in
    x86_64)  URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz" ;;
    aarch64) URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz" ;;
    *) echo "不支持的架构: $ARCH"; exit 1 ;;
  esac &&
  curl -fsSL -o /tmp/ffmpeg.tar.xz "$URL" &&
  tar -xf /tmp/ffmpeg.tar.xz -C /tmp &&
  cp -f /tmp/ffmpeg-*-static/ffmpeg /usr/local/bin/ &&
  cp -f /tmp/ffmpeg-*-static/ffprobe /usr/local/bin/ &&
  chmod +x /usr/local/bin/ffmpeg /usr/local/bin/ffprobe &&
  rm -rf /tmp/ffmpeg* &&
  echo "=== 更新完成 ===" &&
  yt-dlp --version &&
  ffmpeg -version | head -1
' && docker compose restart
```

或者保存为脚本一键执行：

```bash
# 保存更新脚本
cat > ~/bili-downloader/update-tools.sh << 'EOF'
#!/bin/bash
cd ~/bilibili-downloader
docker compose exec bili-downloader sh -c '
  pip install --no-cache-dir -U yt-dlp &&
  ARCH=$(uname -m) &&
  case "$ARCH" in
    x86_64)  URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz" ;;
    aarch64) URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz" ;;
  esac &&
  curl -fsSL -o /tmp/ffmpeg.tar.xz "$URL" &&
  tar -xf /tmp/ffmpeg.tar.xz -C /tmp &&
  cp -f /tmp/ffmpeg-*-static/ffmpeg /usr/local/bin/ &&
  cp -f /tmp/ffmpeg-*-static/ffprobe /usr/local/bin/ &&
  chmod +x /usr/local/bin/ffmpeg /usr/local/bin/ffprobe &&
  rm -rf /tmp/ffmpeg*
' && docker compose restart && echo "✅ yt-dlp + ffmpeg 已更新并重启"
EOF
chmod +x ~/bili-downloader/update-tools.sh
~/bili-downloader/update-tools.sh
```

## 📁 项目结构

```
bilibili-downloader/
├── main.go              # Go 后端（统一最高画质策略 + 抖音/快手自研解析器 + aria2 加速 + Cookie 管理 API）
├── go.mod               # Go 模块定义
├── static/              # 前端 Web UI（embed 编译进二进制）
│   ├── index.html       # 页面结构（Cookie 管理区位于下载按钮下方）
│   ├── style.css        # 样式（移动端一屏适配 + 日志自动换行 + Cookie 标签样式）
│   └── script.js        # 前端逻辑（Cookie 自动识别保存/标签编辑/单删/全选删除）
├── Dockerfile           # 多阶段构建（Go编译 + yt-dlp + ffmpeg + aria2）
├── docker-compose.yml   # Docker Compose 编排（端口 443 HTTPS，挂载 downloads、cookies_store 与 certs）
├── bootstrap.sh         # 一键部署脚本
└── README.md
```

## 🍪 Cookie 管理 API

| 接口 | 方法 | 说明 |
|------|------|------|
| `/api/cookie/detect` | POST | 自动识别 Cookie 所属平台（Body: `{"cookie":"..."}`） |
| `/api/cookies` | GET | 获取所有平台的 Cookie 保存状态 |
| `/api/cookie/{platform}` | GET/POST/DELETE | 查看 / 保存（修改）/ 删除单个平台 Cookie |
| `/api/cookies/delete` | POST | 批量删除（Body: `{"platforms":["bilibili",...]}`，传 `["*"]` 或空数组删除全部） |

## 🎯 统一最高画质策略（复制自 YT-Downloader，全平台适用）

**所有 7 个平台**（YouTube / 哔哩哔哩 / 抖音 / 快手 / 小红书 / Likee / Instagram）使用与 [YT-Downloader](https://github.com/rsxbgdurxbjcx-arch/YT-Downloader) 完全相同的解析逻辑与格式选择策略：

```
yt-dlp \
  -f bestvideo+bestaudio/best \
  --format-sort res,fps,tbr \
  --merge-output-format mkv \
  -o "%(title).80s [%(id)s].%(ext)s"
```

**策略说明**：
- `bestvideo+bestaudio/best`：选择最高画质视频流 + 最高音质音频流，回退到合一单流
- `--format-sort res,fps,tbr`：按 分辨率→帧率→总码率 排序，确保选到最高画质
  - 例如 YouTube 4K 有 H.264(~15Mbps) / VP9(~40Mbps) / AV1(~25Mbps) 多个流
  - 按 tbr 排序后，码率最高的流会被选中（即最高画质）
  - 不强制过滤编码（如 `[vcodec*=vp09]`）：某些视频/平台可能没有对应编码的流，强制过滤会导致匹配失败回退到低画质；按 tbr 排序则无论什么编码都选最高码率
- `--merge-output-format mkv`：mkv 支持所有编码组合（VP9/AV1 + Opus 等），避免 mp4 容器合并失败
- YouTube 使用默认 web client（不用 android client，后者只能拿到 720P，会限制最高画质）

## 🧹 自动清理缓存机制

| 触发条件 | 清理范围 |
|---------|---------|
| 下载失败 | 当前任务的所有缓存文件 |
| 浏览器拉取完成（全平台） | 延迟 30 秒后清理当前任务的所有文件 |
| 后台定时（每 10 分钟） | downloads 目录下所有文件（自动跳过进行中的任务） |
| 容器重启 | downloads 目录下所有文件 |

## 🛠️ 技术栈

- **后端**：Go 1.23（net/http 标准库，无第三方依赖）
- **下载核心**：yt-dlp（最新版，pip 安装）
- **音视频处理**：ffmpeg 7.0 静态构建
- **前端**：原生 HTML/CSS/JS（embed 进 Go 二进制）
- **部署**：Docker + Docker Compose

## ⚠️ 常见问题

### Q：下载高清视频失败？

A：多数平台高清画质需要登录态，请填写 Cookie。Cookie 会自动保存，下次无需重复输入。

### Q：快手提示"触发快手风控验证码（Need captcha）"？

A：快手对视频详情接口有严格的风控（按 IP + Cookie 综合判定）：
1. 稍后重试（风控通常临时性）
2. 更新快手 Cookie 后再试
3. 更换服务器网络/IP（家庭宽带 IP 通过率高于机房 IP）

### Q：抖音下载的不是 1080p？

A：本项目使用自研抖音解析器，按 `ratio` 画质档直连官方 play API，默认选取视频原始分辨率对应档位（最高 4K）。若个别视频解析失败会自动回退 yt-dlp（此时画质以 yt-dlp 结果为准）。

### Q：YouTube 下载失败？

A：YouTube 经常更新反爬机制，如失败：
1. 用上方「一键更新」命令更新 yt-dlp 到最新版
2. 填写 YouTube Cookie
3. 部分地区可能需要代理

### Q：部署超过 2 分钟？

A：首次构建 Docker 镜像需要下载 yt-dlp 和 ffmpeg（约 100MB），取决于网络速度。后续启动只需几秒。

## 📜 开源许可

- **yt-dlp**: Unlicense
- **ffmpeg**: GPLv2+
- **本项目代码**: MIT
