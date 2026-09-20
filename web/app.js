// ==========================================================================
// Note-Web 前端应用
// 基于 hash 路由的 SPA，包含文件库浏览和 PDF 阅读器两大视图
// ==========================================================================

// 快捷获取 DOM 元素
const $ = id => document.getElementById(id);

// ===== 全局状态 =====

// 文件库状态
let allPDFs = [];          // 从后端获取的全部 PDF 列表
let currentFolder = '';    // 当前所在文件夹的相对路径（如 "1/2"）
let isSearchMode = false;  // 是否处于搜索模式

// 阅读器状态
let currentPDF = null;       // 当前 PDF 在数据库中的 ID
let currentPDFKey = null;    // 当前 PDF 的 cache_key（路径哈希，用于 URL 路由）
let currentPDFFolder = '';   // 当前 PDF 所在的文件夹路径（用于返回按钮导航）
let currentOutlineSource = '';// 当前目录来源（goodnotes 或 PDF）
let currentPage = 1;         // 当前页码
let pageCount = 0;           // 当前 PDF 总页数
let requestID = 0;           // 请求计数器，用于丢弃过时的页面加载请求
let preloadedPages = new Set(); // 已预加载的页面 URL 集合，避免重复请求

// 跳转历史状态
// 每个条目是 { target: 目标页码, anchor: 锚点页码 }
// 当用户在锚点 ±10 页范围内时，对应方向的箭头可用
let jumpBack = null;     // 向后跳转目标（如从200跳到100后，jumpBack = { target: 200, anchor: 100 }）
let jumpForward = null;  // 向前跳转目标

// 缩放状态
let zoom = 1;                // 当前缩放倍率（1~4）
let panX = 0;                // X 方向平移偏移（像素）
let panY = 0;                // Y 方向平移偏移（像素）
let pinchStartDist = 0;      // 双指捏合开始时的两指距离
let pinchStartFocal = null;  // 双指捏合开始时的焦点（相对图片左上角）
let pinchStartZoom = 1;      // 双指捏合开始时的缩放倍率
let pinchStartPanX = 0;      // 双指捏合开始时的 X 平移
let pinchStartPanY = 0;      // 双指捏合开始时的 Y 平移
let panStartX = 0;           // 单指拖拽开始时的 X 坐标
let panStartY = 0;           // 单指拖拽开始时的 Y 坐标
let panOrigX = 0;            // 单指拖拽开始时的 panX 值
let panOrigY = 0;            // 单指拖拽开始时的 panY 值
let swipeStartX = 0;         // 滑动手势开始时的 X 坐标
let swipeStartY = 0;         // 滑动手势开始时的 Y 坐标

// 根据当前入口路径推断 API 前缀：
// /                 -> /api/...
// /proxy-index/     -> /proxy-index/api/...
// /proxy-index      -> 服务端会重定向到 /proxy-index/
const APP_BASE_PATH = (() => {
    let path = location.pathname || '/';
    if (path.endsWith('/index.html')) path = path.slice(0, -'/index.html'.length) || '/';
    path = path.replace(/\/+$/, '');
    return path === '' || path === '/' ? '' : path;
})();

// ===== SVG 图标常量 =====
const ICON_FOLDER = '<svg class="folder-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>';
const ICON_FILE = '<svg class="pdf-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>';
const ICON_BOOKMARK = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/></svg>';
const ICON_BOOKMARK_FILL = '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/></svg>';

// ===== 工具函数 =====

/** 转义 HTML 特殊字符，防止 XSS（M11: 同时转义引号，防止属性上下文注入） */
function escapeHtml(s) {
    const div = document.createElement('div');
    div.textContent = s;
    return div.innerHTML;
}

