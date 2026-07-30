const $ = id => document.getElementById(id);

// ===== State =====
let allPDFs = [];
let currentFolder = '';
let isSearchMode = false;

let currentPDF = null;
let currentPDFKey = null;
let currentPDFFolder = '';
let currentPage = 1;
let pageCount = 0;
let requestID = 0;

// Zoom state
let zoom = 1;
let panX = 0;
let panY = 0;
let lastTouchDist = 0;
let touchStartX = 0;
let touchStartY = 0;
let lastTapTime = 0;

// ===== Icons =====
const ICON_FOLDER = '<svg class="folder-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>';
const ICON_FILE = '<svg class="pdf-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>';
const ICON_BOOKMARK = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/></svg>';
const ICON_BOOKMARK_FILL = '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/></svg>';

// ===== Utilities =====
function escapeHtml(s) {
    const div = document.createElement('div');
    div.textContent = s;
    return div.innerHTML;
}

function formatSize(bytes) {
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + ' KB';
    return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

function showToast(msg, duration = 2000) {
    const t = $('toast');
    t.textContent = msg;
    t.classList.remove('hidden');
    clearTimeout(t._timer);
    t._timer = setTimeout(() => t.classList.add('hidden'), duration);
}

async function api(path, opts) {
    const res = await fetch(path, opts);
    if (!res.ok) {
        const err = await res.json().catch(() => ({ error: res.statusText }));
        throw new Error(err.error || 'Request failed');
    }
    return res;
}

async function apiJSON(path) {
    const res = await fetch(path);
    if (!res.ok) throw new Error('Request failed');
    return res.json();
}

// ===== Routing =====
function handleRoute() {
    const hash = location.hash.slice(1) || '/';
    const parts = hash.split('/').filter(Boolean);

    if (parts[0] === 'read' && parts[1]) {
        const key = parts[1];
        const page = parts[2] ? parseInt(parts[2]) : null;
        showReader(key, page);
    } else if (parts[0] === 'folder') {
        const folder = parts.slice(1).map(decodeURIComponent).join('/');
        showFolder(folder);
    } else {
        showFolder('');
    }
}

window.addEventListener('hashchange', handleRoute);

// ===== Library / Folder View =====
async function loadLibrary() {
    try {
        allPDFs = await apiJSON('/api/pdfs');
        handleRoute();
    } catch (e) {
        showToast('加载失败');
    }
}

function showFolder(folder) {
    currentFolder = folder;
    $('library').classList.remove('hidden');
    $('reader').classList.add('hidden');
    $('search').value = '';
    isSearchMode = false;
    renderBreadcrumb(folder);
    renderFolder(folder);
}

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

function getFolderContents(folder) {
    const prefix = folder ? folder + '/' : '';
    const folders = new Map();
    const pdfs = [];

    for (const pdf of allPDFs) {
        const rp = pdf.relative_path;
        if (!rp.startsWith(prefix)) continue;
        const remaining = rp.slice(prefix.length);
        const slashIdx = remaining.indexOf('/');

        if (slashIdx === -1) {
            pdfs.push(pdf);
        } else {
            const name = remaining.slice(0, slashIdx);
            if (!folders.has(name)) {
                folders.set(name, 0);
            }
            folders.set(name, folders.get(name) + 1);
        }
    }

    return {
        folders: [...folders.entries()].sort((a, b) => a[0].localeCompare(b[0], 'zh')),
        pdfs: pdfs.sort((a, b) => a.title.localeCompare(b.title, 'zh'))
    };
}

function renderFolder(folder) {
    const list = $('pdf-list');
    const { folders, pdfs } = getFolderContents(folder);

    if (folders.length === 0 && pdfs.length === 0) {
        list.innerHTML = '<div style="text-align:center;padding:40px;color:var(--text-muted)">暂无文件</div>';
        return;
    }

    let html = '';
    for (const [name, count] of folders) {
        const path = folder ? folder + '/' + name : name;
        html += `<div class="folder-item" data-folder="${escapeHtml(path)}">
            ${ICON_FOLDER}
            <span class="folder-name">${escapeHtml(name)}</span>
            <span class="folder-count">${count} 个文件</span>
        </div>`;
    }

    for (const pdf of pdfs) {
        let cacheBar = '';
        if (pdf.pinned && pdf.page_count > 0) {
            const pct = (pdf.cached_pages / pdf.page_count * 100).toFixed(0);
            cacheBar = `<div class="pdf-cache-bar"><div class="pdf-cache-fill" style="width:${pct}%"></div></div>`;
        }
        const metaParts = [`${pdf.page_count} 页`, formatSize(pdf.file_size)];
        if (pdf.pinned) metaParts.push(`已缓存 ${pdf.cached_pages}/${pdf.page_count}`);

        html += `<div class="pdf-item" data-key="${pdf.cache_key}" data-folder="${escapeHtml(folder)}">
            ${ICON_FILE}
            <div class="pdf-info">
                <div class="pdf-title">${escapeHtml(pdf.title)}</div>
                <div class="pdf-meta">${metaParts.join(' · ')}</div>
                ${cacheBar}
            </div>
            <button class="pin-btn ${pdf.pinned ? 'pinned' : ''}" data-pin="${pdf.cache_key}">
                ${pdf.pinned ? ICON_BOOKMARK_FILL : ICON_BOOKMARK}
            </button>
        </div>`;
    }

    list.innerHTML = html;

    list.querySelectorAll('.folder-item').forEach(el => {
        el.addEventListener('click', () => {
            location.hash = '#/folder/' + encodeURIComponent(el.dataset.folder);
        });
    });

    list.querySelectorAll('.pdf-item').forEach(el => {
        el.addEventListener('click', e => {
            if (e.target.closest('.pin-btn')) return;
            location.hash = '#/read/' + el.dataset.key;
        });
    });

    list.querySelectorAll('.pin-btn').forEach(btn => {
        btn.addEventListener('click', e => {
            e.stopPropagation();
            const key = btn.dataset.pin;
            const pdf = allPDFs.find(p => p.cache_key === key);
            togglePin(key, btn, pdf);
        });
    });
}

// ===== Search =====
$('search').addEventListener('input', e => {
    const q = e.target.value.toLowerCase().trim();
    if (!q) {
        isSearchMode = false;
        renderFolder(currentFolder);
        return;
    }
    isSearchMode = true;
    const filtered = allPDFs.filter(p =>
        p.title.toLowerCase().includes(q) ||
        p.relative_path.toLowerCase().includes(q)
    );
    renderSearchResults(filtered);
});

function renderSearchResults(pdfs) {
    const list = $('pdf-list');
    if (pdfs.length === 0) {
        list.innerHTML = '<div style="text-align:center;padding:40px;color:var(--text-muted)">未找到匹配文件</div>';
        return;
    }

    let html = '';
    for (const pdf of pdfs) {
        const dir = pdf.relative_path.includes('/')
            ? pdf.relative_path.slice(0, pdf.relative_path.lastIndexOf('/'))
            : '';
        let cacheBar = '';
        if (pdf.pinned && pdf.page_count > 0) {
            const pct = (pdf.cached_pages / pdf.page_count * 100).toFixed(0);
            cacheBar = `<div class="pdf-cache-bar"><div class="pdf-cache-fill" style="width:${pct}%"></div></div>`;
        }
        const metaParts = [`${pdf.page_count} 页`, formatSize(pdf.file_size)];
        if (dir) metaParts.push(dir);

        html += `<div class="pdf-item" data-key="${pdf.cache_key}" data-folder="${escapeHtml(dir)}">
            ${ICON_FILE}
            <div class="pdf-info">
                <div class="pdf-title">${escapeHtml(pdf.title)}</div>
                <div class="pdf-meta">${metaParts.join(' · ')}</div>
                ${cacheBar}
            </div>
            <button class="pin-btn ${pdf.pinned ? 'pinned' : ''}" data-pin="${pdf.cache_key}">
                ${pdf.pinned ? ICON_BOOKMARK_FILL : ICON_BOOKMARK}
            </button>
        </div>`;
    }

    list.innerHTML = html;

    list.querySelectorAll('.pdf-item').forEach(el => {
        el.addEventListener('click', e => {
            if (e.target.closest('.pin-btn')) return;
            location.hash = '#/read/' + el.dataset.key;
        });
    });

    list.querySelectorAll('.pin-btn').forEach(btn => {
        btn.addEventListener('click', e => {
            e.stopPropagation();
            const key = btn.dataset.pin;
            const pdf = allPDFs.find(p => p.cache_key === key);
            togglePin(key, btn, pdf);
        });
    });
}

// ===== Pin / Unpin =====
async function togglePin(key, btn, pdf) {
    try {
        if (pdf.pinned) {
            await api(`/api/pdfs/${key}/pin`, { method: 'DELETE' });
            pdf.pinned = false;
            pdf.cached_pages = 0;
            pdf.cache_active = false;
            btn.classList.remove('pinned');
            btn.innerHTML = ICON_BOOKMARK;
            showToast('已取消缓存');
            if (isSearchMode) renderSearchResults(allPDFs.filter(p =>
                p.title.toLowerCase().includes($('search').value.toLowerCase().trim())));
            else renderFolder(currentFolder);
        } else {
            await api(`/api/pdfs/${key}/pin`, { method: 'PUT' });
            pdf.pinned = true;
            btn.classList.add('pinned');
            btn.innerHTML = ICON_BOOKMARK_FILL;
            showToast('已标记缓存，后台渲染中...');
            pollCacheStatus(key, pdf);
        }
    } catch (e) {
        showToast('操作失败: ' + e.message);
    }
}

async function pollCacheStatus(key, pdf) {
    const interval = setInterval(async () => {
        try {
            const status = await apiJSON(`/api/pdfs/${key}/cache`);
            if (!status.pinned) { clearInterval(interval); return; }
            pdf.cached_pages = status.cached;
            if (status.cached >= status.total && !status.rebuilding) {
                clearInterval(interval);
            }
            if (!isSearchMode) renderFolder(currentFolder);
        } catch (e) { }
    }, 3000);
}

// ===== Reader =====
function showReader(key, page) {
    const pdf = allPDFs.find(p => p.cache_key === key);
    if (!pdf) {
        showToast('PDF not found');
        location.hash = '#/';
        return;
    }

    currentPDF = pdf.id;
    currentPDFKey = key;
    currentPDFFolder = pdf.relative_path.includes('/')
        ? pdf.relative_path.slice(0, pdf.relative_path.lastIndexOf('/'))
        : '';
    pageCount = pdf.page_count;

    const savedPage = parseInt(localStorage.getItem(`pdf-${key}-page`)) || 1;
    currentPage = page || savedPage;
    if (currentPage > pageCount) currentPage = 1;

    $('library').classList.add('hidden');
    $('reader').classList.remove('hidden');
    $('reader-title').textContent = pdf.title;
    $('page-total').textContent = pageCount;
    $('page-input').max = pageCount;

    updatePinButton(pdf);
    resetZoom();
    loadPage(currentPage, 'fade');
}

function updatePinButton(pdf) {
    const btn = $('reader-pin');
    btn.className = 'icon-btn pin-btn ' + (pdf.pinned ? 'pinned' : '');
    btn.innerHTML = pdf.pinned ? ICON_BOOKMARK_FILL : ICON_BOOKMARK;
    btn.onclick = () => togglePin(currentPDFKey, btn, pdf);
}

// ===== Smart Page Loading =====
function loadPage(page, direction) {
    if (page < 1 || page > pageCount) return;

    const myID = ++requestID;
    currentPage = page;
    $('page-input').value = page;
    $('prev-btn').disabled = page <= 1;
    $('next-btn').disabled = page >= pageCount;

    localStorage.setItem(`pdf-${currentPDFKey}-page`, page);
    history.replaceState(null, '', `#/read/${currentPDFKey}/${page}`);

    const img = $('page-img');
    const loader = $('page-loader');

    loader.classList.remove('hidden');
    img.style.opacity = '0';

    img.onload = () => {
        if (myID !== requestID) return;
        loader.classList.add('hidden');
        img.classList.remove('enter', 'enter-left', 'enter-right');
        void img.offsetWidth;
        if (direction === 'next') img.classList.add('enter-right');
        else if (direction === 'prev') img.classList.add('enter-left');
        else img.classList.add('enter');
        img.style.opacity = '';
    };
    img.onerror = () => {
        if (myID !== requestID) return;
        loader.querySelector('span').textContent = '加载失败，点击重试';
    };

    img.src = `/api/pdfs/${currentPDFKey}/page/${page}?t=${Date.now()}`;

    setTimeout(() => {
        if (myID !== requestID) return;
        if (page < pageCount) {
            new Image().src = `/api/pdfs/${currentPDFKey}/page/${page + 1}`;
        }
        if (page > 1) {
            new Image().src = `/api/pdfs/${currentPDFKey}/page/${page - 1}`;
        }
    }, 200);
}

function goPrev() { if (currentPage > 1) loadPage(currentPage - 1, 'prev'); }
function goNext() { if (currentPage < pageCount) loadPage(currentPage + 1, 'next'); }

// ===== Navigation =====
$('back-btn').addEventListener('click', () => {
    location.hash = currentPDFFolder ? '#/folder/' + encodeURIComponent(currentPDFFolder) : '#/';
});

$('prev-btn').addEventListener('click', goPrev);
$('next-btn').addEventListener('click', goNext);

$('page-input').addEventListener('change', e => {
    const page = parseInt(e.target.value);
    if (page >= 1 && page <= pageCount) {
        loadPage(page, 'fade');
    } else {
        e.target.value = currentPage;
    }
});

$('tap-left').addEventListener('click', () => { if (zoom === 1) goPrev(); });
$('tap-right').addEventListener('click', () => { if (zoom === 1) goNext(); });

document.addEventListener('keydown', e => {
    if ($('reader').classList.contains('hidden')) return;
    if (e.key === 'ArrowLeft') goPrev();
    if (e.key === 'ArrowRight' || e.key === ' ') { e.preventDefault(); goNext(); }
});

// ===== Swipe (only when not zoomed) =====
let swipeStartX = 0, swipeStartY = 0;
$('page-area').addEventListener('touchstart', e => {
    if (e.touches.length === 1 && zoom === 1) {
        swipeStartX = e.touches[0].clientX;
        swipeStartY = e.touches[0].clientY;
    }
}, { passive: true });

$('page-area').addEventListener('touchend', e => {
    if (zoom !== 1) return;
    const dx = e.changedTouches[0].clientX - swipeStartX;
    const dy = e.changedTouches[0].clientY - swipeStartY;
    if (Math.abs(dx) > 60 && Math.abs(dx) > Math.abs(dy) * 2) {
        if (dx > 0) goPrev();
        else goNext();
    }
}, { passive: true });

// ===== Pinch-to-Zoom =====
function getTouchDist(touches) {
    const dx = touches[0].clientX - touches[1].clientX;
    const dy = touches[0].clientY - touches[1].clientY;
    return Math.sqrt(dx * dx + dy * dy);
}

function applyZoom() {
    const img = $('page-img');
    img.style.transform = `scale(${zoom}) translate(${panX / zoom}px, ${panY / zoom}px)`;
    $('page-area').style.overflow = zoom > 1 ? 'auto' : 'hidden';
}

function resetZoom() {
    zoom = 1; panX = 0; panY = 0;
    $('page-img').style.transform = '';
    $('page-area').style.overflow = 'hidden';
}

$('page-area').addEventListener('touchstart', e => {
    const now = Date.now();
    if (e.touches.length === 1) {
        if (now - lastTapTime < 300) {
            zoom = zoom > 1 ? 1 : 2;
            panX = 0; panY = 0;
            applyZoom();
            if (zoom === 1) resetZoom();
        }
        lastTapTime = now;
    }
    if (e.touches.length === 2) {
        lastTouchDist = getTouchDist(e.touches);
        e.preventDefault();
    }
}, { passive: false });

$('page-area').addEventListener('touchmove', e => {
    if (e.touches.length === 2) {
        e.preventDefault();
        const dist = getTouchDist(e.touches);
        if (lastTouchDist > 0) {
            const ratio = dist / lastTouchDist;
            zoom = Math.min(4, Math.max(1, zoom * ratio));
            if (zoom === 1) { panX = 0; panY = 0; }
            applyZoom();
        }
        lastTouchDist = dist;
    } else if (e.touches.length === 1 && zoom > 1) {
        e.preventDefault();
        const dx = e.touches[0].clientX - touchStartX;
        const dy = e.touches[0].clientY - touchStartY;
        panX += dx;
        panY += dy;
        touchStartX = e.touches[0].clientX;
        touchStartY = e.touches[0].clientY;
        applyZoom();
    } else if (e.touches.length === 1 && zoom === 1) {
        touchStartX = e.touches[0].clientX;
        touchStartY = e.touches[0].clientY;
    }
}, { passive: false });

$('page-area').addEventListener('touchend', e => {
    if (e.touches.length < 2) lastTouchDist = 0;
}, { passive: true });

// ===== Init =====
loadLibrary();
