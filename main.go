// Package main 实现多平台视频下载器后端服务
//
// 架构：Go 原生 net/http + embed 静态资源 + yt-dlp + ffmpeg
//
// 核心能力：
//  1. 统一采用 YT-Downloader 的解析逻辑与格式选择策略（全部 7 个平台）：
//     -f bestvideo+bestaudio/best --format-sort res,fps,tbr --merge-output-format mkv
//     保证下载最高分辨率、最高帧率、最高码率的视频流 + 最高音质音频流
//  2. 任务式异步下载：提交任务 → 轮询进度 → 浏览器拉取文件
//  3. 下载进度实时推送（流式解析 yt-dlp 输出，而非进程结束后一次性解析）
//  4. Cookie 持久化：注入一次自动保存，下次自动填充
//  5. 自动清理：下载失败清理、推送完成清理、定时清理（跳过进行中任务）
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// ============ 配置 ============
var (
	BaseDir        = "."
	DownloadDir    = "downloads"
	CookieStoreDir = "cookies_store"
	YtdlpPath      = "yt-dlp"
	FfmpegPath     = "ffmpeg"
	FfprobePath    = "ffprobe"
	Aria2Path      = "" // aria2c 路径（检测不到则禁用多线程下载）
)

// 移动端 UA（抖音/快手解析及直连下载使用）
const mobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1"
const desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// aria2 多线程下载参数（16 连接突破 CDN 单连接限速）
const aria2DownloaderArgs = "aria2c:-x 16 -s 16 -k 1M --file-allocation=none --summary-interval=1"

// aria2c 进度行: [#009e1c 3.1MiB/5.1MiB(61%) CN:2 DL:3.0MiB ETA:1s]
var aria2ProgressRe = regexp.MustCompile(`^\[#\w+\s+\S+/\S+\((\d+(?:\.\d+)?)%\)(?:\s+CN:\d+)?\s+DL:([^\s]+)(?:\s+ETA:([^\s\]]+))?`)

// ============ 统一格式选择策略（复制自 YT-Downloader，应用于全部平台）============
// 格式选择器：bestvideo+bestaudio/best
//   - bestvideo：最高画质视频流（独立视频轨）
//   - bestaudio：最高音质音频流（独立音频轨）
//   - /best：回退方案（音视频合一的单流）
//
// --format-sort res,fps,tbr 强制 yt-dlp 按以下优先级排序选择流：
//   - res（分辨率）：优先最高分辨率，覆盖 8K/4K/1080P
//   - fps（帧率）：优先最高帧率，覆盖 120fps/60fps/30fps
//   - tbr（总码率）：同分辨率同帧率时，优先最高码率流
//     → YouTube 4K 视频通常有 H.264(~15Mbps) / VP9(~40Mbps) / AV1(~25Mbps) 多个流
//     → 按 tbr 排序后码率最高的流会被选中（即最高画质）
//
// 为什么不用 [vcodec*=vp09] 这类过滤？
//   - 某些视频/平台可能没有对应编码的流，强制过滤会导致匹配失败回退到低画质
//   - 用 --format-sort tbr 更可靠：无论什么编码都选最高码率（即最高画质）
//
// --merge-output-format mkv：mkv 支持所有编码组合，确保音视频合并成功
//   - VP9/AV1 视频 + Opus 音频的组合，mp4 容器可能合并失败
//   - mkv 容器兼容所有编码，避免合并错误
const (
	unifiedFormatSelector = "bestvideo+bestaudio/best"
	unifiedFormatSort     = "res,fps,tbr"
	unifiedMergeFormat    = "mkv"
	unifiedOutputTemplate = "%(title).80s [%(id)s].%(ext)s"
)

// ============ 平台配置 ============
type PlatformInfo struct {
	Name          string
	Domains       []string
	NeedsCookieHD bool
	UserAgent     string
	Referer       string
	ExtraArgs     []string
}

var SupportedPlatforms = map[string]PlatformInfo{
	"bilibili": {
		Name:          "哔哩哔哩",
		Domains:       []string{"bilibili.com", "b23.tv", "biligame.com", "bili2233.cn"},
		NeedsCookieHD: true,
	},
	"douyin": {
		Name:          "抖音",
		Domains:       []string{"douyin.com", "iesdouyin.com", "v.douyin.com", "dy.com"},
		NeedsCookieHD: true,
		UserAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		Referer:       "https://www.douyin.com/",
		ExtraArgs:     []string{"--no-check-formats"},
	},
	"kuaishou": {
		Name:          "快手",
		Domains:       []string{"kuaishou.com", "v.kuaishou.com", "chenzhongtech.com", "gifshow.com", "kwai.com"},
		NeedsCookieHD: true,
		UserAgent:     "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1",
		Referer:       "https://www.kuaishou.com/",
		ExtraArgs:     []string{"--force-generic-extractor", "--no-check-formats"},
	},
	"xiaohongshu": {
		Name:          "小红书",
		Domains:       []string{"xiaohongshu.com", "xhslink.com"},
		NeedsCookieHD: true,
		UserAgent:     "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1",
		Referer:       "https://www.xiaohongshu.com/",
		ExtraArgs:     []string{"--no-check-formats"},
	},
	"likee": {
		Name:          "Likee",
		Domains:       []string{"likee.video", "likee.com", "l.likee.video"},
		NeedsCookieHD: false,
		UserAgent:     "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1",
		Referer:       "https://likee.video/",
	},
	"instagram": {
		Name:          "Instagram",
		Domains:       []string{"instagram.com", "instagr.am"},
		NeedsCookieHD: true,
		UserAgent:     "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1",
		Referer:       "https://www.instagram.com/",
		ExtraArgs:     []string{"--no-check-formats"},
	},
	"youtube": {
		Name:          "YouTube",
		Domains:       []string{"youtube.com", "youtu.be", "m.youtube.com"},
		NeedsCookieHD: false, // YouTube 解析/下载无需 Cookie（与 YT-Downloader 一致）
		Referer:       "https://www.youtube.com/",
		// 保持 yt-dlp 默认客户端（与 YT-Downloader 完全一致）：
		// 默认客户端在干净状态下可拿 4K/8K 最高画质；
		// 切勿在此固定 player_client——tv/android 等客户端在无 PO token 时
		// 只返回 360p 合并流，会导致画质暴跌。反爬墙由运行时自动重试处理
		// （见 runTask 中 isBotWall 检测 + youtubeBotBypassArgs 回退）
		ExtraArgs: []string{},
	},
}

// ============ 任务状态 ============
type Task struct {
	TaskID      string                 `json:"task_id"`
	Status      string                 `json:"status"`
	Stage       string                 `json:"stage"`
	Progress    float64                `json:"progress"`
	Title       string                 `json:"title"`
	Platform    string                 `json:"platform"`
	Error       string                 `json:"error"`
	Filename    string                 `json:"filename"`
	Filepath    string                 `json:"filepath"`
	Filesize    int64                  `json:"filesize"`
	CreatedAt   int64                  `json:"created_at"`
	CompletedAt int64                  `json:"completed_at"`
	VideoFmt    map[string]interface{} `json:"video_format,omitempty"`
	AudioFmt    map[string]interface{} `json:"audio_format,omitempty"`
	AutoClean   bool                   `json:"auto_clean,omitempty"` // 推送完成后自动清理（全平台统一策略）
	// 内部状态（不序列化）：拉取保护，防止清理任务误删正在传输的文件
	PullActive   int32 `json:"-"` // 进行中的浏览器拉取数（原子操作）
	CleanupAfter int64 `json:"-"` // 最后一次拉取结束后的清理截止时间（原子操作）
}

var (
	tasks   = make(map[string]*Task)
	tasksMu sync.RWMutex
)