/** 安全转义 HTML 属性值（不仅转义HTML实体，也转义引号） */
function escapeAttr(s) {
    return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/'/g, '&#39;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/** 将字节数格式化为人类可读的大小（KB/MB） */
function formatSize(bytes) {
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + ' KB';
    return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

/** 显示一条底部悬浮提示消息，默认 2 秒后自动消失 */
function showToast(msg, duration = 2000) {
    const t = $('toast');
    t.textContent = msg;
    t.classList.remove('hidden');
    clearTimeout(t._timer);
    t._timer = setTimeout(() => t.classList.add('hidden'), duration);
}

/** 发起 API 请求，返回 Response 对象，失败时抛出异常 */
async function api(path, opts) {
    const res = await fetch(apiPath(path), opts);
    if (!res.ok) {
        const err = await res.json().catch(() => ({ error: res.statusText }));
        throw new Error(err.error || 'Request failed');
    }
    return res;
}

/** 发起 GET 请求并解析 JSON 响应 */
async function apiJSON(path) {
    const res = await fetch(apiPath(path));
    if (!res.ok) throw new Error('Request failed');
    return res.json();
}

/** 给站点子路径代理补上前缀 */
function apiPath(path) {
    if (!APP_BASE_PATH || /^https?:\/\//i.test(path)) return path;
    return APP_BASE_PATH + path;
}

/**
 * 根据页码生成对应页面的 API URL
 * @param {number} page - 页码
 * @param {string|number} [bust] - 缓存版本号。传入时会在 URL 上附加 v 参数，
 *   强制浏览器重新获取该页（用于页面内容已更新的场景）
 */
function pageURL(page, bust) {
    const url = apiPath(`/api/pdfs/${currentPDFKey}/page/${page}`);
    return bust ? `${url}?v=${encodeURIComponent(bust)}` : url;
}

// ===== URL 路由 =====

/**
 * 解析当前 URL hash 并切换到对应视图
 * 路由格式：
 *   #/                     -> 文件库根目录
 *   #/folder/1/2           -> 文件库子目录 "1/2"
 *   #/read/{cacheKey}      -> 阅读器（自动恢复上次页码）
 *   #/read/{cacheKey}/5    -> 阅读器第5页
 */
function handleRoute() {
    const hash = location.hash.slice(1) || '/';
    const parts = hash.split('/').filter(Boolean);
    if (parts[0] === 'read' && parts[1]) {
        showReader(parts[1], parts[2] ? parseInt(parts[2]) : null);
    } else if (parts[0] === 'folder') {
        const folder = parts.slice(1).map(p => { try { return decodeURIComponent(p); } catch (e) { return p; } }).join('/');
        showFolder(folder);
    } else {
        showFolder('');
    }
}
window.addEventListener('hashchange', handleRoute);

// ===== 文件库 / 文件夹视图 =====

/** 从后端加载所有 PDF 列表，然后根据当前 hash 路由渲染对应视图 */
async function loadLibrary() {
    try {
        allPDFs = await apiJSON('/api/pdfs');
        handleRoute();
        loadCacheStats();
    } catch (e) { showToast('加载失败'); }
}

/** 加载全局缓存占用统计，显示为右上角圆环图 */
async function loadCacheStats() {
    try {
        const stats = await apiJSON('/api/cache/stats');
        const pct = Math.min(100, Math.max(0, stats.pct || 0));
        // 圆周长 2*PI*r = 2*3.1416*10 ≈ 62.832
        const circumference = 2 * Math.PI * 10;
        const offset = circumference * (1 - pct / 100);
        const ring = $('cache-ring');
        ring.setAttribute('stroke-dashoffset', offset);
        // 用量 > 90% 红色，> 70% 橙色，否则蓝色
        ring.setAttribute('stroke', pct > 90 ? 'var(--error)' : pct > 70 ? 'var(--warning)' : 'var(--accent)');
        $('cache-tooltip').textContent = `${stats.total_mb.toFixed(0)}MB / ${stats.max_mb}MB (${pct.toFixed(0)}%)`;
    } catch (e) { /* 静默失败 */ }
}

/** 显示指定文件夹的内容，隐藏阅读器 */
function showFolder(folder) {
    stopDocWatch();
    currentFolder = folder;
    $('library').classList.remove('hidden');
    $('reader').classList.add('hidden');
    $('search').value = '';
    isSearchMode = false;
    renderBreadcrumb(folder);
    renderFolder(folder);
}

/**
 * 渲染面包屑导航（根目录 / 子目录1 / 子目录2）
 * 每一级都是可点击链接，最后一级为当前目录（不可点击）
 */
function renderBreadcrumb(folder) {
    const bc = $('breadcrumb');
    let html = '<a href="#/">根目录</a>';
    if (folder) {
        const parts = folder.split('/');
        let path = '';
        for (let i = 0; i < parts.length; i++) {
            path = path ? path + '/' + parts[i] : parts[i];
            if (i < parts.length - 1) {
                html += `<span class="sep">/</span><a href="#/folder/${encodeURIComponent(path)}">${escapeHtml(parts[i])}</a>`;
            } else {
                html += `<span class="sep">/</span><span class="current">${escapeHtml(parts[i])}</span>`;
            }
        }
    }
    bc.innerHTML = html;
}

/**
 * 从 allPDFs 中提取指定文件夹下的内容
 * @param {string} folder - 文件夹相对路径，空字符串表示根目录
 * @returns {{folders: Array, pdfs: Array}} folders 是 [名称, 文件数] 数组，pdfs 是 PDF 对象数组
 */
function getFolderContents(folder) {
    const prefix = folder ? folder + '/' : '';
    const folders = new Map(); // 子文件夹名 -> 文件计数
    const pdfs = [];           // 直接位于该文件夹下的 PDF

    for (const pdf of allPDFs) {
        const rp = pdf.relative_path;
        if (!rp.startsWith(prefix)) continue;       // 不在当前文件夹下
        const remaining = rp.slice(prefix.length);   // 去掉前缀后的剩余路径
        const slashIdx = remaining.indexOf('/');
        if (slashIdx === -1) {
            // 没有更深层的斜杠，说明是当前文件夹的直接文件
            pdfs.push(pdf);
        } else {
            // 有斜杠，说明在子文件夹中，记录子文件夹名
            const name = remaining.slice(0, slashIdx);
            folders.set(name, (folders.get(name) || 0) + 1);
        }
    }
    return {
        folders: [...folders.entries()].sort((a, b) => a[0].localeCompare(b[0], 'zh')),
        pdfs: pdfs.sort((a, b) => a.title.localeCompare(b.title, 'zh'))
    };
}

/**
 * 生成 PDF 的缓存状态信息（用于文件列表显示）
 * - 已完成缓存：显示绿色"已缓存"徽章，无进度条
 * - 缓存进行中：显示"缓存中 X/Y"和进度条
 * - 等待缓存：显示"等待缓存"和空进度条
 * - 未缓存：不显示任何缓存信息
 */
function renderCacheInfo(pdf) {
    const metaParts = [`${pdf.page_count} 页`, formatSize(pdf.file_size)];
    if (pdf.outline_source === 'goodnotes') {
        metaParts.push('<span class="outline-badge">GoodNotes 目录</span>');
    } else if (pdf.outline_source === 'deep') {
        metaParts.push('<span class="outline-badge">深度扫描</span>');
    } else if (pdf.outline_source === 'pdf') {
        metaParts.push('<span class="outline-badge">PDF 目录</span>');
    }
    let cacheBar = '';
    if (pdf.pinned) {
        if (pdf.cached_pages >= pdf.page_count && pdf.page_count > 0) {
            // 缓存完成
            metaParts.push('<span class="pdf-badge">已缓存</span>');
        } else if (pdf.cached_pages > 0) {
            // 缓存进行中
            metaParts.push(`缓存中 ${pdf.cached_pages}/${pdf.page_count}`);
            const pct = (pdf.cached_pages / pdf.page_count * 100).toFixed(0);
            cacheBar = `<div class="pdf-cache-bar"><div class="pdf-cache-fill" style="width:${pct}%"></div></div>`;
        } else {
            // 已标记但尚未开始缓存
            metaParts.push('等待缓存');
            cacheBar = `<div class="pdf-cache-bar"><div class="pdf-cache-fill" style="width:0%"></div></div>`;
        }
    }
    return { metaParts: metaParts.join(' · '), cacheBar };
}

/** 渲染文件夹内容：子文件夹列表 + PDF 文件列表 */
function renderFolder(folder) {
    const list = $('pdf-list');
    const { folders, pdfs } = getFolderContents(folder);
    if (folders.length === 0 && pdfs.length === 0) {
        list.innerHTML = '<div style="text-align:center;padding:40px;color:var(--text-muted)">暂无文件</div>';
        return;
    }
    let html = '';
    // 渲染子文件夹
    for (const [name, count] of folders) {
        const path = folder ? folder + '/' + name : name;
        html += `<div class="folder-item" data-folder="${escapeHtml(path)}">${ICON_FOLDER}<span class="folder-name">${escapeHtml(name)}</span><span class="folder-count">${count} 个文件</span></div>`;
    }
    // 渲染 PDF 文件
    for (const pdf of pdfs) {
        const ci = renderCacheInfo(pdf);
        html += `<div class="pdf-item" data-key="${pdf.cache_key}">
            ${ICON_FILE}<div class="pdf-info">
                <div class="pdf-title">${escapeHtml(pdf.title)}</div>
                <div class="pdf-meta">${ci.metaParts}</div>${ci.cacheBar}
            </div>
            <button class="pin-btn ${pdf.pinned ? 'pinned' : ''}" data-pin="${pdf.cache_key}">${pdf.pinned ? ICON_BOOKMARK_FILL : ICON_BOOKMARK}</button>
        </div>`;
    }
    list.innerHTML = html;
    bindListItems(list);
}

/** 渲染搜索结果（扁平列表，显示文件所在目录） */
function renderSearchResults(pdfs) {
    const list = $('pdf-list');
    if (pdfs.length === 0) {
        list.innerHTML = '<div style="text-align:center;padding:40px;color:var(--text-muted)">未找到匹配文件</div>';
        return;
    }
    let html = '';
    for (const pdf of pdfs) {
        const dir = pdf.relative_path.includes('/') ? pdf.relative_path.slice(0, pdf.relative_path.lastIndexOf('/')) : '';
        const ci = renderCacheInfo(pdf);
        const metaParts = ci.metaParts + (dir ? ' · ' + escapeHtml(dir) : '');
        html += `<div class="pdf-item" data-key="${pdf.cache_key}">
            ${ICON_FILE}<div class="pdf-info">
                <div class="pdf-title">${escapeHtml(pdf.title)}</div>
                <div class="pdf-meta">${metaParts}</div>${ci.cacheBar}
            </div>
            <button class="pin-btn ${pdf.pinned ? 'pinned' : ''}" data-pin="${pdf.cache_key}">${pdf.pinned ? ICON_BOOKMARK_FILL : ICON_BOOKMARK}</button>
        </div>`;
    }
    list.innerHTML = html;
    bindListItems(list);
}

/** 为列表中的文件夹项、PDF 项和缓存按钮绑定点击事件 */
function bindListItems(list) {
    // 文件夹点击 -> 导航到子文件夹
    list.querySelectorAll('.folder-item').forEach(el => {
        el.addEventListener('click', () => { location.hash = '#/folder/' + encodeURIComponent(el.dataset.folder); });
    });
    // PDF 点击 -> 打开阅读器
    list.querySelectorAll('.pdf-item').forEach(el => {
        el.addEventListener('click', e => {
            if (e.target.closest('.pin-btn')) return; // 点击的是缓存按钮，不打开阅读器
            location.hash = '#/read/' + el.dataset.key;
        });
    });
    // 缓存按钮点击 -> 切换缓存状态
    list.querySelectorAll('.pin-btn').forEach(btn => {
        btn.addEventListener('click', e => {
            e.stopPropagation();
            const key = btn.dataset.pin;
            togglePin(key, btn, allPDFs.find(p => p.cache_key === key));
        });
    });
}

// ===== 搜索 =====
// 输入框内容变化时，按标题和路径过滤 PDF 列表
$('search').addEventListener('input', e => {
    const q = e.target.value.toLowerCase().trim();
    if (!q) { isSearchMode = false; renderFolder(currentFolder); return; } // 清空搜索 -> 回到文件夹视图
    isSearchMode = true;
    renderSearchResults(allPDFs.filter(p =>
        p.title.toLowerCase().includes(q) || p.relative_path.toLowerCase().includes(q)));
});

// ===== 缓存标记 / 取消标记 =====

/**
 * 切换 PDF 的主动缓存状态
 * 标记缓存：调用后端 PUT /pin，触发全量预渲染
 * 取消缓存：调用后端 DELETE /pin，缓存转为被动模式（可被 LRU 淘汰）
 */
async function togglePin(key, btn, pdf) {
    if (!pdf) return;
    try {
        if (pdf.pinned) {
            // 取消缓存
            await api(`/api/pdfs/${key}/pin`, { method: 'DELETE' });
            pdf.pinned = false; pdf.cached_pages = 0; pdf.cache_active = false;
            btn.classList.remove('pinned'); btn.innerHTML = ICON_BOOKMARK;
            showToast('已取消缓存');
            if (isSearchMode) { $('search').dispatchEvent(new Event('input')); }
            else renderFolder(currentFolder);
        } else {
            // 标记缓存
            await api(`/api/pdfs/${key}/pin`, { method: 'PUT' });
            pdf.pinned = true;
            btn.classList.add('pinned'); btn.innerHTML = ICON_BOOKMARK_FILL;
            showToast('已标记缓存，后台渲染中...');
            pollCacheStatus(key, pdf); // 开始轮询缓存进度
        }
    } catch (e) { showToast('操作失败: ' + e.message); }
}

/**
 * 每 3 秒轮询 PDF 的缓存状态，更新文件列表中的进度条
 * 当缓存完成或取消缓存时停止轮询
 */
async function pollCacheStatus(key, pdf) {
    const interval = setInterval(async () => {
        try {
            const status = await apiJSON(`/api/pdfs/${key}/cache`);
            if (!status.pinned) { clearInterval(interval); return; }
            pdf.cached_pages = status.cached;
            if (!isSearchMode) renderFolder(currentFolder);
            else { $('search').dispatchEvent(new Event('input')); }
            if (status.cached >= status.total && !status.rebuilding) clearInterval(interval);
        } catch (e) { }
    }, 3000);
}

// ===== 文档更新监视 =====
//
// 阅读器打开期间每 10 秒查询一次缓存状态。后端重新渲染缓存时 cache_version 递增，
// 并在 changed_pages 中给出本次真正变化的页码，因此客户端只需要刷新变化的页面，
// 而不必让用户手动清空缓存或重新下载整份文档。

const DOC_WATCH_INTERVAL = 10000;  // 轮询间隔（毫秒）
let docWatchTimer = null;          // 定时器句柄
let docCacheVersion = null;        // 已同步的缓存版本号

/** 开始监视当前文档的更新（重复调用会先清理旧定时器） */
function startDocWatch() {
    stopDocWatch();
    docWatchTimer = setInterval(checkDocUpdate, DOC_WATCH_INTERVAL);
}

/** 停止监视（离开阅读器时调用） */
function stopDocWatch() {
    if (docWatchTimer) { clearInterval(docWatchTimer); docWatchTimer = null; }
    docCacheVersion = null;
}

/** 查询缓存状态，发现文档更新时只刷新真正变化的页面 */
async function checkDocUpdate() {
    if (!docWatchTimer || !currentPDFKey) return;
    if ($('reader').classList.contains('hidden')) return; // 已离开阅读器

    let status;
    try {
        status = await apiJSON(`/api/pdfs/${currentPDFKey}/cache`);
    } catch (e) {
        return; // 网络抖动等，下次轮询再试
    }
    // 请求返回时用户可能已经离开阅读器
    if (!docWatchTimer || $('reader').classList.contains('hidden')) return;

    // 页数变化（文档中插入或删除了页面）：首次轮询也要校正
    if (status.total > 0 && status.total !== pageCount) {
        pageCount = status.total;
        $('page-total').textContent = pageCount;
        $('page-input').max = pageCount;
        if (currentPage > pageCount) currentPage = pageCount;
        $('next-btn').disabled = currentPage >= pageCount;
    }

    const version = status.version || 0;
    if (docCacheVersion === null) { docCacheVersion = version; return; } // 首次只记录基线
    if (version === docCacheVersion) return;
    docCacheVersion = version;

    // changed_pages 为空表示变化范围未知（例如整份被动缓存被清除），此时刷新当前页
    const changed = Array.isArray(status.changed_pages) ? status.changed_pages : [];
    if (changed.length === 0 || changed.indexOf(currentPage) !== -1) {
        refreshCurrentPage(version);
        showToast('文档已更新');
    }
}

/** 强制重新加载当前页（保留缩放和阅读位置），bust 用于绕过浏览器缓存 */
function refreshCurrentPage(bust) {
    const url = pageURL(currentPage, bust);
    const myID = ++requestID; // 使正在进行的加载请求失效
    const loader = $('page-loader');

    preloadedPages.clear();
    loader.classList.remove('hidden');
    loader.querySelector('span').textContent = '更新中...';

    const pre = new Image();
    pre.onload = () => {
        if (myID !== requestID) return;
        preloadedPages.add(url);
        $('page-base').src = url;
        loader.classList.add('hidden');
    };
    pre.onerror = () => {
        if (myID !== requestID) return;
        loader.classList.add('hidden');
        showToast('页面加载失败');
    };
    pre.src = url;
}

// ===== 阅读器 =====

/**
 * 打开阅读器，加载指定 PDF
 * @param {string} key - PDF 的 cache_key
 * @param {number|null} page - 指定页码，null 则恢复上次阅读位置
 */
function showReader(key, page) {
    const pdf = allPDFs.find(p => p.cache_key === key);
    if (!pdf) { showToast('PDF not found'); location.hash = '#/'; return; }
    currentPDF = pdf.id;
    currentPDFKey = key;
    currentOutlineSource = pdf.outline_source || '';
    $('deep-scan-input').checked = pdf.deep_scan !== false;
    // 计算 PDF 所在文件夹路径（用于返回按钮导航）
    currentPDFFolder = pdf.relative_path.includes('/') ? pdf.relative_path.slice(0, pdf.relative_path.lastIndexOf('/')) : '';
    pageCount = pdf.page_count;
    // 恢复上次阅读位置
    const savedPage = parseInt(localStorage.getItem(`pdf-${key}-page`)) || 1;
    currentPage = page || savedPage;
    if (currentPage > pageCount) currentPage = 1;

    // 重置跳转历史和预加载缓存
    jumpBack = null; jumpForward = null;
    preloadedPages.clear();

    // 切换视图
    $('library').classList.add('hidden');
    $('reader').classList.remove('hidden');
    $('reader-title').textContent = pdf.title;
    $('page-total').textContent = pageCount;
    $('page-input').max = pageCount;
    updatePinButton(pdf);
    $('overlay-wrapper').style.display = 'none'; // 确保叠加层隐藏
    resetZoom();
    updateJumpButtons();
    loadPage(currentPage, 'fade');
    startDocWatch(); // 监视文档更新（只刷新变化的页面）
}

/** 更新阅读器头部的缓存按钮状态 */
function updatePinButton(pdf) {
    const btn = $('reader-pin');
    btn.className = 'icon-btn pin-btn ' + (pdf.pinned ? 'pinned' : '');
    btn.innerHTML = pdf.pinned ? ICON_BOOKMARK_FILL : ICON_BOOKMARK;
    btn.onclick = () => togglePin(currentPDFKey, btn, pdf);
}

// ===== 智能页面加载 =====

const PAGE_SLIDE_DURATION = 200;

/** 先提交动画起始位置，避免浏览器合并样式后直接显示结束位置 */
function animatePageWrapper(wrapper, fromTransform, toTransform, myID) {
    wrapper.style.display = '';
    wrapper.style.transition = 'none';
    wrapper.style.transform = fromTransform;
    void wrapper.offsetWidth;
    requestAnimationFrame(() => {
        if (myID !== requestID) return;
        wrapper.style.transition = `transform ${PAGE_SLIDE_DURATION}ms ease-out`;
        wrapper.style.transform = toTransform;
    });
}

/**
 * 加载指定页面，根据导航方式选择不同的加载策略
 * @param {number} page - 目标页码
 * @param {string} direction - 'next' 下一页（叠加滑动）、'prev' 上一页（叠加滑动）、'fade' 跳转（直接替换）
 *
 * 智能加载逻辑：
 * 1. requestID 机制：每次加载递增 ID，过时的 onload 回调被丢弃
 * 2. preloadedPages 集合：已加载的页面 URL 被记录，命中时跳过 loader 动画
 * 3. 预加载：延迟 200ms 后预加载相邻页面，若用户已翻走则取消
 */
function loadPage(page, direction, bust) {
    if (page < 1 || page > pageCount) return;
    const myID = ++requestID; // 本次加载的唯一标识
    const isSequential = direction === 'next' || direction === 'prev';
    const overlayWrapper = $('overlay-wrapper');

    // 新导航会使旧回调失效，同时也要立即清掉旧动画留下的可见叠加层。
    overlayWrapper.style.display = 'none';
    overlayWrapper.style.transition = '';
    overlayWrapper.style.transform = '';

    // 更新 UI 状态
    currentPage = page;
    $('page-input').value = page;
    $('prev-btn').disabled = page <= 1;
    $('next-btn').disabled = page >= pageCount;
    localStorage.setItem(`pdf-${currentPDFKey}-page`, page); // 保存阅读位置
    history.replaceState(null, '', `#/read/${currentPDFKey}/${page}`); // 更新 URL（不产生历史记录）
    resetZoom(); // 翻页时重置缩放

    // 顺序翻页时检查跳转历史范围
    if (isSequential) checkJumpRange();
    updateJumpButtons();

    const url = pageURL(page, bust);
    const isPreloaded = preloadedPages.has(url);

    // 根据导航方式选择加载策略
    if (isSequential) {
        slidePage(url, direction, myID, isPreloaded); // 叠加层滑动动画
    } else {
        swapPage(url, myID, isPreloaded); // 直接替换（无动画）
    }

    // 智能预加载：延迟 200ms，确保用户停在此页后才预加载相邻页
    setTimeout(() => {
        if (myID !== requestID) return; // 用户已翻走，取消预加载
        // 预加载后 2 页
        for (let p = page + 1; p <= Math.min(page + 2, pageCount); p++) {
            const u = pageURL(p);
            if (!preloadedPages.has(u)) {
                const pre = new Image();
                pre.onload = () => preloadedPages.add(u);
                pre.src = u;
            }
        }
        // 预加载前 1 页
        if (page > 1) {
            const u = pageURL(page - 1);
            if (!preloadedPages.has(u)) {
                const pre = new Image();
                pre.onload = () => preloadedPages.add(u);
                pre.src = u;
            }
        }
    }, 200);
}

/**
 * 直接替换页面（用于页码跳转，无动画）
 * 如果页面已预加载，立即设置；否则显示加载动画，等待加载完成
 */
function swapPage(url, myID, isPreloaded) {
    const base = $('page-base');
    const loader = $('page-loader');
    const finish = () => {
        if (myID !== requestID) return; // 过时请求，丢弃
        preloadedPages.add(url);
        loader.classList.add('hidden');
        base.src = url;
    };
    if (isPreloaded) {
        finish();
    } else {
        loader.classList.remove('hidden');
        loader.querySelector('span').textContent = '渲染中...';
        const pre = new Image();
        pre.onload = finish;
        pre.onerror = () => { if (myID === requestID) loader.querySelector('span').textContent = '加载失败'; };
        pre.src = url;
    }
}

/**
 * 叠加层滑动翻页动画
 * 使用两个图片层：base-wrapper（底层，当前页）和 overlay-wrapper（顶层，用于动画）
 *
 * 向前翻页（next）：
 *   1. 将新页加载到 overlay 层
 *   2. overlay 从右侧(translateX(100%))滑入覆盖 base
 *   3. 动画结束后将 overlay 的 src 复制到 base，隐藏 overlay
 *
 * 向后翻页（prev）：
 *   1. 将当前页复制到 overlay 层（覆盖在 base 之上）
 *   2. 将上一页加载到 base 层（被 overlay 遮挡，不可见）
 *   3. overlay 向右滑出(translateX(100%))，露出 base 中的上一页
 *   4. 动画结束后隐藏 overlay
 *
 * 旧页面在加载和动画期间始终保持可见，避免闪烁
 */
function slidePage(url, direction, myID, isPreloaded) {
    const base = $('page-base');
    const overlay = $('page-overlay');
    const ow = $('overlay-wrapper');
    const loader = $('page-loader');

    if (direction === 'next') {
        // === 向前翻页：新页从右侧滑入覆盖旧页 ===
        const startSlide = () => {
            if (myID !== requestID) return;
            preloadedPages.add(url);
            loader.classList.add('hidden');
            overlay.src = url;
            animatePageWrapper(ow, 'translateX(100%)', 'translateX(0)', myID);
            // 动画结束后：将新页 src 复制到 base，隐藏 overlay
            setTimeout(() => {
                if (myID !== requestID) return;
                base.src = url;
                ow.style.display = 'none';
                ow.style.transition = '';
                ow.style.transform = '';
            }, PAGE_SLIDE_DURATION + 20);
        };
        if (isPreloaded) { startSlide(); }
        else {
            loader.classList.remove('hidden');
            loader.querySelector('span').textContent = '渲染中...';
            const pre = new Image();
            pre.onload = startSlide;
            pre.onerror = () => { if (myID === requestID) loader.querySelector('span').textContent = '加载失败'; };
            pre.src = url;
        }
    } else {
        // === 向后翻页：当前页向右滑出，露出上一页 ===
        overlay.src = base.src; // 将当前页复制到叠加层

        const startSlide = () => {
            if (myID !== requestID) return;
            preloadedPages.add(url);
            loader.classList.add('hidden');
            base.src = url; // 将上一页加载到底层（被 overlay 遮挡，不可见）
            animatePageWrapper(ow, 'translateX(0)', 'translateX(100%)', myID);
            // 动画结束后隐藏叠加层
            setTimeout(() => {
                if (myID !== requestID) return;
                ow.style.display = 'none';
                ow.style.transition = '';
                ow.style.transform = '';
            }, PAGE_SLIDE_DURATION + 20);
        };
        if (isPreloaded) { startSlide(); }
        else {
            loader.classList.remove('hidden');
            loader.querySelector('span').textContent = '渲染中...';
            const pre = new Image();
            pre.onload = startSlide;
            pre.onerror = () => { if (myID === requestID) loader.querySelector('span').textContent = '加载失败'; };
            pre.src = url;
        }
    }
}

// ===== 跳转历史 =====

/**
 * 记录一次页码跳转
 * @param {number} fromPage - 跳转前的页码
 * @param {number} toPage - 跳转后的页码
 * 设置 jumpBack = { target: fromPage, anchor: toPage }，清除 jumpForward
 */
function recordJump(fromPage, toPage) {
    if (fromPage === toPage) return;
    jumpBack = { target: fromPage, anchor: toPage };
    jumpForward = null;
    updateJumpButtons();
}

/**
 * 检查跳转历史是否在有效范围内
 * 当当前页与锚点页相差超过 10 页时，清除对应方向的跳转记录
 * 仅在顺序翻页（next/prev）时调用
 */
function checkJumpRange() {
    if (jumpBack && Math.abs(currentPage - jumpBack.anchor) > 10) jumpBack = null;
    if (jumpForward && Math.abs(currentPage - jumpForward.anchor) > 10) jumpForward = null;
}

/** 根据跳转历史状态启用/禁用箭头按钮 */
function updateJumpButtons() {
    $('jump-back-btn').disabled = !jumpBack;
    $('jump-forward-btn').disabled = !jumpForward;
}

/** 点击向后跳转箭头：跳转到 jumpBack.target，并将当前页记入 jumpForward */
function clickJumpBack() {
    if (!jumpBack) return;
    jumpForward = { target: currentPage, anchor: jumpBack.target };
    const target = jumpBack.target;
    jumpBack = null;
    loadPage(target, 'fade');
    updateJumpButtons();
}

/** 点击向前跳转箭头：跳转到 jumpForward.target，并将当前页记入 jumpBack */
function clickJumpForward() {
    if (!jumpForward) return;
    jumpBack = { target: currentPage, anchor: jumpForward.target };
    const target = jumpForward.target;
    jumpForward = null;
    loadPage(target, 'fade');
    updateJumpButtons();
}

/**
 * 跳转到指定页码（通过页码输入框或首页按钮触发）
 * 会记录跳转历史，使用 'fade' 方式加载（无滑动动画）
 */
function jumpToPage(page) {
    if (page < 1 || page > pageCount || page === currentPage) return;
    recordJump(currentPage, page);
    loadPage(page, 'fade');
}

// ===== 导航 =====

/** 上一页（顺序导航，使用滑动动画） */
function goPrev() { if (currentPage > 1) loadPage(currentPage - 1, 'prev'); }
/** 下一页（顺序导航，使用滑动动画） */
function goNext() { if (currentPage < pageCount) loadPage(currentPage + 1, 'next'); }

// 返回按钮 -> 导航到 PDF 所在的文件夹
$('back-btn').addEventListener('click', () => {
    location.hash = currentPDFFolder ? '#/folder/' + encodeURIComponent(currentPDFFolder) : '#/';
});
$('prev-btn').addEventListener('click', goPrev);
$('next-btn').addEventListener('click', goNext);
$('jump-back-btn').addEventListener('click', clickJumpBack);
$('jump-forward-btn').addEventListener('click', clickJumpForward);
// 首页按钮 -> 跳转到第1页（计入跳转历史）
$('home-btn').addEventListener('click', () => jumpToPage(1));

// 页码输入框 -> 跳转到输入的页码
$('page-input').addEventListener('change', e => {
    const page = parseInt(e.target.value);
    if (page >= 1 && page <= pageCount) jumpToPage(page);
    else e.target.value = currentPage;
});

// 左右点击区域（仅在未缩放时翻页）
$('tap-left').addEventListener('click', () => { if (zoom === 1) goPrev(); });
$('tap-right').addEventListener('click', () => { if (zoom === 1) goNext(); });

// 键盘导航（桌面端）
// 注意：不处理空格键（避免与输入框空格冲突），在输入框中不触发翻页
document.addEventListener('keydown', e => {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
    if (e.isComposing) return; // IME 组字中不处理快捷键
    if ($('reader').classList.contains('hidden')) return;

    if (e.key === 'ArrowLeft' || e.key === 'PageUp') {
        e.preventDefault();
        goPrev();
    }
    if (e.key === 'ArrowRight' || e.key === 'PageDown') {
        e.preventDefault();
        goNext();
    }

    // 按 \ 键打开/关闭目录侧边栏
    if (e.code === 'Backslash') {
        e.preventDefault();
        if ($('marks-sidebar').classList.contains('open')) closeMarksSidebar();
        else openMarksSidebar();
    }
});

// ===== 滑动手势（仅在未缩放时生效）=====
// 记录触摸起始位置
$('page-area').addEventListener('touchstart', e => {
    if (e.touches.length === 1 && zoom === 1) {
        swipeStartX = e.touches[0].clientX;
        swipeStartY = e.touches[0].clientY;
    }
}, { passive: true });

// 触摸结束时判断是否为有效的滑动手势（水平位移 > 60px 且水平位移 > 垂直位移的 2 倍）
$('page-area').addEventListener('touchend', e => {
    if (zoom !== 1) return;
    if (e.changedTouches.length !== 1) return;
    const dx = e.changedTouches[0].clientX - swipeStartX;
    const dy = e.changedTouches[0].clientY - swipeStartY;
    if (Math.abs(dx) > 60 && Math.abs(dx) > Math.abs(dy) * 2) {
        if (dx > 0) goPrev(); else goNext();
    }
}, { passive: true });

// ===== 缩放系统 =====

/**
 * 计算两指之间的距离（用于 pinch 缩放比例计算）
 * @param {TouchList} touches - 触摸点列表，需要恰好 2 个
 * @returns {number} 两指间距离（像素）
 */
function getTouchDist(touches) {
    const dx = touches[0].clientX - touches[1].clientX;
    const dy = touches[0].clientY - touches[1].clientY;
    return Math.sqrt(dx * dx + dy * dy);
}

/**
 * 将屏幕触摸坐标转换为相对图片左上角的焦点坐标
 * 用于缩放时确定以哪个点为中心进行缩放
 * @returns {{x: number, y: number}} 焦点坐标（相对图片自然位置）
 */
function getFocalPoint(touchX, touchY) {
    const img = $('page-base');
    const area = $('page-area');
    const areaRect = area.getBoundingClientRect();
    // offsetLeft/offsetTop 是图片在 page-area 中的自然位置（不受 transform 影响）
    return {
        x: touchX - areaRect.left - img.offsetLeft,
        y: touchY - areaRect.top - img.offsetTop
    };
}

/** 将当前 zoom/pan 状态应用为 CSS transform */
function applyTransform() {
    // transform-origin: 0 0（在 CSS 中设置），所以变换从图片左上角开始
    // 顺序：先平移再缩放 -> translate(panX, panY) scale(zoom)
    $('page-base').style.transform = `translate(${panX}px, ${panY}px) scale(${zoom})`;
    $('page-area').style.cursor = zoom > 1 ? (mousePanning ? 'grabbing' : 'grab') : '';
}

/** 重置缩放和平移到初始状态（zoom=1, pan=0） */
function resetZoom() {
    if (horizontalZoomFrame !== null) {
        cancelAnimationFrame(horizontalZoomFrame);
        horizontalZoomFrame = null;
    }
    horizontalZoomDelta = 0;
    horizontalZoomFocal = null;
    zoom = 1; panX = 0; panY = 0;
    $('page-base').style.transform = '';
    $('page-base').classList.remove('smooth');
    $('page-overlay').style.transform = '';
    $('page-area').style.cursor = '';
}

/**
 * 以指定焦点为中心进行缩放
 * 核心公式：newPan = focal - (focal - oldPan) * (newZoom / oldZoom)
 * 这个公式保证焦点位置在缩放前后保持不变（跟手）
 * @param {number} focalX - 焦点 X 坐标（相对图片自然位置）
 * @param {number} focalY - 焦点 Y 坐标
 * @param {number} newZoom - 目标缩放倍率（会被 clamp 到 1~4）
 */
function zoomAt(focalX, focalY, newZoom) {
    newZoom = Math.min(4, Math.max(1, newZoom));
    if (newZoom === zoom) return;
    panX = focalX - (focalX - panX) * (newZoom / zoom);
    panY = focalY - (focalY - panY) * (newZoom / zoom);
    zoom = newZoom;
    if (zoom === 1) { panX = 0; panY = 0; } // 缩放到 1x 时自动归位
    applyTransform();
}

/**
 * 带平滑过渡的缩放（用于横向滚轮缩放）
 * 添加 CSS transition 类，200ms 后移除
 */
function smoothZoomAt(focalX, focalY, newZoom) {
    const img = $('page-base');
    img.classList.add('smooth');
    zoomAt(focalX, focalY, newZoom);
    setTimeout(() => img.classList.remove('smooth'), 200);
}

// ===== 触摸事件：双指缩放 + 单指平移 =====

// touchstart：根据触摸点数量初始化缩放或平移
$('page-area').addEventListener('touchstart', e => {
    if (e.touches.length === 2) {
        // 双指触摸 -> 初始化 pinch 缩放
        e.preventDefault();
        const mid = { x: (e.touches[0].clientX + e.touches[1].clientX) / 2, y: (e.touches[0].clientY + e.touches[1].clientY) / 2 };
        const focal = getFocalPoint(mid.x, mid.y);
        pinchStartDist = getTouchDist(e.touches);  // 记录初始两指距离
        pinchStartFocal = focal;                     // 记录初始焦点
        pinchStartZoom = zoom;                       // 记录初始缩放倍率
        pinchStartPanX = panX;                       // 记录初始平移
        pinchStartPanY = panY;
    } else if (e.touches.length === 1) {
        // 单指触摸 -> 初始化平移
        panStartX = e.touches[0].clientX;
        panStartY = e.touches[0].clientY;
        panOrigX = panX; // 记录平移起始时的 pan 值
        panOrigY = panY;
    }
}, { passive: false });

// touchmove：根据触摸点数量执行缩放或平移
$('page-area').addEventListener('touchmove', e => {
    if (e.touches.length === 2 && pinchStartDist > 0) {
        // === 双指缩放 + 双指平移 ===
        e.preventDefault();
        const dist = getTouchDist(e.touches);
        const mid = { x: (e.touches[0].clientX + e.touches[1].clientX) / 2, y: (e.touches[0].clientY + e.touches[1].clientY) / 2 };
        const currFocal = getFocalPoint(mid.x, mid.y);
        // 根据两指距离比计算新缩放倍率
        const newZoom = Math.min(4, Math.max(1, pinchStartZoom * dist / pinchStartDist));
        // 以初始焦点为中心进行缩放（焦点位置保持不变）
        panX = pinchStartFocal.x - (pinchStartFocal.x - pinchStartPanX) * (newZoom / pinchStartZoom);
        panY = pinchStartFocal.y - (pinchStartFocal.y - pinchStartPanY) * (newZoom / pinchStartZoom);
        // 叠加双指平移（两指中点移动量）
        panX += currFocal.x - pinchStartFocal.x;
        panY += currFocal.y - pinchStartFocal.y;
        zoom = newZoom;
        if (zoom === 1) { panX = 0; panY = 0; }
        applyTransform();
    } else if (e.touches.length === 1 && zoom > 1) {
        // === 单指平移（仅在缩放状态下生效）===
        e.preventDefault();
        panX = panOrigX + (e.touches[0].clientX - panStartX);
        panY = panOrigY + (e.touches[0].clientY - panStartY);
        applyTransform();
    }
}, { passive: false });

// touchend：处理从双指到单指的过渡
$('page-area').addEventListener('touchend', e => {
    // 当从双指变为单指时，重置单指平移的起始位置和原始 pan 值
    // 防止松开一指后页面跳偏（因为 panOrigX/Y 还是双指开始前的旧值）
    if (e.touches.length === 1 && pinchStartDist > 0) {
        panStartX = e.touches[0].clientX;
        panStartY = e.touches[0].clientY;
        panOrigX = panX;
        panOrigY = panY;
    }
    if (e.touches.length < 2) pinchStartDist = 0;
}, { passive: true });

// ===== 滚轮事件：横向缩放 + 竖向翻页 + Ctrl缩放 =====
const HORIZONTAL_WHEEL_THRESHOLD = 40;
const HORIZONTAL_WHEEL_ZOOM_STEP = 1.07;
const HORIZONTAL_CONTINUOUS_SPEED = 1.2;
const CTRL_WHEEL_ZOOM_STEP = 0.055;
const VERTICAL_WHEEL_THRESHOLD = 80;
const WHEEL_GESTURE_END_DELAY = 180;
let horizontalWheelDelta = 0;
let horizontalWheelResetTimer = null;
let horizontalZoomDelta = 0;
let horizontalZoomFocal = null;
let horizontalZoomFrame = null;
let verticalWheelDelta = 0;
let verticalWheelHandled = false;
let verticalWheelResetTimer = null;

/** 将不同 deltaMode 的滚轮输入统一换算为像素 */
function normalizeWheelDelta(delta, deltaMode, pageSize) {
    if (deltaMode === WheelEvent.DOM_DELTA_LINE) return delta * 16;
    if (deltaMode === WheelEvent.DOM_DELTA_PAGE) return delta * pageSize;
    return delta;
}

/** 方向改变时丢弃上一方向的累计量，避免左右/上下抵消后误触 */
function accumulateWheelDelta(total, delta) {
    if (total !== 0 && Math.sign(total) !== Math.sign(delta)) total = 0;
    return total + delta;
}

function scheduleHorizontalWheelReset() {
    clearTimeout(horizontalWheelResetTimer);
    horizontalWheelResetTimer = setTimeout(() => { horizontalWheelDelta = 0; }, WHEEL_GESTURE_END_DELAY);
}

function scheduleVerticalWheelReset() {
    clearTimeout(verticalWheelResetTimer);
    verticalWheelResetTimer = setTimeout(() => {
        verticalWheelDelta = 0;
        verticalWheelHandled = false;
    }, WHEEL_GESTURE_END_DELAY);
}

/** 合并同一帧内的横滚轮事件，避免连续重启 CSS transition 造成拖滞 */
function queueHorizontalZoom(delta, focal) {
    horizontalZoomDelta = accumulateWheelDelta(horizontalZoomDelta, delta);
    horizontalZoomFocal = focal;
    if (horizontalZoomFrame !== null) return;

    horizontalZoomFrame = requestAnimationFrame(() => {
        const pendingDelta = horizontalZoomDelta;
        const pendingFocal = horizontalZoomFocal;
        horizontalZoomDelta = 0;
        horizontalZoomFocal = null;
        horizontalZoomFrame = null;
        if (!pendingDelta || !pendingFocal) return;

        const steps = Math.abs(pendingDelta) / HORIZONTAL_WHEEL_THRESHOLD * HORIZONTAL_CONTINUOUS_SPEED;
        const continuousFactor = Math.pow(HORIZONTAL_WHEEL_ZOOM_STEP, steps);
        const factor = pendingDelta > 0 ? continuousFactor : 1 / continuousFactor;
        $('page-base').classList.remove('smooth');
        zoomAt(pendingFocal.x, pendingFocal.y, zoom * factor);
    });
}

$('page-area').addEventListener('wheel', e => {
    const area = $('page-area');
    const deltaX = normalizeWheelDelta(e.deltaX, e.deltaMode, area.clientWidth);
    const deltaY = normalizeWheelDelta(e.deltaY, e.deltaMode, area.clientHeight);

    if (e.ctrlKey && deltaY !== 0) {
        // Ctrl + 竖向滚轮 -> 缩放
        e.preventDefault();
        const focal = getFocalPoint(e.clientX, e.clientY);
        const factor = deltaY > 0 ? 1 - CTRL_WHEEL_ZOOM_STEP : 1 + CTRL_WHEEL_ZOOM_STEP;
        smoothZoomAt(focal.x, focal.y, zoom * factor);
        return;
    }

    // Shift + 竖滚轮也按横向输入处理，兼容没有独立横滚轮的桌面设备。
    const horizontalDelta = deltaX !== 0 ? deltaX : (e.shiftKey ? deltaY : 0);
    const verticalDelta = e.shiftKey && deltaX === 0 ? 0 : deltaY;
    if (horizontalDelta !== 0 && Math.abs(horizontalDelta) > Math.abs(verticalDelta)) {
        e.preventDefault();
        const focal = getFocalPoint(e.clientX, e.clientY);
        let factor;
        if (zoom === 1) {
            horizontalWheelDelta = accumulateWheelDelta(horizontalWheelDelta, horizontalDelta);
            scheduleHorizontalWheelReset();
            if (Math.abs(horizontalWheelDelta) < HORIZONTAL_WHEEL_THRESHOLD) return;
            factor = horizontalWheelDelta > 0 ? HORIZONTAL_WHEEL_ZOOM_STEP : 1 / HORIZONTAL_WHEEL_ZOOM_STEP;
            horizontalWheelDelta = 0;
            smoothZoomAt(focal.x, focal.y, zoom * factor);
        } else {
            // 进入放大状态后取消安全阈值，并在动画帧内合并输入。
            horizontalWheelDelta = 0;
            queueHorizontalZoom(horizontalDelta, focal);
        }
        return;
    }

    if (verticalDelta !== 0) {
        e.preventDefault();
        verticalWheelDelta = accumulateWheelDelta(verticalWheelDelta, verticalDelta);
        scheduleVerticalWheelReset();
        if (verticalWheelHandled || Math.abs(verticalWheelDelta) < VERTICAL_WHEEL_THRESHOLD) return;

        verticalWheelHandled = true;
        verticalWheelDelta = 0;
        // 翻页会重置缩放；在第一页/最后一页没有可翻页时也保持同样的归位行为。
        if (zoom > 1) resetZoom();
        if (verticalDelta > 0) goNext();
        else goPrev();
    }
}, { passive: false });

// ===== 鼠标拖拽平移（缩放状态下）=====
// 桌面端：zoom > 1 时按住左键拖拽平移图片
let mousePanning = false;
$('page-area').addEventListener('mousedown', e => {
    if (e.button !== 0) return;             // 只处理左键
    if (zoom <= 1) return;                  // 仅缩放状态下平移
    if (e.target.closest('.tap-zone') || e.target.closest('button')) return; // 不干扰按钮和点击区
    e.preventDefault();
    mousePanning = true;
    panStartX = e.clientX;
    panStartY = e.clientY;
    panOrigX = panX;
    panOrigY = panY;
    $('page-area').style.cursor = 'grabbing';
});
document.addEventListener('mousemove', e => {
    if (!mousePanning) return;
    panX = panOrigX + (e.clientX - panStartX);
    panY = panOrigY + (e.clientY - panStartY);
    applyTransform();
});
document.addEventListener('mouseup', () => {
    if (mousePanning) {
        mousePanning = false;
        $('page-area').style.cursor = '';
    }
});

// ===== 目录与标记侧边栏 =====

let collapsedMarks = new Set(); // 已折叠的标记ID集合
let collapsedGroups = new Set(); // 已折叠的来源分组（user / outline:来源）
let currentMarks = [];          // 当前标记列表（从后端加载）

/** 打开侧边栏 */
function openMarksSidebar() {
    $('marks-sidebar').classList.remove('hidden');
    $('marks-backdrop').classList.remove('hidden');
    requestAnimationFrame(() => {
        $('marks-sidebar').classList.add('open');
        $('marks-backdrop').classList.add('open');
    });
    loadMarks();
}

/** 关闭侧边栏 */
function closeMarksSidebar() {
    $('marks-sidebar').classList.remove('open');
    $('marks-backdrop').classList.remove('open');
    setTimeout(() => {
        $('marks-sidebar').classList.add('hidden');
        $('marks-backdrop').classList.add('hidden');
    }, 200);
}

/** 从后端加载标记列表 */
async function loadMarks() {
    const list = $('marks-list');
    list.innerHTML = '<div class="marks-empty">加载中...</div>';
    try {
        currentMarks = await apiJSON(`/api/pdfs/${currentPDFKey}/marks`);
        renderMarks(currentMarks || []);
    } catch (e) {
        list.innerHTML = '<div class="marks-empty">加载失败</div>';
    }
}

/**
 * 渲染标记列表为树形结构
 * 根据 parent_id 构建父子关系，按 sort_order 排序
 * 有子项的标记显示展开/折叠箭头，点击箭头切换折叠状态
 * 用户标记可拖拽排序和删除，PDF目录只读
 */
function renderMarks(marks) {
    currentMarks = marks;
    const list = $('marks-list');
    if (marks.length === 0) {
        let emptyText = '暂无目录或标记<br>可在下方添加';
        if (currentOutlineSource === 'goodnotes') {
            emptyText = 'GoodNotes 中暂无目录';
        } else if (currentOutlineSource === 'deep') {
            emptyText = '深度扫描未找到目录';
        } else if (currentOutlineSource === 'pdf') {
            emptyText = '该 PDF 中暂无目录';
        }
        list.innerHTML = `<div class="marks-empty">${emptyText}</div>`;
        return;
    }

    // 构建 parent_id -> children 映射
    const childrenMap = new Map();
    const roots = [];
    for (const m of marks) {
        if (m.parent_id) {
            if (!childrenMap.has(m.parent_id)) childrenMap.set(m.parent_id, []);
            childrenMap.get(m.parent_id).push(m);
        } else {
            roots.push(m);
        }
    }

    // 按来源分组：我的标记 / GoodNotes 目录 / 深度扫描 / PDF 目录。
    // 每组标题可点击折叠，避免目录项和用户标记混在一起难以分辨。
    const outlineRoots = roots.filter(m => m.is_outline);
    const userRoots = roots.filter(m => !m.is_outline);
    const hasOutline = outlineRoots.length > 0;
    const outlineGroupKey = 'outline:' + (currentOutlineSource || 'pdf');
    let outlineLabel = 'PDF 目录';
    if (currentOutlineSource === 'goodnotes') outlineLabel = 'GoodNotes 目录';
    else if (currentOutlineSource === 'deep') outlineLabel = '深度扫描';

    const groups = [];
    if (userRoots.length > 0) groups.push({ key: 'user', label: '我的标记', roots: userRoots });
    if (hasOutline) groups.push({ key: outlineGroupKey, label: outlineLabel, roots: outlineRoots });

    // 统计每组的节点数（含子项），显示在标题右侧
    function countNodes(nodes) {
        let total = 0;
        const stack = [...nodes];
        while (stack.length > 0) {
            const node = stack.pop();
            total++;
            const children = childrenMap.get(node.id) || [];
            for (const child of children) stack.push(child);
        }
        return total;
    }

    let html = '';
    // 递归渲染树节点
    function renderNode(m, level) {
        const children = childrenMap.get(m.id) || [];
        const hasChildren = children.length > 0;
        const isCollapsed = collapsedMarks.has(m.id);
        const indent = level * 20;
        const isSection = m.page_num === 0;

        html += `<div class="mark-item ${isSection ? 'section' : ''}" data-id="${m.id}" data-page="${m.page_num}" data-level="${level}" data-parent="${m.parent_id || 0}" style="padding-left:${8 + indent}px">`;

        // 展开/折叠箭头
        html += `<span class="mark-toggle ${hasChildren ? '' : 'no-children'} ${isCollapsed ? 'collapsed' : ''}" data-toggle="${m.id}">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 9l6 6 6-6"/></svg>
        </span>`;

        // 拖拽手柄（仅用户标记）
        if (!m.is_outline) {
            html += `<span class="mark-drag" data-drag="${m.id}">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="9" cy="6" r="1"/><circle cx="15" cy="6" r="1"/><circle cx="9" cy="12" r="1"/><circle cx="15" cy="12" r="1"/><circle cx="9" cy="18" r="1"/><circle cx="15" cy="18" r="1"/></svg>
            </span>`;
        }

        html += `<span class="mark-title ${isSection ? 'section' : ''}">${escapeHtml(m.title)}</span>`;
        if (m.page_num > 0) html += `<span class="mark-page">${m.page_num}</span>`;
        if (!m.is_outline) {
            html += `<button class="mark-delete" data-id="${m.id}" aria-label="删除"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6L6 18M6 6l12 12"/></svg></button>`;
        }
        html += '</div>';

        // 递归渲染子项（折叠时跳过）
        if (hasChildren && !isCollapsed) {
            for (const child of children) renderNode(child, level + 1);
        }
    }
    for (const group of groups) {
        const collapsed = collapsedGroups.has(group.key);
        html += `<div class="marks-section ${collapsed ? 'collapsed' : ''}" data-group="${group.key}">
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>
            <span>${group.label}</span>
            <span class="marks-section-count">${countNodes(group.roots)}</span>
        </div>`;
        if (!collapsed) {
            for (const m of group.roots) renderNode(m, 0);
        }
    }

    list.innerHTML = html;
    bindMarkEvents(list);
}

/** 为标记列表绑定事件：点击跳转、展开折叠、删除、拖拽 */
function bindMarkEvents(list) {
    // 点击分组标题 -> 折叠/展开该来源下的全部目录
    list.querySelectorAll('.marks-section[data-group]').forEach(el => {
        el.addEventListener('click', () => {
            const group = el.dataset.group;
            if (collapsedGroups.has(group)) collapsedGroups.delete(group);
            else collapsedGroups.add(group);
            renderMarks(currentMarks);
        });
    });

    // 点击标记项 -> 跳转到对应页面
    list.querySelectorAll('.mark-item').forEach(el => {
        el.addEventListener('click', e => {
            if (e.target.closest('.mark-toggle') || e.target.closest('.mark-delete') || e.target.closest('.mark-drag')) return;
            const page = parseInt(el.dataset.page);
            if (page > 0) {
                closeMarksSidebar();
                setTimeout(() => jumpToPage(page), 100);
            }
        });
    });

    // 展开/折叠箭头
    list.querySelectorAll('.mark-toggle[data-toggle]').forEach(el => {
        el.addEventListener('click', e => {
            e.stopPropagation();
            const id = parseInt(el.dataset.toggle);
            if (collapsedMarks.has(id)) {
                collapsedMarks.delete(id);
            } else {
                collapsedMarks.add(id);
            }
            renderMarks(currentMarks); // 重新渲染
        });
    });

    // 删除按钮
    list.querySelectorAll('.mark-delete').forEach(btn => {
        btn.addEventListener('click', e => {
            e.stopPropagation();
            deleteMark(parseInt(btn.dataset.id));
        });
    });

    // 拖拽手柄（Pointer Events，同时支持鼠标和触摸）
    list.querySelectorAll('.mark-drag[data-drag]').forEach(el => {
        el.addEventListener('pointerdown', e => startMarkDrag(e, parseInt(el.dataset.drag)));
    });
}

// ===== 拖拽排序（Pointer Events）=====
let dragData = null;

function startMarkDrag(e, markId) {
    e.preventDefault();
    e.stopPropagation();
    const list = $('marks-list');
    const allItems = [...list.querySelectorAll('.mark-item')];
    const item = allItems.find(el => parseInt(el.dataset.id) === markId);
    if (!item) return;

    const itemLevel = parseInt(item.dataset.level);
    // 找到所有子项（DOM中紧随其后的层级更高的项）
    const itemIndex = allItems.indexOf(item);
    const children = [];
    for (let i = itemIndex + 1; i < allItems.length; i++) {
        if (parseInt(allItems[i].dataset.level) <= itemLevel) break;
        children.push(allItems[i]);
    }

    dragData = { id: markId, element: item, children, startY: e.clientY, moved: false };
    item.setPointerCapture(e.pointerId);
    item.classList.add('dragging');
    children.forEach(c => c.classList.add('dragging'));

    item.addEventListener('pointermove', onMarkDragMove);
    item.addEventListener('pointerup', onMarkDragEnd);
}

function onMarkDragMove(e) {
    if (!dragData) return;
    e.preventDefault();
    if (Math.abs(e.clientY - dragData.startY) < 5) return;
    dragData.moved = true;

    // 找到指针下方的标记项
    const elements = document.elementsFromPoint(e.clientX, e.clientY);
    const target = elements.find(el => el.classList && el.classList.contains('mark-item') && !el.classList.contains('dragging'));

    // 清除之前的拖放指示
    $('marks-list').querySelectorAll('.drag-over').forEach(el => el.classList.remove('drag-over'));
    if (target) target.classList.add('drag-over');
}

function onMarkDragEnd(e) {
    if (!dragData) return;
    e.preventDefault();
    const list = $('marks-list');

    if (dragData.moved) {
        const elements = document.elementsFromPoint(e.clientX, e.clientY);
        const target = elements.find(el => el.classList && el.classList.contains('mark-item') && !el.classList.contains('dragging'));

        if (target && target !== dragData.element) {
            const rect = target.getBoundingClientRect();
            const insertBefore = e.clientY < rect.top + rect.height / 2;

            // 移动拖拽项及其子项到目标位置
            const itemsToMove = [dragData.element, ...dragData.children];
            if (insertBefore) {
                list.insertBefore(dragData.element, target);
            } else {
                target.after(dragData.element);
            }
            let prev = dragData.element;
            for (const child of dragData.children) {
                prev.after(child);
                prev = child;
            }
            // 更新层级（如果放在不同缩进的位置，保持原层级不变）
            saveMarkOrder();
        }
    }

    // 清理
    dragData.element.classList.remove('dragging');
    dragData.children.forEach(c => c.classList.remove('dragging'));
    list.querySelectorAll('.drag-over').forEach(el => el.classList.remove('drag-over'));
    dragData.element.removeEventListener('pointermove', onMarkDragMove);
    dragData.element.removeEventListener('pointerup', onMarkDragEnd);

    // 如果发生了拖拽移动，阻止后续的 click 事件（防止误触发页面跳转）
    if (dragData.moved) {
        const suppress = ev => { ev.preventDefault(); ev.stopPropagation(); };
        dragData.element.addEventListener('click', suppress, { capture: true, once: true });
        dragData.children.forEach(c => c.addEventListener('click', suppress, { capture: true, once: true }));
    }

    dragData = null;
}

/** 收集当前DOM顺序，发送排序更新到后端 */
function saveMarkOrder() {
    const items = [...$('marks-list').querySelectorAll('.mark-item')];
    const order = items.map((el, i) => ({
        id: parseInt(el.dataset.id),
        sort_order: i + 1,
        parent_id: parseInt(el.dataset.parent) || 0
    }));
    api(`/api/pdfs/${currentPDFKey}/marks/reorder`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(order)
    }).catch(() => showToast('排序保存失败'));
}

/** 删除一个用户自定义标记 */
async function deleteMark(id) {
    try {
        await api(`/api/pdfs/${currentPDFKey}/marks/${id}`, { method: 'DELETE' });
        loadMarks();
        showToast('已删除');
    } catch (e) { showToast('删除失败'); }
}

// 侧边栏事件绑定
$('marks-btn').addEventListener('click', openMarksSidebar);
$('marks-close').addEventListener('click', closeMarksSidebar);
$('marks-backdrop').addEventListener('click', closeMarksSidebar);

// 添加标记按钮（后端自动根据标题编号归类）
$('mark-add-btn').addEventListener('click', () => {
    const input = $('mark-title-input');
    const title = input.value.trim();
    if (!title) return;
    api(`/api/pdfs/${currentPDFKey}/marks`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ page: currentPage, title })
    }).then(() => {
        input.value = '';
        loadMarks();
        showToast('已添加');
    }).catch(() => showToast('添加失败'));
});
$('mark-title-input').addEventListener('keydown', e => {
    if (e.key === 'Enter') $('mark-add-btn').click();
});

