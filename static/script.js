// 多平台视频下载器 - 前端逻辑
// 交互流程：输入URL → 提交下载 → 轮询进度 → 自动下载文件
// Cookie 管理：粘贴 Cookie → 自动识别平台 → 自动保存为平台名 → 标签化展示 / 修改 / 删除

const $ = (id) => document.getElementById(id);

// ============ 状态管理 ============
const state = {
    pollTimer: null,
    currentTaskId: null,
    autoDownloaded: false,
    taskActive: false,      // 当前任务是否仍在进行（queued/processing）
    savedCookies: {},       // { platform_key: { name, cookie } }
    platforms: [],          // [{ key, name }]（来自 /api/platforms，用于手动选择）
    editLock: null,         // 正在编辑的平台 key（点击平台名进入编辑模式）
};

// 平台固定排序（chip 展示顺序）
const PLATFORM_ORDER = ["bilibili", "douyin", "kuaishou", "xiaohongshu", "likee", "instagram", "youtube"];

// ============ 事件绑定 ============
$("startBtn").addEventListener("click", startDownload);
$("resetBtn").addEventListener("click", resetUI);
$("url").addEventListener("keydown", (e) => {
    if (e.key === "Enter") startDownload();
});
$("url").addEventListener("paste", () => setTimeout(extractUrlFromText, 0));
$("url").addEventListener("blur", extractUrlFromText);

// Cookie 输入：防抖后自动识别平台并保存
$("cookie").addEventListener("input", debounce(handleCookieInput, 800));
$("cookieSaveBtn").addEventListener("click", saveCookieManually);
$("checkAll").addEventListener("change", toggleCheckAll);
$("deleteSelectedBtn").addEventListener("click", deleteSelectedCookies);

// ============ 无感提取 URL ============
// 从分享文案中提取视频链接
function extractUrlFromText() {
    const text = $("url").value.trim();
    if (!text) return;
    // 已是纯链接则不处理
    if (/^https?:\/\/[^\s]+$/.test(text)) return;

    const patterns = [
        /https?:\/\/(?:www\.|m\.|live\.)?bilibili\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/b23\.tv\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/bili2233\.cn\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.|v\.)?douyin\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.|v\.)?kuaishou\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.)?kwai\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.)?chenzhongtech\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.)?xiaohongshu\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.)?xhslink\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.|l\.)?likee\.video\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.)?instagram\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/instagr\.am\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/(?:www\.|m\.)?youtube\.com\/[^\s"'，。、；,.;:：)）】\]]+/i,
        /https?:\/\/youtu\.be\/[^\s"'，。、；,.;:：)）】\]]+/i,
    ];

    for (const p of patterns) {
        const m = text.match(p);
        if (m) {
            const url = m[0].replace(/[)）】\]]+$/, "");
            if (url !== text) {
                $("url").value = url;
            }
            return;
        }
    }
}

// ============ 平台检测 ============
function detectPlatformFromUrl(url) {
    const map = {
        bilibili: [/bilibili\.com/i, /b23\.tv/i, /bili2233\.cn/i],
        douyin: [/douyin\.com/i, /iesdouyin\.com/i],
        kuaishou: [/kuaishou\.com/i, /chenzhongtech\.com/i, /gifshow\.com/i, /kwai\.com/i],
        xiaohongshu: [/xiaohongshu\.com/i, /xhslink\.com/i],
        likee: [/likee\.video/i, /likee\.com/i],
        instagram: [/instagram\.com/i, /instagr\.am/i],
        youtube: [/youtube\.com/i, /youtu\.be/i],
    };
    for (const [key, regs] of Object.entries(map)) {
        if (regs.some(r => r.test(url))) return key;
    }
    return null;
}

function getPlatformName(key) {
    const names = {
        bilibili: "哔哩哔哩", douyin: "抖音", kuaishou: "快手",
        xiaohongshu: "小红书", likee: "Likee", instagram: "Instagram", youtube: "YouTube"
    };
    return names[key] || key;
}

// ============ Cookie 管理 ============