// ============ yt-dlp 格式 ============
type YtdlpFormat struct {
	FormatID   string  `json:"format_id"`
	Ext        string  `json:"ext"`
	Vcodec     string  `json:"vcodec"`
	Acodec     string  `json:"acodec"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Fps        float64 `json:"fps"`
	Vbr        float64 `json:"vbr"`
	Abr        float64 `json:"abr"`
	Tbr        float64 `json:"tbr"`
	Asr        int     `json:"asr"`
	FormatNote string  `json:"format_note"`
}

type YtdlpInfo struct {
	Title            string        `json:"title"`
	ID               string        `json:"id"`
	Ext              string        `json:"ext"`
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	Fps              float64       `json:"fps"`
	Tbr              float64       `json:"tbr"`
	Vcodec           string        `json:"vcodec"`
	Acodec           string        `json:"acodec"`
	Format           string        `json:"format"`
	Formats          []YtdlpFormat `json:"formats"`
	RequestedFormats []YtdlpFormat `json:"requested_formats"` // 应用 -f 选择器后的实际选中流
}

// ============ Cookie 平台自动识别 ============
// cookieSignature 单个特征：Name 为 cookie 名，Weight 为权重
// 高区分度的特征（如 SESSDATA 只属于 B 站）权重高，通用名（如 sessionid）权重低
type cookieSignature struct {
	Name   string
	Weight int
}

var platformCookieSignatures = map[string][]cookieSignature{
	"bilibili": {
		{"SESSDATA", 5}, {"bili_jct", 5}, {"DedeUserID", 5}, {"buvid3", 4},
		{"b_nut", 4}, {"buvid4", 4}, {"bili_ticket", 4}, {"b_lsid", 3},
	},
	"youtube": {
		{"__Secure-1PSID", 5}, {"__Secure-3PSID", 5}, {"__Secure-1PAPISID", 5},
		{"LOGIN_INFO", 5}, {"VISITOR_INFO1_LIVE", 4}, {"SAPISID", 3},
		{"APISID", 3}, {"HSID", 2}, {"SSID", 2}, {"SID", 1}, {"YSC", 2},
		{"PREF", 1}, {"GPS", 2},
	},
	"instagram": {
		{"ds_user_id", 5}, {"ig_did", 5}, {"ig_nrcb", 4}, {"rur", 4},
		{"mid", 3}, {"datr", 2}, {"dpr", 1}, {"wd", 1},
	},
	"xiaohongshu": {
		{"web_session", 5}, {"xsec_token", 5}, {"a1", 4}, {"webId", 4},
		{"webBuild", 4}, {"gid", 4}, {"xhsTrackerId", 4}, {"abRequestId", 3},
	},
	"douyin": {
		{"sessionid_ss", 5}, {"msToken", 5}, {"ttwid", 5}, {"odin_tt", 5},
		{"passport_csrf_token", 5}, {"sid_guard", 4}, {"uid_tt", 4},
		{"sid_tt", 4}, {"sessionid", 3}, {"bd_ticket_guard_client_data", 4},
		{"dy_swidth", 2}, {"s_v_web_id", 3},
	},
	"kuaishou": {
		{"kuaishou.server_st", 6}, {"kuaishou.sid", 5}, {"kuaishou.live_st", 5},
		{"kpf", 5}, {"kpn", 5}, {"kwai", 4}, {"kuaishou.live.bfb1s", 4},
		{"clientid", 2}, {"did", 1},
	},
	"likee": {
		{"likee_session", 6}, {"likee_uid", 6}, {"bigo_token", 5}, {"bigo_uid", 5},
		{"likee_country", 4}, {"likee", 3}, {"bigo", 3},
	},
}

// detectCookiePlatform 根据 cookie 内容自动识别所属平台
// 返回平台 key、平台名、命中的特征名列表
// 识别逻辑：按特征名精确匹配累计权重，取权重最高的平台；
// 另设兜底子串规则（cookie 名含平台标识子串时加分）
func detectCookiePlatform(cookieStr string) (string, string, []string) {
	// 解析所有 cookie 名
	names := make(map[string]bool)
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		name := strings.TrimSpace(part[:strings.Index(part, "=")])
		if name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return "", "", nil
	}

	scores := make(map[string]int)
	matched := make(map[string][]string)
	for platform, sigs := range platformCookieSignatures {
		for _, sig := range sigs {
			if names[sig.Name] {
				scores[platform] += sig.Weight
				matched[platform] = append(matched[platform], sig.Name)
			}
		}
	}

	// 兜底子串规则：cookie 名本身包含平台标识（处理特征表未覆盖的情况）
	substringRules := []struct {
		Sub      string
		Platform string
		Weight   int
	}{
		{"bili", "bilibili", 2},
		{"kuaishou", "kuaishou", 3},
		{"kwai", "kuaishou", 2},
		{"likee", "likee", 3},
		{"bigo", "likee", 2},
		{"douyin", "douyin", 3},
		{"xhs", "xiaohongshu", 3},
	}
	for name := range names {
		lower := strings.ToLower(name)
		for _, rule := range substringRules {
			if strings.Contains(lower, rule.Sub) {
				scores[rule.Platform] += rule.Weight
				// 去重：特征名可能已被精确匹配记录过
				exists := false
				for _, m := range matched[rule.Platform] {
					if m == name {
						exists = true
						break
					}
				}
				if !exists {
					matched[rule.Platform] = append(matched[rule.Platform], name)
				}
			}
		}
	}

	best := ""
	bestScore := 0
	for platform, score := range scores {
		if score > bestScore {
			bestScore = score
			best = platform
		}
	}
	// 权重太低视为不可靠，返回未识别
	if bestScore < 3 {
		return "", "", nil
	}
	return best, SupportedPlatforms[best].Name, matched[best]
}

// ============ 工具函数 ============
func sanitizeFilename(name string) string {
	reg := regexp.MustCompile(`[\\/:*?"<>|\r\n\t]`)
	name = reg.ReplaceAllString(name, "_")
	name = strings.Trim(name, " .")
	if name == "" {
		name = "video"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func detectPlatform(rawURL string) string {
	for key, info := range SupportedPlatforms {
		for _, domain := range info.Domains {
			matched, _ := regexp.MatchString("(?i)"+regexp.QuoteMeta(domain), rawURL)
			if matched {
				return key
			}
		}
	}
	return ""
}

// extractURL 从用户输入中提取真实 URL（复制自 YT-Downloader）
// 支持"【标题】 https://b23.tv/xxx"这类分享文本
func extractURL(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	idx := strings.Index(input, "http://")
	if idx < 0 {
		idx = strings.Index(input, "https://")
	}
	if idx < 0 {
		return ""
	}
	rest := input[idx:]
	for i, ch := range rest {
		// 空白字符（空格、制表符、换行、回车、中文空格）作为 URL 结束符
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '　' {
			return rest[:i]
		}
	}
	return rest
}

// resolveShortURL 预解析短链，避免 yt-dlp 回退到 generic 提取器
func resolveShortURL(rawURL string) string {
	shortDomains := []string{"b23.tv", "v.douyin.com", "v.kuaishou.com", "xhslink.com", "youtu.be", "instagr.am", "bili2233.cn"}
	needResolve := false
	for _, d := range shortDomains {
		if strings.Contains(rawURL, d) {
			needResolve = true
			break
		}
	}
	if !needResolve {
		return rawURL
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return rawURL
	}
	const desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	req.Header.Set("User-Agent", desktopUA)

	// 跟随最多 10 次重定向
	for i := 0; i < 10; i++ {
		resp, err := client.Do(req)
		if err != nil {
			return rawURL
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return rawURL
			}
			if strings.HasPrefix(loc, "http") {
				rawURL = loc
			} else {
				u, _ := req.URL.Parse(loc)
				rawURL = u.String()
			}
			req, err = http.NewRequest("GET", rawURL, nil)
			if err != nil {
				return rawURL
			}
			req.Header.Set("User-Agent", desktopUA)
			continue
		}
		resp.Body.Close()
		break
	}
	return rawURL
}

// ============ Cookie 持久化 ============
func getCookiePath(platform string) string {
	return filepath.Join(CookieStoreDir, platform+".txt")
}

func saveCookieRaw(platform, cookie string) error {
	return os.WriteFile(getCookiePath(platform), []byte(strings.TrimSpace(cookie)), 0600)
}

func loadCookieRaw(platform string) (string, bool) {
	data, err := os.ReadFile(getCookiePath(platform))
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	return s, s != ""
}

func deleteCookieRaw(platform string) bool {
	err := os.Remove(getCookiePath(platform))
	return err == nil
}

// writeCookieFile 将浏览器 cookie 字符串转为 Netscape 格式文件
// 关键规则：flag=TRUE 时 domain 必须以点开头；flag=FALSE 时不能以点开头
func writeCookieFile(cookieStr, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	var lines []string
	lines = append(lines, "# Netscape HTTP Cookie File")
	lines = append(lines, "# Generated by bili-go")

	// 解析 name=value 对
	var pairs [][2]string
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		idx := strings.Index(part, "=")
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name != "" {
			pairs = append(pairs, [2]string{name, value})
		}
	}

	baseDomains := []string{
		"bilibili.com", "b23.tv", "douyin.com", "iesdouyin.com",
		"kuaishou.com", "chenzhongtech.com", "xiaohongshu.com", "xhslink.com",
		"likee.video", "instagram.com", "youtube.com", "google.com",
	}

	for _, pair := range pairs {
		name, value := pair[0], pair[1]
		for _, domain := range baseDomains {
			// 带点域名：flag=TRUE（包含子域）
			lines = append(lines, fmt.Sprintf(".%s\tTRUE\t/\tTRUE\t9999999999\t%s\t%s", domain, name, value))
			// 不带点域名：flag=FALSE（仅当前域）
			lines = append(lines, fmt.Sprintf("%s\tFALSE\t/\tTRUE\t9999999999\t%s\t%s", domain, name, value))
		}
	}

	return os.WriteFile(destPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// ============ 格式选择器（用于解析结果展示的回退逻辑）============
func isStoryboard(f YtdlpFormat) bool {
	if strings.Contains(strings.ToLower(f.FormatNote), "storyboard") {
		return true
	}
	if strings.Contains(strings.ToLower(f.FormatID), "storyboard") {
		return true
	}
	if f.Vcodec != "none" && f.Vcodec != "" && f.Width <= 0 && f.Height <= 0 {
		return true
	}
	return false
}

// selectBestVideo 选择最高质量视频流：分辨率 → 帧率 → 码率（与 --format-sort res,fps,tbr 一致）
func selectBestVideo(formats []YtdlpFormat) *YtdlpFormat {
	var videoFmts []YtdlpFormat
	for _, f := range formats {
		if f.Vcodec == "none" || f.Vcodec == "" {
			continue
		}
		if isStoryboard(f) {
			continue
		}
		videoFmts = append(videoFmts, f)
	}
	if len(videoFmts) == 0 {
		return nil
	}
	sort.SliceStable(videoFmts, func(i, j int) bool {
		ri := videoFmts[i].Width * videoFmts[i].Height
		rj := videoFmts[j].Width * videoFmts[j].Height
		if ri != rj {
			return ri > rj
		}
		if videoFmts[i].Fps != videoFmts[j].Fps {
			return videoFmts[i].Fps > videoFmts[j].Fps
		}
		vi := videoFmts[i].Vbr
		if vi == 0 {
			vi = videoFmts[i].Tbr - videoFmts[i].Abr
		}
		vj := videoFmts[j].Vbr
		if vj == 0 {
			vj = videoFmts[j].Tbr - videoFmts[j].Abr
		}
		return vi > vj
	})
	return &videoFmts[0]
}

// selectBestAudio 选择最高音质音频流：码率 → 总码率 → 采样率
func selectBestAudio(formats []YtdlpFormat) *YtdlpFormat {
	var audioFmts []YtdlpFormat
	for _, f := range formats {
		if f.Vcodec == "none" && f.Acodec != "none" && f.Acodec != "" {
			audioFmts = append(audioFmts, f)
		}
	}
	if len(audioFmts) == 0 {
		return nil
	}
	sort.SliceStable(audioFmts, func(i, j int) bool {
		if audioFmts[i].Abr != audioFmts[j].Abr {
			return audioFmts[i].Abr > audioFmts[j].Abr
		}
		if audioFmts[i].Tbr != audioFmts[j].Tbr {
			return audioFmts[i].Tbr > audioFmts[j].Tbr
		}
		return audioFmts[i].Asr > audioFmts[j].Asr
	})
	return &audioFmts[0]
}

// ============ yt-dlp 命令构造 ============

// appendPlatformArgs 追加平台特定参数（UA / Referer / ExtraArgs / Cookie）
func appendPlatformArgs(args []string, cookieFile, platform string) []string {
	if info, ok := SupportedPlatforms[platform]; ok {
		if info.UserAgent != "" {
			args = append(args, "--user-agent", info.UserAgent)
		}
		if info.Referer != "" {
			args = append(args, "--add-header", "Referer: "+info.Referer)
		}
		args = append(args, info.ExtraArgs...)
	}
	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}
	return args
}

// youtubeBotBypassArgs YouTube 触发反爬墙（Sign in to confirm you're not a bot）时的
// 备用客户端参数：仅在该特定失败时自动启用，tv/web_embedded/android_vr/android
// 可绕过反爬墙且无需 Cookie；yt-dlp 会依次尝试并合并各客户端格式
var youtubeBotBypassArgs = []string{"--extractor-args", "youtube:player_client=tv,web_embedded,android_vr,android"}

// isBotWall 判断 yt-dlp 错误输出是否为 YouTube 反爬墙
func isBotWall(stderr string) bool {
	return strings.Contains(stderr, "Sign in to confirm") || strings.Contains(stderr, "not a bot")
}

// buildParseCmd 构造解析命令（复制自 YT-Downloader）
// -J 输出 JSON 元数据，同时应用统一格式选择器，
// 使返回的 requested_formats 字段包含实际选中的流（用于前端展示）
// extra 为运行时追加参数（如反爬墙备用客户端），位于平台参数之后
func buildParseCmd(rawURL, cookieFile, platform string, extra ...string) []string {
	args := []string{
		YtdlpPath,
		"-J",
		"-f", unifiedFormatSelector,
		"--format-sort", unifiedFormatSort, // 按分辨率→帧率→总码率排序，确保选最高码率流
		"--no-playlist",
		"--no-warnings",
		"--no-progress",
		"--no-check-certificate",
	}
	args = appendPlatformArgs(args, cookieFile, platform)
	args = append(args, extra...)
	args = append(args, rawURL)
	return args
}

// buildDownloadCmd 构造统一下载命令（复制自 YT-Downloader）
// 一次调用 yt-dlp 完成：选流 → 下载视频+音频 → ffmpeg 合并为 mkv
// 进度通过 --progress-template 实时输出，供 runWithProgress 流式解析
func buildDownloadCmd(rawURL, cookieFile, outputTemplate, platform string, extra ...string) []string {
	args := []string{
		YtdlpPath,
		"-f", unifiedFormatSelector,
		"--format-sort", unifiedFormatSort, // 按分辨率→帧率→总码率排序，选最高画质流
		"--merge-output-format", unifiedMergeFormat, // mkv 支持所有编码组合
		"--no-playlist",
		"--no-warnings",
		"--newline",
		"--no-mtime",
		"--no-part",
		"--no-check-certificate",
		"--ffmpeg-location", filepath.Dir(FfmpegPath),
		"--progress-template", "download:PROGRESS:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s",
		"--progress-template", "postprocess:POSTPROC:%(postprocessor)s",
		"-o", outputTemplate,
	}
	// 哔哩哔哩接入 aria2c 16 线程下载，突破 CDN 单连接限速（YouTube 保持原状，不使用 aria2c）
	if platform == "bilibili" && Aria2Path != "" {
		args = append(args, "--downloader", "aria2c", "--downloader-args", aria2DownloaderArgs)
	}
	args = appendPlatformArgs(args, cookieFile, platform)
	args = append(args, extra...)
	args = append(args, rawURL)
	return args
}

// buildDirectDownloadCmd 构造直连下载命令（抖音/快手自研解析出的直链）
// 单一媒体文件，无需选流与合并；优先 aria2c 多线程
func buildDirectDownloadCmd(directURL, outputTemplate, platform, ua string) []string {
	args := []string{
		YtdlpPath,
		"--no-playlist",
		"--no-warnings",
		"--newline",
		"--no-mtime",
		"--no-part",
		"--no-check-certificate",
		"--progress-template", "download:PROGRESS:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s",
		"-o", outputTemplate,
	}
	if Aria2Path != "" {
		args = append(args, "--downloader", "aria2c", "--downloader-args", aria2DownloaderArgs)
	}
	if ua != "" {
		args = append(args, "--user-agent", ua)
	}
	if info, ok := SupportedPlatforms[platform]; ok && info.Referer != "" {
		args = append(args, "--add-header", "Referer: "+info.Referer)
	}
	args = append(args, directURL)
	return args
}

// ============ 抖音/快手自研解析器（直连最高画质 1080p）============

type directVideo struct {
	URL     string // 视频直链
	Title   string
	Width   int
	Height  int
	Quality string // 画质说明（用于展示）
}

// resolveDirectVideo 按平台分发到自研解析器
func resolveDirectVideo(platform, rawURL, cookie string) (*directVideo, error) {
	switch platform {
	case "douyin":
		return resolveDouyin(rawURL, cookie)
	case "kuaishou":
		return resolveKuaishou(rawURL, cookie)
	}
	return nil, fmt.Errorf("平台 %s 无自研解析器", platform)
}

// ---------- 抖音解析器 ----------
// 原理：分享页 https://www.iesdouyin.com/share/video/{id}/ 的 HTML 内嵌
// window._ROUTER_DATA（SSR 数据，无需签名），其中 video.play_addr.url_list
// 含 video_id，video.width/height 为原始分辨率。
// 下载地址按官方 play API 构造：playwm 改 play（去水印），ratio 参数控制画质，
// 实测 ratio=1080p 可拿到 1920x1080 原画（yt-dlp 默认只拿 720p）。

var (
	douyinIDRe      = regexp.MustCompile(`/(?:video|note)/(\d{10,25})`)
	douyinModalRe   = regexp.MustCompile(`modal_id=(\d{10,25})`)
	douyinRouterRe  = regexp.MustCompile(`(?s)window\._ROUTER_DATA\s*=\s*(\{.*?\})\s*</script>`)
	douyinVideoIDRe = regexp.MustCompile(`video_id=([^&]+)`)
)

type douyinRouterData struct {
	LoaderData map[string]struct {
		VideoInfoRes struct {
			ItemList []struct {
				Desc  string `json:"desc"`
				Video struct {
					Width    int `json:"width"`
					Height   int `json:"height"`
					PlayAddr struct {
						URLList []string `json:"url_list"`
					} `json:"play_addr"`
				} `json:"video"`
			} `json:"item_list"`
		} `json:"videoInfoRes"`
	} `json:"loaderData"`
}

// douyinRatio 按原始短边分辨率选择 play API 的 ratio 画质档
func douyinRatio(w, h int) string {
	minDim := w
	if h > 0 && h < minDim {
		minDim = h
	}
	switch {
	case minDim >= 2160:
		return "4k"
	case minDim >= 1440:
		return "2k"
	case minDim >= 1080:
		return "1080p"
	case minDim >= 720:
		return "720p"
	default:
		return "540p"
	}
}

func resolveDouyin(rawURL, cookie string) (*directVideo, error) {
	awemeID := ""
	if m := douyinIDRe.FindStringSubmatch(rawURL); m != nil {
		awemeID = m[1]
	} else if m := douyinModalRe.FindStringSubmatch(rawURL); m != nil {
		awemeID = m[1]
	}
	if awemeID == "" {
		return nil, fmt.Errorf("无法从链接中提取抖音视频 ID")
	}

	pageURL := "https://www.iesdouyin.com/share/video/" + awemeID + "/"
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", mobileUA)
	req.Header.Set("Referer", "https://www.douyin.com/")
	if strings.TrimSpace(cookie) != "" {
		req.Header.Set("Cookie", strings.TrimSpace(cookie))
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求抖音分享页失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("抖音分享页返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 30<<20))
	if err != nil {
		return nil, fmt.Errorf("读取抖音分享页失败: %v", err)
	}

	m := douyinRouterRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("分享页未包含视频数据（_ROUTER_DATA 缺失）")
	}
	var rd douyinRouterData
	if err := json.Unmarshal(m[1], &rd); err != nil {
		return nil, fmt.Errorf("解析分享页数据失败: %v", err)
	}
	for _, page := range rd.LoaderData {
		if len(page.VideoInfoRes.ItemList) == 0 {
			continue
		}
		item := page.VideoInfoRes.ItemList[0]
		if len(item.Video.PlayAddr.URLList) == 0 {
			return nil, fmt.Errorf("分享页未返回播放地址（视频可能已删除或设为私密）")
		}
		vm := douyinVideoIDRe.FindStringSubmatch(item.Video.PlayAddr.URLList[0])
		if vm == nil {
			return nil, fmt.Errorf("未能提取 video_id")
		}
		ratio := douyinRatio(item.Video.Width, item.Video.Height)
		title := strings.TrimSpace(item.Desc)
		if title == "" {
			title = "douyin_" + awemeID
		}
		return &directVideo{
			URL:     fmt.Sprintf("https://aweme.snssdk.com/aweme/v1/play/?line=0&ratio=%s&video_id=%s", ratio, vm[1]),
			Title:   title,
			Width:   item.Video.Width,
			Height:  item.Video.Height,
			Quality: ratio,
		}, nil
	}
	return nil, fmt.Errorf("分享页未返回视频信息（视频可能已删除或设为私密）")
}

// ---------- 快手解析器 ----------
// 原理：桌面站 GraphQL 接口 visionVideoDetail 返回 manifest.adaptationSet[].representation[]
// 多档分辨率流（含 1080p）。从各形态链接提取 photoId 后查询即可。
// 注意：该接口受快手风控保护，触发验证码时会返回明确错误提示。

var (
	kuaishouPhotoIDRes = []*regexp.Regexp{
		regexp.MustCompile(`/fw/photo/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`/short-video/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`/f/w-([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`/photo/([A-Za-z0-9_-]+)`),
	}
)

const kuaishouDetailQuery = `query visionVideoDetail($photoId: String, $page: String) { visionVideoDetail(photoId: $photoId, page: $page) { status type photo { id duration caption photoUrl coverUrl likeCount timestamp viewCount manifest { adaptationSet { id duration representation { id url width height qualityType } } } } } }`

type kuaishouGQLResp struct {
	Data struct {
		VisionVideoDetail struct {
			Status int    `json:"status"`
			Type   string `json:"type"`
			Photo  struct {
				ID       string `json:"id"`
				Caption  string `json:"caption"`
				PhotoURL string `json:"photoUrl"`
				Manifest struct {
					AdaptationSet []struct {
						ID             int `json:"id"`
						Representation []struct {
							ID          string `json:"id"`
							URL         string `json:"url"`
							Width       int    `json:"width"`
							Height      int    `json:"height"`
							QualityType string `json:"qualityType"`
						} `json:"representation"`
					} `json:"adaptationSet"`
				} `json:"manifest"`
			} `json:"photo"`
		} `json:"visionVideoDetail"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func resolveKuaishou(rawURL, cookie string) (*directVideo, error) {
	photoID := ""
	for _, re := range kuaishouPhotoIDRes {
		if m := re.FindStringSubmatch(rawURL); m != nil {
			photoID = m[1]
			break
		}
	}
	if photoID == "" {
		return nil, fmt.Errorf("无法从链接中提取快手视频 ID（支持 fw/photo、short-video、f/w- 等链接形态）")
	}
	if strings.TrimSpace(cookie) == "" {
		return nil, fmt.Errorf("快手解析需要 Cookie，请在页面下方注入快手 Cookie")
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"operationName": "visionVideoDetail",
		"variables":     map[string]string{"photoId": photoID, "page": "detail"},
		"query":         kuaishouDetailQuery,
	})
	req, err := http.NewRequest("POST", "https://www.kuaishou.com/graphql", strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://www.kuaishou.com/short-video/"+photoID)
	req.Header.Set("Origin", "https://www.kuaishou.com")
	req.Header.Set("Cookie", strings.TrimSpace(cookie))
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求快手接口失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, fmt.Errorf("读取快手接口响应失败: %v", err)
	}

	var gql kuaishouGQLResp
	if err := json.Unmarshal(body, &gql); err != nil {
		return nil, fmt.Errorf("解析快手接口响应失败: %v", err)
	}
	for _, e := range gql.Errors {
		if strings.Contains(e.Message, "captcha") || strings.Contains(e.Message, "Captcha") {
			return nil, fmt.Errorf("触发快手风控验证码（Need captcha）：请稍后重试，或更换服务器网络/IP，或更新快手 Cookie 后再试")
		}
	}
	if len(gql.Errors) > 0 {
		return nil, fmt.Errorf("快手接口报错: %s", gql.Errors[0].Message)
	}
	photo := gql.Data.VisionVideoDetail.Photo
	if photo.ID == "" && photo.PhotoURL == "" && len(photo.Manifest.AdaptationSet) == 0 {
		return nil, fmt.Errorf("快手未返回视频数据（视频可能已删除/设为私密，或 Cookie 已失效）")
	}

	// 从 manifest 各档流中选最高分辨率（1080p 优先）
	type cand struct {
		url          string
		width, height int
		quality      string
	}
	var best *cand
	for _, as := range photo.Manifest.AdaptationSet {
		for _, r := range as.Representation {
			if r.URL == "" {
				continue
			}
			c := &cand{url: r.URL, width: r.Width, height: r.Height, quality: r.QualityType}
			if best == nil || c.width*c.height > best.width*best.height {
				best = c
			}
		}
	}
	if best != nil {
		title := strings.TrimSpace(photo.Caption)
		if title == "" {
			title = "kuaishou_" + photoID
		}
		q := best.quality
		if q == "" {
			q = fmt.Sprintf("%dx%d", best.width, best.height)
		}
		return &directVideo{URL: best.url, Title: title, Width: best.width, Height: best.height, Quality: q}, nil
	}
	// 回退：photoUrl（通常为默认清晰度）
	if photo.PhotoURL != "" {
		title := strings.TrimSpace(photo.Caption)
		if title == "" {
			title = "kuaishou_" + photoID
		}
		return &directVideo{URL: photo.PhotoURL, Title: title, Quality: "默认"}, nil
	}
	return nil, fmt.Errorf("快手接口未返回可用的视频流")
}

// runDirectDownload 直连下载（抖音/快手自研解析成功后调用）
func runDirectDownload(task *Task, workDir, platform string, dv *directVideo) {
	platformName := SupportedPlatforms[platform].Name
	task.Platform = platformName
	if strings.TrimSpace(dv.Title) != "" {
		task.Title = dv.Title
	} else {
		task.Title = platformName + "_video"
	}
	if dv.Width > 0 && dv.Height > 0 {
		task.VideoFmt = map[string]interface{}{
			"resolution": fmt.Sprintf("%dx%d", dv.Width, dv.Height),
			"quality":    dv.Quality,
		}
	}
	task.Stage = fmt.Sprintf("已选择: %s（%s），开始多线程下载...", task.Title, dv.Quality)
	task.Progress = 20

	// 直连文件名必须简短固定：直链 URL 极长，若按 URL 推导文件名会超过
	// 文件系统 255 字节限制导致 aria2c 建文件失败（code 16）。
	// 下载完成后 completeTask 会重命名为视频标题。
	outputTemplate := filepath.Join(workDir, "video.%(ext)s")
	cmd := buildDirectDownloadCmd(dv.URL, outputTemplate, platform, mobileUA)
	log.Printf("[%s] 直连下载: %s", platformName, dv.URL)
	log.Printf("[%s] 执行命令: %s", platformName, strings.Join(cmd, " "))

	if err := runWithProgress(cmd, task, 25, 95, 1, platformName); err != nil {
		task.Status = "failed"
		task.Stage = "失败"
		task.Error = err.Error()
		cleanupFiles(workDir)
		return
	}
	completeTask(task, workDir)
}

// completeTask 下载完成后处理：查找文件 → 重命名为标题 → 标记完成
func completeTask(task *Task, workDir string) {
	finalPath, finalName, err := findOutputFile(workDir)
	if err != nil {
		task.Status = "failed"
		task.Stage = "失败"
		task.Error = err.Error()
		cleanupFiles(workDir)
		return
	}
	// 直连下载的文件名来自 URL（无意义），重命名为视频标题
	if task.Title != "" && !strings.Contains(finalName, task.Title) {
		ext := filepath.Ext(finalName)
		newName := sanitizeFilename(task.Title) + ext
		newPath := filepath.Join(workDir, newName)
		if err := os.Rename(finalPath, newPath); err == nil {
			finalPath, finalName = newPath, newName
		}
	}

	fi, _ := os.Stat(finalPath)
	task.Status = "completed"
	task.Stage = "下载完成，等待浏览器拉取"
	task.Progress = 100
	task.Filename = finalName
	task.Filepath = finalPath
	task.AutoClean = true
	if fi != nil {
		task.Filesize = fi.Size()
	}
	task.CompletedAt = time.Now().Unix()
	log.Printf("[%s] 下载完成: %s (%d bytes)", task.Platform, finalName, task.Filesize)
}

// ============ 统一下载流程（全平台使用 YT-Downloader 策略）============
func runDownloadTask(taskID, rawURL, cookie, platform string) {
	task := getTask(taskID)
	if task == nil {
		return
	}
	workDir := filepath.Join(DownloadDir, taskID)
	os.MkdirAll(workDir, 0755)

	// 阶段1: 解析
	platformName := "未知平台"
	if info, ok := SupportedPlatforms[platform]; ok {
		platformName = info.Name
	}
	task.Status = "processing"
	task.Stage = "解析" + platformName + "视频信息中..."
	task.Progress = 5

	resolvedURL := resolveShortURL(rawURL)
	if resolvedURL != rawURL {
		task.Stage = "短链已解析，获取视频信息中..."
		task.Progress = 8
	}

	// 抖音/快手：自研解析器直连最高画质（1080p，无需 cookie 文件）
	// 抖音：分享页 SSR 数据 + play API ratio 画质档（实测可拿 1920x1080）
	// 快手：桌面站 GraphQL visionVideoDetail 返回 manifest 多档流
	if platform == "douyin" || platform == "kuaishou" {
		dv, derr := resolveDirectVideo(platform, resolvedURL, cookie)
		if derr == nil {
			runDirectDownload(task, workDir, platform, dv)
			return
		}
		if platform == "kuaishou" {
			// yt-dlp 对快手必然报 Unsupported URL，直接展示自研解析器的明确错误
			task.Status = "failed"
			task.Stage = "失败"
			task.Error = derr.Error()
			cleanupFiles(workDir)
			return
		}
		// 抖音解析失败时回退 yt-dlp 流程（可保低画质兜底）
		log.Printf("[抖音] 自研解析失败，回退 yt-dlp: %v", derr)
	}

	// Cookie 文件处理（仅 yt-dlp 流程需要）
	var cookieFile string
	if strings.TrimSpace(cookie) != "" {
		cookieFile = filepath.Join(workDir, "cookies.txt")
		if err := writeCookieFile(strings.TrimSpace(cookie), cookieFile); err != nil {
			task.Status = "failed"
			task.Stage = "失败"
			task.Error = "生成 cookie 文件失败: " + err.Error()
			return
		}
	}

	infoOut, infoErr, rc := runCmd(buildParseCmd(resolvedURL, cookieFile, platform))

	// YouTube 反爬墙自动回退：默认客户端被墙时，切换备用客户端重试（无需 Cookie）
	// 注意：只有触发反爬墙才启用——这些备用客户端在无 PO token 时画质档位有限，
	// 默认客户端仍是最高画质的首选路径
	var bypassArgs []string
	if rc != 0 && platform == "youtube" && isBotWall(infoErr) {
		log.Printf("[YouTube] 检测到反爬墙，自动切换备用客户端重试: %s", task.TaskID)
		task.Stage = "检测到反爬验证，自动切换客户端重试..."
		bypassArgs = youtubeBotBypassArgs
		infoOut, infoErr, rc = runCmd(buildParseCmd(resolvedURL, cookieFile, platform, bypassArgs...))
	}

	if rc != 0 {
		task.Status = "failed"
		task.Stage = "失败"
		task.Error = "解析失败: " + truncate(infoErr, 500)
		cleanupFiles(workDir)
		return
	}

	var info YtdlpInfo
	if err := json.Unmarshal([]byte(infoOut), &info); err != nil {
		task.Status = "failed"
		task.Stage = "失败"
		task.Error = "解析 JSON 失败: " + err.Error()
		cleanupFiles(workDir)
		return
	}

	title := info.Title
	if title == "" {
		title = info.ID
	}
	if title == "" {
		title = "video"
	}
	task.Title = title
	task.Platform = platformName
	task.Stage = "已解析: " + title
	task.Progress = 15

	// 阶段2: 提取实际选中的流信息（用于前端展示）
	// 优先使用 requested_formats（yt-dlp 应用统一选择器后的真实结果），
	// 回退到自定义选择器（与 --format-sort res,fps,tbr 排序规则一致）
	var bestVideo, bestAudio *YtdlpFormat
	if len(info.RequestedFormats) > 0 {
		bestVideo = &info.RequestedFormats[0]
		if len(info.RequestedFormats) > 1 {
			bestAudio = &info.RequestedFormats[1]
		}
	} else {
		bestVideo = selectBestVideo(info.Formats)
		bestAudio = selectBestAudio(info.Formats)
	}
	if bestVideo == nil && info.Vcodec != "" && info.Vcodec != "none" {
		// 单一直流（generic 提取器无 formats 列表），用顶层信息
		bestVideo = &YtdlpFormat{
			Width: info.Width, Height: info.Height, Fps: info.Fps,
			Tbr: info.Tbr, Vcodec: info.Vcodec, FormatID: info.Format,
		}
	}

	if bestVideo != nil && bestVideo.Vcodec != "" {
		task.VideoFmt = map[string]interface{}{
			"format_id":  bestVideo.FormatID,
			"resolution": fmt.Sprintf("%dx%d", bestVideo.Width, bestVideo.Height),
			"fps":        bestVideo.Fps,
			"vcodec":     bestVideo.Vcodec,
			"vbr":        bestVideo.Vbr,
			"tbr":        bestVideo.Tbr,
		}
	}
	if bestAudio != nil {
		task.AudioFmt = map[string]interface{}{
			"format_id": bestAudio.FormatID,
			"acodec":    bestAudio.Acodec,
			"abr":       bestAudio.Abr,
			"asr":       bestAudio.Asr,
		}
	}
	if bestVideo != nil {
		task.Stage = fmt.Sprintf("已选择: %dx%d @ %.0ffps，开始下载...", bestVideo.Width, bestVideo.Height, bestVideo.Fps)
	}
	task.Progress = 20

	// 阶段3: 统一下载（复制自 YT-Downloader）
	// 一次调用完成选流+下载+ffmpeg合并，支持最高分辨率/帧率/码率
	outputTemplate := filepath.Join(workDir, unifiedOutputTemplate)
	downloadCmd := buildDownloadCmd(resolvedURL, cookieFile, outputTemplate, platform, bypassArgs...)
	log.Printf("[%s] 执行命令: %s", platformName, strings.Join(downloadCmd, " "))

	// 分段数 = 实际选中的流数量（1=合一单流，2=视频+音频分离流）
	// 用于将进度条平滑映射到两段下载过程
	segments := len(info.RequestedFormats)
	if segments < 1 {
		segments = 1
	}
	if segments > 2 {
		segments = 2
	}

	dlErr := runWithProgress(downloadCmd, task, 25, 95, segments, platformName)
	// YouTube 下载阶段单独触发反爬墙（解析通过但下载被墙）：自动切换备用客户端重试
	if dlErr != nil && platform == "youtube" && len(bypassArgs) == 0 && isBotWall(dlErr.Error()) {
		log.Printf("[YouTube] 下载阶段检测到反爬墙，自动切换备用客户端重试: %s", task.TaskID)
		task.Stage = "检测到反爬验证，自动切换客户端重试..."
		task.Progress = 20
		retryCmd := buildDownloadCmd(resolvedURL, cookieFile, outputTemplate, platform, youtubeBotBypassArgs...)
		log.Printf("[%s] 执行命令: %s", platformName, strings.Join(retryCmd, " "))
		dlErr = runWithProgress(retryCmd, task, 25, 95, segments, platformName)
	}
	if dlErr != nil {
		task.Status = "failed"
		task.Stage = "失败"
		task.Error = dlErr.Error()
		cleanupFiles(workDir) // 下载失败时清理所有缓存
		return
	}

	// 清理 cookie 文件（最终文件已生成，无需保留）
	os.Remove(cookieFile)

	// 阶段4/5: 查找文件并完成（全平台统一标记，推送后自动清理）
	completeTask(task, workDir)
}

// findOutputFile 在任务目录中查找下载完成的文件（取最大的有效媒体文件）
func findOutputFile(workDir string) (string, string, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return "", "", fmt.Errorf("读取下载目录失败: %w", err)
	}
	var bestPath, bestName string
	var maxSize int64 = -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		// 跳过临时/中间文件
		if strings.HasSuffix(lower, ".part") ||
			strings.HasSuffix(lower, ".ytdl") ||
			strings.HasSuffix(lower, ".temp") ||
			strings.HasSuffix(lower, ".frag") ||
			strings.HasSuffix(lower, ".tmp") ||
			strings.HasSuffix(lower, ".json") ||
			name == "cookies.txt" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > maxSize {
			maxSize = info.Size()
			bestPath = filepath.Join(workDir, name)
			bestName = name
		}
	}
	if bestPath == "" || maxSize <= 0 {
		return "", "", fmt.Errorf("yt-dlp 未生成有效的输出文件")
	}
	return bestPath, bestName, nil
}

func cleanupFiles(workDir string) {
	os.RemoveAll(workDir)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ============ 命令执行 ============
func runCmd(cmd []string) (string, string, int) {
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Env = os.Environ()
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	rc := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			rc = exitErr.ExitCode()
		} else {
			rc = -1
		}
	}
	return stdout.String(), stderr.String(), rc
}

// runWithProgress 运行 yt-dlp 并【流式】解析进度
//
// 关键修复（原实现缺陷）：
//   原实现用 CombinedOutput 等待进程结束后才一次性解析输出，
//   导致下载过程中前端进度完全静止，结束后才跳变。
//   现改为 StdoutPipe + bufio.Scanner 逐行实时解析，进度即时可见。
//
// 进度映射：
//   - 分离流（segments=2）：视频流映射 startPct→中点，音频流映射中点→endPct
//   - 合一单流（segments=1）：全程映射 startPct→endPct
//   - 通过 percent 回落检测自动切换分段
func runWithProgress(cmd []string, task *Task, startPct, endPct float64, segments int, label string) error {
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Env = os.Environ()

	stdout, err := c.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建 stderr 管道失败: %w", err)
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("启动 yt-dlp 失败: %w", err)
	}

	// stderr 收集（保留最后 30 行用于错误报告）
	var errLines []string
	var errMu sync.Mutex
	var stderrWg sync.WaitGroup
	stderrWg.Add(1)
	go func() {
		defer stderrWg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			errMu.Lock()
			errLines = append(errLines, sc.Text())
			if len(errLines) > 30 {
				errLines = errLines[len(errLines)-30:]
			}
			errMu.Unlock()
		}
	}()

	// stdout 流式解析进度
	segSpan := (endPct - startPct) / float64(segments)
	curSeg := 0
	lastPct := -1.0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// aria2c 多线程下载进度行: [#gid 3.1MiB/5.1MiB(61%) CN:2 DL:3.0MiB ETA:1s]
		if strings.HasPrefix(line, "[#") {
			if m := aria2ProgressRe.FindStringSubmatch(line); m != nil {
				pct, perr := strconv.ParseFloat(m[1], 64)
				if perr != nil {
					continue
				}
				if lastPct >= 0 && pct < lastPct-5 && curSeg < segments-1 {
					curSeg++
				}
				lastPct = pct
				speed, eta := m[2], ""
				if len(m) > 3 {
					eta = m[3]
				}
				segName := "视频"
				if segments > 1 && curSeg == 1 {
					segName = "音频"
				}
				task.Stage = fmt.Sprintf("[%s] %s流多线程下载中 %.1f%% 速度:%s 剩余:%s", label, segName, pct, speed, eta)
				cur := startPct + float64(curSeg)*segSpan + segSpan*(pct/100.0)
				if cur < endPct {
					task.Progress = cur
				}
			}
			continue
		}
		if strings.HasPrefix(line, "PROGRESS:") {
			payload := strings.TrimPrefix(line, "PROGRESS:")
			parts := strings.Split(payload, "|")
			if len(parts) < 1 {
				continue
			}
			pctStr := strings.TrimSpace(parts[0])
			pctStr = strings.TrimSuffix(pctStr, "%")
			pct, perr := strconv.ParseFloat(pctStr, 64)
			if perr != nil {
				continue
			}
			// percent 明显回落 → 进入下一段（视频流 → 音频流）
			if lastPct >= 0 && pct < lastPct-5 && curSeg < segments-1 {
				curSeg++
			}
			lastPct = pct

			speed, eta := "", ""
			if len(parts) > 1 {
				speed = strings.TrimSpace(parts[1])
			}
			if len(parts) > 2 {
				eta = strings.TrimSpace(parts[2])
			}
			segName := "视频"
			if segments > 1 && curSeg == 1 {
				segName = "音频"
			}
			task.Stage = fmt.Sprintf("[%s] %s流下载中 %.1f%% 速度:%s 剩余:%s", label, segName, pct, speed, eta)
			cur := startPct + float64(curSeg)*segSpan + segSpan*(pct/100.0)
			if cur < endPct {
				task.Progress = cur
			}
		} else if strings.HasPrefix(line, "POSTPROC:") {
			task.Stage = "ffmpeg 合并音视频中..."
			task.Progress = endPct + 1
		}
	}

	waitErr := c.Wait()
	stderrWg.Wait()

	if waitErr != nil {
		errMu.Lock()
		tail := strings.Join(errLines, "\n")
		errMu.Unlock()
		return fmt.Errorf("%s下载失败: %v\n输出:\n%s", label, waitErr, tail)
	}
	return nil
}