// ===== 侧栏底部菜单：批量添加目录 / 目录深度扫描 =====

const marksMenu = $('marks-menu');

function closeMarksMenu() {
    marksMenu.classList.add('hidden');
    $('batch-add-btn').setAttribute('aria-expanded', 'false');
}

function openMarksMenu() {
    marksMenu.classList.remove('hidden');
    $('batch-add-btn').setAttribute('aria-expanded', 'true');
}

$('batch-add-btn').addEventListener('click', e => {
    e.stopPropagation();
    if (marksMenu.classList.contains('hidden')) openMarksMenu(); else closeMarksMenu();
});

// 点击菜单以外的区域收起菜单
document.addEventListener('click', e => {
    if (!marksMenu.classList.contains('hidden') && !e.target.closest('.marks-menu-wrap')) {
        closeMarksMenu();
    }
});

// 打开批量添加模态窗口
$('menu-batch-add').addEventListener('click', () => {
    closeMarksMenu();
    $('batch-modal').classList.remove('hidden');
    $('batch-input').focus();
});

/**
 * 切换当前 PDF 的目录深度扫描：开启时会立即重算目录，
 * 即从 .goodnotes 内嵌的 PDF 附件里读取它自带的书签。
 */
async function setDeepScan(enabled) {
    const input = $('deep-scan-input');
    input.disabled = true;
    try {
        const res = await api(`/api/pdfs/${currentPDFKey}/deep-scan`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled })
        });
        const pdf = await res.json();
        currentOutlineSource = pdf.outline_source || '';
        const index = allPDFs.findIndex(p => p.id === pdf.id);
        if (index >= 0) allPDFs[index] = pdf;
        input.checked = pdf.deep_scan !== false;
        await loadMarks();
        showToast(enabled ? '已开启目录深度扫描' : '已关闭目录深度扫描');
    } catch (err) {
        input.checked = !enabled;
        showToast('目录深度扫描设置失败');
    } finally {
        input.disabled = false;
    }
}