// 加载全部已保存的 Cookie 并渲染标签
async function refreshCookies() {
    try {
        const resp = await fetch("/api/cookies");
        const data = await resp.json();
        state.savedCookies = {};
        for (const [key, info] of Object.entries(data)) {
            if (info.has_cookie && info.cookie) {
                state.savedCookies[key] = { name: info.name || getPlatformName(key), cookie: info.cookie };
            }
        }
        renderChips();
    } catch (e) {
        console.error("加载 cookie 失败:", e);
    }
}

// 加载平台列表（用于识别失败时的手动选择）
async function loadPlatforms() {
    try {
        const resp = await fetch("/api/platforms");
        const data = await resp.json();
        state.platforms = (data.platforms || []).map(p => ({ key: p.key, name: p.name }));
    } catch (e) {
        state.platforms = PLATFORM_ORDER.map(k => ({ key: k, name: getPlatformName(k) }));
    }
    // 按固定顺序排序并填充下拉框
    state.platforms.sort((a, b) => PLATFORM_ORDER.indexOf(a.key) - PLATFORM_ORDER.indexOf(b.key));
    const sel = $("cookiePlatformSelect");
    sel.innerHTML = "";
    for (const p of state.platforms) {
        const opt = document.createElement("option");
        opt.value = p.key;
        opt.textContent = p.name;
        sel.appendChild(opt);
    }
}

// Cookie 输入处理：编辑模式自动保存到锁定平台；否则自动识别平台并保存
async function handleCookieInput() {
    const val = $("cookie").value.trim();

    // 清空输入框：解除编辑锁定、隐藏手动选择
    if (!val) {
        if (state.editLock) setCookieStatus("");
        state.editLock = null;
        $("cookieManual").style.display = "none";
        return;
    }

    // 编辑模式：直接保存到锁定的平台，不做平台识别
    if (state.editLock) {
        await saveCookie(state.editLock, val, "已更新");
        return;
    }

    // 自动识别平台
    let data;
    try {
        const resp = await fetch("/api/cookie/detect", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ cookie: val }),
        });
        data = await resp.json();
    } catch (e) {
        setCookieStatus("识别请求失败: " + e.message, "error");
        return;
    }

    if (data.ok && data.platform) {
        // 与已保存内容一致则不重复写入
        const saved = state.savedCookies[data.platform];
        if (saved && saved.cookie === val) {
            setCookieStatus(`已识别：${data.name}（与已保存内容一致）`, "saved");
            $("cookieManual").style.display = "none";
            return;
        }
        await saveCookie(data.platform, val, "已识别并保存");
        // 保存成功后清空输入框，Cookie 以平台标签形式展示
        $("cookie").value = "";
        $("cookieManual").style.display = "none";
    } else {
        // 识别失败：显示手动选择平台
        $("cookieManual").style.display = "flex";
        setCookieStatus("未识别所属平台，请手动选择后保存", "");
    }
}