// ============ 任务管理 ============
func getTask(taskID string) *Task {
	tasksMu.RLock()
	defer tasksMu.RUnlock()
	return tasks[taskID]
}

// ============ API 处理 ============
func main() {
	// 初始化目录
	os.MkdirAll(DownloadDir, 0755)
	os.MkdirAll(CookieStoreDir, 0755)

	// 容器重启时自动清理所有下载缓存
	cleanupAllDownloads()
	log.Printf("🧹 启动清理完成")

	// 后台定时清理：每 10 分钟自动清理下载缓存（跳过进行中的任务）
	go startCleanupTimer()

	// 检测二进制
	checkBinaries()

	// 路由
	http.HandleFunc("/api/health", handleHealth)
	http.HandleFunc("/api/platforms", handlePlatforms)
	http.HandleFunc("/api/download", handleDownload)
	http.HandleFunc("/api/status/", handleStatus)
	http.HandleFunc("/api/file/", handleFile)
	http.HandleFunc("/api/task/", handleDeleteTask)
	http.HandleFunc("/api/cookies", handleGetAllCookies)
	http.HandleFunc("/api/cookies/delete", handleBatchDeleteCookies)
	http.HandleFunc("/api/cookie/detect", handleDetectCookie)
	http.HandleFunc("/api/cookie/", handleCookieCRUD)

	// 静态文件
	staticContent, _ := fs.Sub(staticFS, "static")
	http.Handle("/", http.FileServer(http.FS(staticContent)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "443"
	}

	// HTTP_ONLY=1 时退回纯 HTTP 模式（适用于 Cloudflare Flexible SSL 或仅内网 IP 访问）
	if os.Getenv("HTTP_ONLY") == "1" || strings.ToLower(os.Getenv("HTTP_ONLY")) == "true" {
		log.Printf("🚀 服务启动（HTTP 模式），监听 :%s", port)
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}

	// 默认 HTTPS 模式：启动时自动生成自签名证书（参考 YT-Downloader）
	// 兼容 Cloudflare 代理：将 CF  SSL/TLS 模式设为 "Full" 即可（CF 接受源站自签名证书）
	certDir := os.Getenv("TLS_DIR")
	if certDir == "" {
		certDir = "certs"
	}
	certFile := filepath.Join(certDir, "cert.pem")
	keyFile := filepath.Join(certDir, "key.pem")
	if err := ensureTLSCert(certFile, keyFile); err != nil {
		log.Fatalf("❌ TLS 证书初始化失败: %v", err)
	}
	log.Printf("🔐 HTTPS 已启用（自签名证书，浏览器首次访问需确认安全提示）")
	log.Printf("🚀 服务启动，监听 :%s（域名访问：https://你的域名；Cloudflare 请将 SSL/TLS 设为 Full）", port)
	log.Fatal(http.ListenAndServeTLS(":"+port, certFile, keyFile, nil))
}

// ensureTLSCert 启动时自动生成自签名 TLS 证书（参考 YT-Downloader 的实现）
// 若 cert.pem / key.pem 已存在则直接复用（挂载 certs 目录可持久化证书）
// SANs 覆盖：localhost、127.0.0.1、::1、主机名，以及 DOMAIN 环境变量指定的域名
func ensureTLSCert(certFile, keyFile string) error {
	if _, err := os.Stat(certFile); err == nil {
		if _, err := os.Stat(keyFile); err == nil {
			log.Printf("🔐 复用已有 TLS 证书: %s", certFile)
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0o755); err != nil {
		return err
	}

	log.Printf("🔐 首次启动，正在生成自签名 TLS 证书...")
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("生成私钥失败: %v", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("生成序列号失败: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"bilibili-downloader"},
			CommonName:   "bilibili-downloader",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 年有效期
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, hostname)
	}
	// DOMAIN 环境变量：把访问域名写入证书 SANs（直连域名访问时浏览器提示更友好）
	if domain := strings.TrimSpace(os.Getenv("DOMAIN")); domain != "" {
		tmpl.Subject.CommonName = domain
		tmpl.DNSNames = append(tmpl.DNSNames, domain)
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("签发证书失败: %v", err)
	}

	certOut, err := os.OpenFile(certFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		certOut.Close()
		return err
	}
	certOut.Close()

	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {
		keyOut.Close()
		return err
	}
	keyOut.Close()

	log.Printf("🔐 自签名证书已生成: %s（有效期 10 年）", certFile)
	return nil
}

// cleanupAllDownloads 清理 downloads 目录下的过期残留（参考 YT-Downloader 的 maxAge 策略）
// 只删除修改时间超过 30 分钟的任务目录，且跳过：
//   - 状态为 queued/processing 的进行中任务
//   - 正在被浏览器拉取的任务（PullActive > 0）
//   - 拉取结束后仍在宽限期内的任务（CleanupAfter 未到）
// 大文件慢速拉取可能持续很久，绝不能在传输中途删除文件
func cleanupAllDownloads() {
	const maxAge = 30 * time.Minute
	now := time.Now()

	protected := make(map[string]bool)
	tasksMu.RLock()
	for id, t := range tasks {
		if t.Status == "queued" || t.Status == "processing" {
			protected[id] = true
			continue
		}
		if atomic.LoadInt32(&t.PullActive) > 0 {
			protected[id] = true
			continue
		}
		if ca := atomic.LoadInt64(&t.CleanupAfter); ca > 0 && now.Unix() < ca {
			protected[id] = true
		}
	}
	tasksMu.RUnlock()

	entries, err := os.ReadDir(DownloadDir)
	if err == nil {
		for _, e := range entries {
			if e.Name() == ".gitkeep" || protected[e.Name()] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			// 只清理超过 maxAge 的残留目录，新近完成/正在拉取的一律保留
			if now.Sub(info.ModTime()) < maxAge {
				continue
			}
			os.RemoveAll(filepath.Join(DownloadDir, e.Name()))
			log.Printf("🧹 清理过期残留: %s", e.Name())
		}
	}

	// 清理任务 map 中的失效条目：非进行中、不在拉取、且目录已不存在
	tasksMu.Lock()
	for id, t := range tasks {
		if t.Status == "queued" || t.Status == "processing" {
			continue
		}
		if atomic.LoadInt32(&t.PullActive) > 0 {
			continue
		}
		if ca := atomic.LoadInt64(&t.CleanupAfter); ca > 0 && now.Unix() < ca {
			continue
		}
		if _, err := os.Stat(filepath.Join(DownloadDir, id)); os.IsNotExist(err) {
			delete(tasks, id)
		}
	}
	tasksMu.Unlock()
}

// scheduleTaskCleanup 在拉取结束后调度任务清理
// 等到 CleanupAfter 截止（默认拉取结束后 60 秒宽限，允许浏览器重试/续传），
// 期间若有新的拉取开始（PullActive > 0）则继续等待，直到无拉取且宽限期过
func scheduleTaskCleanup(taskID string) {
	go func() {
		for {
			task := getTask(taskID)
			if task == nil {
				return
			}
			if atomic.LoadInt32(&task.PullActive) > 0 {
				time.Sleep(30 * time.Second)
				continue
			}
			ca := atomic.LoadInt64(&task.CleanupAfter)
			if ca <= 0 {
				return
			}
			if wait := time.Until(time.Unix(ca, 0)); wait > 0 {
				time.Sleep(wait)
				continue
			}
			// 宽限期已过且无拉取：执行清理
			cleanupFiles(filepath.Join(DownloadDir, taskID))
			tasksMu.Lock()
			delete(tasks, taskID)
			tasksMu.Unlock()
			log.Printf("🧹 已自动清理任务: %s", taskID)
			return
		}
	}()
}

// startCleanupTimer 每 10 分钟清理过期下载缓存（跳过进行中与拉取中任务）
func startCleanupTimer() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		log.Printf("🧹 定时清理：清理过期下载缓存（跳过进行中与拉取中任务）")
		cleanupAllDownloads()
	}
}