$('deep-scan-input').addEventListener('change', e => {
    closeMarksMenu();
    setDeepScan(e.target.checked);
});

// 关闭模态窗口
function closeBatchModal() {
    $('batch-modal').classList.add('hidden');
    $('batch-input').value = '';
}
$('batch-close').addEventListener('click', closeBatchModal);
$('batch-cancel').addEventListener('click', closeBatchModal);

// 确认批量添加：解析文本，发送到后端
$('batch-confirm').addEventListener('click', async () => {
    const text = $('batch-input').value.trim();
    if (!text) return;

    // 解析每行：格式 "标题 -- 页码"，无 "--" 的行视为纯标题（page=0）
    const items = [];
    for (const line of text.split('\n')) {
        const trimmed = line.trim();
        if (!trimmed) continue;
        const sepIdx = trimmed.lastIndexOf(' -- ');
        if (sepIdx > 0) {
            const title = trimmed.slice(0, sepIdx).trim();
            const pageStr = trimmed.slice(sepIdx + 4).trim();
            const page = parseInt(pageStr) || 0;
            if (title) items.push({ title, page });
        } else {
            items.push({ title: trimmed, page: 0 });
        }
    }

    if (items.length === 0) {
        showToast('未解析到有效条目');
        return;
    }

    try {
        const res = await api(`/api/pdfs/${currentPDFKey}/marks/batch`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(items)
        });
        const data = await res.json();
        closeBatchModal();
        loadMarks();
        showToast(`已添加 ${data.count} 个目录项`);
    } catch (e) {
        showToast('批量添加失败');
    }
});

// 按 Esc 关闭模态窗口
$('batch-input').addEventListener('keydown', e => {
    if (e.key === 'Escape') closeBatchModal();
});

// ===== 初始化 =====
loadLibrary();