// 保存 Cookie 到指定平台并刷新标签
async function saveCookie(platform, cookie, verb) {
    try {
        const resp = await fetch(`/api/cookie/${platform}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ cookie }),
        });
        if (!resp.ok) throw new Error("保存失败");
        state.savedCookies[platform] = { name: getPlatformName(platform), cookie };
        renderChips();
        setCookieStatus(`${verb}：${getPlatformName(platform)}`, "saved");
    } catch (e) {
        setCookieStatus("保存失败: " + e.message, "error");
    }
}

// 手动选择平台保存（识别失败时的兜底）
async function saveCookieManually() {
    const val = $("cookie").value.trim();
    if (!val) return;
    const platform = $("cookiePlatformSelect").value;
    await saveCookie(platform, val, "已保存");
    $("cookie").value = "";
    $("cookieManual").style.display = "none";
}

// 渲染已保存的平台标签
function renderChips() {
    const wrap = $("cookieChips");
    const keys = PLATFORM_ORDER.filter(k => state.savedCookies[k])
        .concat(Object.keys(state.savedCookies).filter(k => !PLATFORM_ORDER.includes(k)));
    wrap.innerHTML = "";

    for (const key of keys) {
        const info = state.savedCookies[key];
        const chip = document.createElement("div");
        chip.className = "cookie-chip";
        chip.dataset.platform = key;

        // 勾选框（批量删除用）
        const check = document.createElement("input");
        check.type = "checkbox";
        check.className = "chip-check";
        check.dataset.platform = key;
        check.addEventListener("change", syncCheckAllState);
        chip.appendChild(check);

        // 平台名（点击进入编辑模式）
        const name = document.createElement("span");
        name.className = "chip-name";
        name.textContent = info.name;
        name.title = "点击查看 / 修改 Cookie";
        name.addEventListener("click", () => enterEditMode(key));
        chip.appendChild(name);

        // 单个删除按钮
        const del = document.createElement("button");
        del.type = "button";
        del.className = "chip-del";
        del.textContent = "✕";
        del.title = "删除该平台 Cookie";
        del.addEventListener("click", (e) => {
            e.stopPropagation();
            deleteCookie(key);
        });
        chip.appendChild(del);

        wrap.appendChild(chip);
    }

    $("cookieSaved").style.display = keys.length > 0 ? "block" : "none";
    if (keys.length === 0) {
        $("checkAll").checked = false;
    }
}

// 进入编辑模式：把已保存的 Cookie 填回输入框，锁定平台
function enterEditMode(key) {
    const info = state.savedCookies[key];
    if (!info) return;
    state.editLock = key;
    $("cookie").value = info.cookie;
    $("cookieManual").style.display = "none";
    setCookieStatus(`正在编辑：${info.name}（修改后自动保存，清空输入框退出编辑）`, "");
    $("cookie").focus();
}

// 删除单个平台的 Cookie
async function deleteCookie(key) {
    const name = getPlatformName(key);
    if (!confirm(`确定删除 ${name} 的 Cookie？`)) return;
    try {
        await fetch(`/api/cookie/${key}`, { method: "DELETE" });
        delete state.savedCookies[key];
        if (state.editLock === key) {
            state.editLock = null;
            $("cookie").value = "";
        }
        renderChips();
        setCookieStatus(`已删除：${name}`, "");
    } catch (e) {
        setCookieStatus("删除失败: " + e.message, "error");
    }
}

// 全选 / 取消全选
function toggleCheckAll() {
    const checked = $("checkAll").checked;
    document.querySelectorAll(".chip-check").forEach(c => { c.checked = checked; });
}

// 任一标签勾选变化时，同步"全选"复选框状态
function syncCheckAllState() {
    const checks = document.querySelectorAll(".chip-check");
    const all = checks.length > 0 && [...checks].every(c => c.checked);
    $("checkAll").checked = all;
}

// 删除选中的 Cookie（支持全选删除所有）
async function deleteSelectedCookies() {
    const selected = [...document.querySelectorAll(".chip-check:checked")].map(c => c.dataset.platform);
    if (selected.length === 0) {
        setCookieStatus("请先勾选要删除的平台", "");
        return;
    }
    const allChecked = $("checkAll").checked;
    const msg = allChecked
        ? "确定删除所有已保存的 Cookie？"
        : `确定删除选中的 ${selected.length} 个平台 Cookie？`;
    if (!confirm(msg)) return;
    try {
        await fetch("/api/cookies/delete", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ platforms: selected }),
        });
        for (const key of selected) {
            delete state.savedCookies[key];
            if (state.editLock === key) {
                state.editLock = null;
                $("cookie").value = "";
            }
        }
        renderChips();
        setCookieStatus(allChecked ? "已删除全部 Cookie" : `已删除 ${selected.length} 个平台 Cookie`, "");
    } catch (e) {
        setCookieStatus("批量删除失败: " + e.message, "error");
    }
}

function setCookieStatus(text, cls) {
    const el = $("cookieStatus");
    el.textContent = text;
    el.className = "cookie-status" + (cls ? " " + cls : "");
}

// ============ 下载流程 ============
async function startDownload() {
    const url = $("url").value.trim();
    const cookie = $("cookie").value.trim();

    if (!url) {
        showMessage("请输入视频链接", "error");
        return;
    }

    setButtonLoading(true);
    hideMessage();

    try {
        const resp = await fetch("/api/download", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ url, cookie }),
        });
        const data = await resp.json();
        if (!resp.ok) {
            throw new Error(data.detail || "提交失败");
        }

        state.currentTaskId = data.task_id;
        state.autoDownloaded = false;
        state.taskActive = true;

        // 显示进度面板（"新建任务"按钮在此过程中始终可见）
        $("inputPanel").hidden = true;
        $("progressPanel").hidden = false;
        $("actionsArea").style.display = "none";
        resetProgressDisplay(data.platform);

        // 开始轮询
        startPolling();
    } catch (e) {
        showMessage(e.message, "error");
        setButtonLoading(false);
    }
}

function setButtonLoading(loading) {
    const btn = $("startBtn");
    btn.disabled = loading;
    btn.querySelector(".btn-text").textContent = loading ? "处理中..." : "开始下载";
}

function resetProgressDisplay(platformName) {
    $("progressBar").style.width = "0%";
    $("progressPercent").textContent = "0%";
    $("title").textContent = "—";
    $("platform").textContent = platformName || "—";
    $("videoFmt").textContent = "—";
    $("audioFmt").textContent = "—";
    $("stage").textContent = "排队中...";
}

function startPolling() {
    if (state.pollTimer) clearInterval(state.pollTimer);
    state.pollTimer = setInterval(pollStatus, 1500);
}

async function pollStatus() {
    if (!state.currentTaskId) return;
    try {
        const resp = await fetch(`/api/status/${state.currentTaskId}`);
        const data = await resp.json();

        // 更新进度
        const pct = Math.round(data.progress || 0);
        $("progressBar").style.width = pct + "%";
        $("progressPercent").textContent = pct + "%";
        $("stage").textContent = data.stage || "—";

        if (data.title) $("title").textContent = data.title;
        if (data.platform) $("platform").textContent = data.platform;

        if (data.video_format) {
            const vf = data.video_format;
            $("videoFmt").textContent = `${vf.resolution} @ ${vf.fps || "?"}fps`;
        }
        if (data.audio_format) {
            const af = data.audio_format;
            $("audioFmt").textContent = `${af.abr || "?"}kbps`;
        }

        // 状态处理（"新建任务"按钮始终可见，此处仅控制"保存到本地"）
        if (data.status === "completed") {
            clearInterval(state.pollTimer);
            state.pollTimer = null;
            state.taskActive = false;
            const sizeText = data.filesize ? `（${(data.filesize / 1048576).toFixed(1)} MB）` : "";
            $("stage").textContent = `下载完成${sizeText}，等待浏览器拉取`;
            showMessage("下载完成", "success");
            const link = $("downloadLink");
            link.href = `/api/file/${state.currentTaskId}`;
            $("actionsArea").style.display = "flex";
            // 自动触发下载
            if (!state.autoDownloaded) {
                state.autoDownloaded = true;
                setTimeout(() => link.click(), 500);
            }
        } else if (data.status === "failed") {
            clearInterval(state.pollTimer);
            state.pollTimer = null;
            state.taskActive = false;
            showMessage("下载失败：" + (data.error || "未知错误"), "error");
            $("actionsArea").style.display = "none";
        }
    } catch (e) {
        console.error("轮询失败:", e);
    }
}

// ============ UI 辅助 ============
function showMessage(msg, type) {
    const el = $("statusMsg");
    el.textContent = msg;
    el.className = "message show " + (type || "");
}

function hideMessage() {
    $("statusMsg").className = "message";
}

function resetUI() {
    // 任务进行中点击"新建任务"：先确认，再取消后端任务
    if (state.taskActive) {
        if (!confirm("当前任务仍在下载中，确定取消并新建任务吗？")) return;
    }
    if (state.pollTimer) {
        clearInterval(state.pollTimer);
        state.pollTimer = null;
    }
    if (state.currentTaskId) {
        fetch(`/api/task/${state.currentTaskId}`, { method: "DELETE" }).catch(() => {});
    }
    state.currentTaskId = null;
    state.autoDownloaded = false;
    state.taskActive = false;

    $("url").value = "";
    $("cookie").value = "";
    state.editLock = null;
    $("cookieManual").style.display = "none";
    setCookieStatus("");
    $("inputPanel").hidden = false;
    $("progressPanel").hidden = true;
    $("actionsArea").style.display = "none";
    $("downloadLink").removeAttribute("href");
    hideMessage();
    setButtonLoading(false);
}

function debounce(fn, delay) {
    let timer;
    return (...args) => {
        clearTimeout(timer);
        timer = setTimeout(() => fn(...args), delay);
    };
}

// ============ 初始化 ============
loadPlatforms();
refreshCookies();