func checkBinaries() {
	for _, name := range []string{"yt-dlp", "ffmpeg", "ffprobe", "aria2c"} {
		path, err := exec.LookPath(name)
		if err != nil {
			log.Printf("⚠️  %s 未找到，请确保已安装", name)
		} else {
			log.Printf("✅ %s: %s", name, path)
			// 更新全局路径变量，确保使用绝对路径
			switch name {
			case "yt-dlp":
				YtdlpPath = path
			case "ffmpeg":
				FfmpegPath = path
			case "ffprobe":
				FfprobePath = path
			case "aria2c":
				Aria2Path = path
			}
		}
	}
	if Aria2Path == "" {
		log.Printf("⚠️  aria2c 未安装，哔哩哔哩等平台将使用单线程下载")
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	result := map[string]string{"status": "ok"}
	if out, _, rc := runCmd([]string{YtdlpPath, "--version"}); rc == 0 {
		result["yt_dlp"] = strings.TrimSpace(out)
	}
	if out, _, rc := runCmd([]string{FfmpegPath, "-version"}); rc == 0 {
		result["ffmpeg"] = strings.Split(out, "\n")[0]
	}
	json.NewEncoder(w).Encode(result)
}

func handlePlatforms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type plat struct {
		Key           string `json:"key"`
		Name          string `json:"name"`
		NeedsCookieHD bool   `json:"needs_cookie_for_hd"`
	}
	var plts []plat
	for k, v := range SupportedPlatforms {
		plts = append(plts, plat{k, v.Name, v.NeedsCookieHD})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"platforms": plts})
}

type DownloadReq struct {
	URL    string `json:"url"`
	Cookie string `json:"cookie"`
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req DownloadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}
	rawInput := strings.TrimSpace(req.URL)
	if rawInput == "" {
		http.Error(w, `{"detail":"URL 不能为空"}`, 400)
		return
	}

	// 从输入中提取真实 URL（支持"【标题】 https://b23.tv/xxx"这类分享文本，复制自 YT-Downloader）
	extracted := extractURL(rawInput)
	if extracted == "" {
		http.Error(w, `{"detail":"未找到有效的 http:// 或 https:// 链接"}`, 400)
		return
	}

	platform := detectPlatform(extracted)
	if platform == "" {
		var names []string
		for _, v := range SupportedPlatforms {
			names = append(names, v.Name)
		}
		http.Error(w, `{"detail":"不支持的平台链接。当前支持：`+strings.Join(names, "、")+`"}`, 400)
		return
	}

	// Cookie 处理
	cookie := strings.TrimSpace(req.Cookie)
	if cookie != "" {
		saveCookieRaw(platform, cookie)
	} else {
		if saved, ok := loadCookieRaw(platform); ok {
			cookie = saved
		}
	}

	taskID := fmt.Sprintf("%d", time.Now().UnixNano())
	platformName := SupportedPlatforms[platform].Name
	task := &Task{
		TaskID:    taskID,
		Status:    "queued",
		Stage:     "已加入队列",
		Progress:  0,
		Platform:  platformName,
		CreatedAt: time.Now().Unix(),
	}
	tasksMu.Lock()
	tasks[taskID] = task
	tasksMu.Unlock()

	go runDownloadTask(taskID, extracted, cookie, platform)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"task_id": taskID, "platform": platformName})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/api/status/")
	task := getTask(taskID)
	if task == nil {
		http.Error(w, `{"detail":"任务不存在"}`, 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(task)
}

// contentDisposition 构造兼容中文文件名的 Content-Disposition 头（RFC 5987）
// filename 参数提供 ASCII 回退名，filename* 提供 UTF-8 编码的真实文件名
func contentDisposition(filename string) string {
	var b strings.Builder
	for _, ch := range filename {
		if ch >= 32 && ch < 127 && ch != '"' && ch != '\\' {
			b.WriteRune(ch)
		}
	}
	fallback := b.String()
	if fallback == "" {
		fallback = "video" + filepath.Ext(filename)
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, url.PathEscape(filename))
}

// guessMimeType 按扩展名返回 MIME 类型
func guessMimeType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".flv":
		return "video/x-flv"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	default:
		return "application/octet-stream"
	}
}

// streamFile 流式推送文件给浏览器（参考 YT-Downloader 的实现）
// 显式设置 Content-Length，禁用代理缓冲，io.Copy 流式传输，
// 不用 http.ServeFile（其 Range/Seek 行为会在文件被外部改动时产生异常响应）
func streamFile(w http.ResponseWriter, r *http.Request, filePath, filename string) error {
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("文件不存在: %v", err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer f.Close()

	w.Header().Set("Content-Type", guessMimeType(filename))
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // 禁用 nginx 等反向代理的响应缓冲

	written, err := io.Copy(w, f)
	if err != nil {
		return fmt.Errorf("推送中断（已发送 %d/%d 字节）: %v", written, info.Size(), err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	log.Printf("📤 推送完成: %s（%d 字节）", filename, written)
	return nil
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/api/file/")
	task := getTask(taskID)
	if task == nil || task.Status != "completed" {
		http.Error(w, `{"detail":"文件不存在"}`, 404)
		return
	}

	// 拉取保护：传输期间任何清理路径都不得删除该任务文件
	atomic.AddInt32(&task.PullActive, 1)
	log.Printf("📥 浏览器开始拉取: %s（%s）", task.Filename, taskID)

	streamErr := streamFile(w, r, task.Filepath, task.Filename)
	if streamErr != nil {
		log.Printf("⚠️  拉取中断: %s — %v", taskID, streamErr)
	}

	if atomic.AddInt32(&task.PullActive, -1) == 0 && task.AutoClean {
		// 全平台统一策略：最后一次拉取结束后 60 秒宽限（允许浏览器重试），随后自动清理
		atomic.StoreInt64(&task.CleanupAfter, time.Now().Add(60*time.Second).Unix())
		log.Printf("推送结束，60 秒后自动清理: %s", taskID)
		scheduleTaskCleanup(taskID)
	}
}

func handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	taskID := strings.TrimPrefix(r.URL.Path, "/api/task/")
	tasksMu.Lock()
	delete(tasks, taskID)
	tasksMu.Unlock()
	os.RemoveAll(filepath.Join(DownloadDir, taskID))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func handleGetAllCookies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	result := make(map[string]interface{})
	for key, info := range SupportedPlatforms {
		cookie, ok := loadCookieRaw(key)
		result[key] = map[string]interface{}{
			"name":       info.Name,
			"has_cookie": ok,
			"cookie":     cookie,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func handleCookieCRUD(w http.ResponseWriter, r *http.Request) {
	// /api/cookie/{platform}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/cookie/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, `{"detail":"缺少平台参数"}`, 400)
		return
	}
	platform := parts[0]
	if _, ok := SupportedPlatforms[platform]; !ok {
		http.Error(w, `{"detail":"不支持的平台"}`, 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case "GET":
		cookie, ok := loadCookieRaw(platform)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"platform":   platform,
			"has_cookie": ok,
			"cookie":     cookie,
		})
	case "POST":
		var req struct {
			Cookie string `json:"cookie"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"detail":"Invalid JSON"}`, 400)
			return
		}
		if strings.TrimSpace(req.Cookie) == "" {
			http.Error(w, `{"detail":"cookie 不能为空"}`, 400)
			return
		}
		saveCookieRaw(platform, req.Cookie)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":       true,
			"platform": platform,
			"name":     SupportedPlatforms[platform].Name,
		})
	case "DELETE":
		deleted := deleteCookieRaw(platform)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":       true,
			"deleted":  deleted,
			"platform": platform,
		})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleDetectCookie 自动识别 Cookie 所属平台
// POST /api/cookie/detect  Body: {"cookie": "name1=value1; name2=value2"}
// 返回识别到的平台信息；未识别时 platform 为空，由前端提示用户手动选择
func handleDetectCookie(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"detail":"Invalid JSON"}`, 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.TrimSpace(req.Cookie) == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":       false,
			"platform": "",
		})
		return
	}
	platform, name, matchedKeys := detectCookiePlatform(req.Cookie)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":           platform != "",
		"platform":     platform,
		"name":         name,
		"matched_keys": matchedKeys,
	})
}

// handleBatchDeleteCookies 批量删除 Cookie
// POST /api/cookies/delete  Body: {"platforms": ["bilibili","douyin"]}
// platforms 包含 "*" 或为空数组时删除全部平台的 Cookie（全选删除）
func handleBatchDeleteCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Platforms []string `json:"platforms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"detail":"Invalid JSON"}`, 400)
		return
	}
	deleteAll := len(req.Platforms) == 0
	for _, p := range req.Platforms {
		if p == "*" {
			deleteAll = true
			break
		}
	}
	targets := req.Platforms
	if deleteAll {
		targets = targets[:0]
		for key := range SupportedPlatforms {
			targets = append(targets, key)
		}
	}
	var deleted []string
	for _, p := range targets {
		if _, ok := SupportedPlatforms[p]; !ok {
			continue
		}
		if deleteCookieRaw(p) {
			deleted = append(deleted, p)
		}
	}
	log.Printf("🍪 批量删除 Cookie: %v (实际删除 %v)", targets, deleted)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"deleted": deleted,
	})
}
