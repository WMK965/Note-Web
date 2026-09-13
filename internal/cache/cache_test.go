package cache

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/renderer"
)

// fakeRenderer 用内存内容代替 pdftoppm，便于精确断言哪些页面被替换
type fakeRenderer struct {
	pageCount int
	content   map[int]string
}

func (f *fakeRenderer) GetPDFInfo(string) (*renderer.PDFInfo, error) {
	return &renderer.PDFInfo{PageCount: f.pageCount, Title: "fake"}, nil
}

func (f *fakeRenderer) RenderPage(_ string, pageNum int, outputPrefix string) (string, error) {
	out := outputPrefix + ".jpg"
	if err := os.WriteFile(out, []byte(f.content[pageNum]), 0644); err != nil {
		return "", err
	}
	return out, nil
}

func (f *fakeRenderer) RenderThumbnail(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return f.RenderPage(pdfPath, pageNum, outputPrefix)
}

type env struct {
	dir  string
	db   *db.Database
	cfg  *config.Config
	mgr  *Manager
	fake *fakeRenderer
	id   int64
	key  string
	pdf  string
}

func newEnv(t *testing.T, pageCount int) *env {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.Database.Path = filepath.Join(dir, "note-web.db")
	cfg.PDF.Dirs = []string{filepath.Join(dir, "documents")}
	if err := os.MkdirAll(cfg.PDF.Dirs[0], 0755); err != nil {
		t.Fatalf("mkdir documents: %v", err)
	}

	database, err := db.Init(cfg.Database.Path)
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	fake := &fakeRenderer{pageCount: pageCount, content: map[int]string{}}
	for p := 1; p <= pageCount; p++ {
		fake.content[p] = "page-" + strconv.Itoa(p)
	}

	mgr := New(database, fake, cfg)

	pdfPath := filepath.Join(cfg.PDF.Dirs[0], "doc.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 minimal"), 0644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	stat, err := os.Stat(pdfPath)
	if err != nil {
		t.Fatalf("stat pdf: %v", err)
	}
	key := "testkey000000000"
	id, err := database.AddPDF(pdfPath, "Doc", key, pageCount, stat.Size(), stat.ModTime().Unix())
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}

	return &env{dir: dir, db: database, cfg: cfg, mgr: mgr, fake: fake, id: id, key: key, pdf: pdfPath}
}

func (e *env) pageFile(page int) string  { return e.mgr.pagePath(e.key, page) }
func (e *env) thumbFile(page int) string { return e.mgr.thumbPath(e.key, page) }

func (e *env) pin(t *testing.T) {
	t.Helper()
	if err := e.db.PinPDF(e.id); err != nil {
		t.Fatalf("pin: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// 只有内容变化的页面才应被替换，未变化页面的文件必须保持原样
func TestRenderAllPagesReplacesOnlyChangedPages(t *testing.T) {
	e := newEnv(t, 3)
	e.pin(t)

	if err := e.mgr.RenderAllPages(e.id); err != nil {
		t.Fatalf("initial render: %v", err)
	}
	for page := 1; page <= 3; page++ {
		if _, err := os.Stat(e.pageFile(page)); err != nil {
			t.Fatalf("第 %d 页未缓存: %v", page, err)
		}
	}

	// 固定第 1、3 页的修改时间：若它们被重新替换，时间戳会变化
	pinned := time.Unix(1000, 0)
	for _, page := range []int{1, 3} {
		if err := os.Chtimes(e.pageFile(page), pinned, pinned); err != nil {
			t.Fatalf("chtimes page %d: %v", page, err)
		}
	}
	// 预置缩略图，用于验证变化页面的缩略图会被丢弃
	for _, page := range []int{1, 2} {
		if err := os.WriteFile(e.thumbFile(page), []byte("thumb"), 0644); err != nil {
			t.Fatalf("write thumb %d: %v", page, err)
		}
	}

	// 仅第 2 页内容发生变化
	e.fake.content[2] = "page-2-CHANGED"
	if err := e.mgr.RenderAllPages(e.id); err != nil {
		t.Fatalf("second render: %v", err)
	}

	for _, page := range []int{1, 3} {
		info, err := os.Stat(e.pageFile(page))
		if err != nil {
			t.Fatalf("stat page %d: %v", page, err)
		}
		if !info.ModTime().Equal(pinned) {
			t.Errorf("第 %d 页内容未变化，不应被替换（mtime 变了）", page)
		}
	}
	if got := readFile(t, e.pageFile(2)); got != "page-2-CHANGED" {
		t.Errorf("第 2 页内容 = %q, 期望新内容", got)
	}

	// 变化页面的缩略图必须失效，未变化页面的缩略图保留
	if _, err := os.Stat(e.thumbFile(2)); !os.IsNotExist(err) {
		t.Error("变化页面的缩略图应被删除")
	}
	if _, err := os.Stat(e.thumbFile(1)); err != nil {
		t.Errorf("未变化页面的缩略图不应被删除: %v", err)
	}

	// 只报告真正变化的页码
	status, err := e.db.GetCacheStatus(e.id)
	if err != nil {
		t.Fatalf("cache status: %v", err)
	}
	if len(status.ChangedPages) != 1 || status.ChangedPages[0] != 2 {
		t.Errorf("changed_pages = %v, 期望 [2]", status.ChangedPages)
	}
	if status.Version != 1 {
		t.Errorf("cache_version = %d, 期望 1", status.Version)
	}
	if status.Cached != 3 {
		t.Errorf("cached_pages = %d, 期望 3", status.Cached)
	}

	// 临时目录必须清理干净
	if _, err := os.Stat(filepath.Join(e.cfg.CacheDir, e.key+".tmp")); !os.IsNotExist(err) {
		t.Error("临时渲染目录未被清理")
	}
}

// 文档页数变少时，多余的缓存页面必须删除并计入变化页码
func TestRenderAllPagesRemovesStalePages(t *testing.T) {
	e := newEnv(t, 3)
	e.pin(t)
	if err := e.mgr.RenderAllPages(e.id); err != nil {
		t.Fatalf("initial render: %v", err)
	}

	// 文档被替换为 2 页
	e.fake.pageCount = 2
	if err := e.db.UpdatePDFMetadata(e.id, "Doc", 2, 10, time.Now().Unix()); err != nil {
		t.Fatalf("update metadata: %v", err)
	}
	if err := e.mgr.RenderAllPages(e.id); err != nil {
		t.Fatalf("second render: %v", err)
	}

	if _, err := os.Stat(e.pageFile(3)); !os.IsNotExist(err) {
		t.Error("超出页数的缓存页面应被删除")
	}
	status, err := e.db.GetCacheStatus(e.id)
	if err != nil {
		t.Fatalf("cache status: %v", err)
	}
	if status.Cached != 2 {
		t.Errorf("cached_pages = %d, 期望 2", status.Cached)
	}
	if len(status.ChangedPages) == 0 || status.ChangedPages[len(status.ChangedPages)-1] != 3 {
		t.Errorf("changed_pages = %v, 期望包含第 3 页", status.ChangedPages)
	}
}

// 未标记缓存的文档被更新：缓存立即失效并递增版本，避免用户手动清理
func TestInvalidatePDFClearsCacheAndBumpsVersion(t *testing.T) {
	e := newEnv(t, 2)
	if err := e.mgr.RenderAllPages(e.id); err != nil {
		t.Fatalf("render: %v", err)
	}
	before, err := e.db.GetCacheStatus(e.id)
	if err != nil {
		t.Fatalf("cache status: %v", err)
	}

	if err := e.mgr.InvalidatePDF(e.id); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	if _, err := os.Stat(e.pageFile(1)); !os.IsNotExist(err) {
		t.Error("失效后页面缓存文件应被删除")
	}
	after, err := e.db.GetCacheStatus(e.id)
	if err != nil {
		t.Fatalf("cache status: %v", err)
	}
	if after.Version != before.Version+1 {
		t.Errorf("cache_version = %d, 期望 %d", after.Version, before.Version+1)
	}
	if after.Cached != 0 {
		t.Errorf("cached_pages = %d, 期望 0", after.Cached)
	}
	if len(after.ChangedPages) != 0 {
		t.Errorf("被动缓存整份失效时 changed_pages 应为空（范围未知），实际 %v", after.ChangedPages)
	}
}

// 请求页面时若发现磁盘文件已更新（未标记缓存的场景），应立即重新渲染该页
func TestGetPageRefreshesChangedUnpinnedPDF(t *testing.T) {
	e := newEnv(t, 2)

	// 首次访问：按需渲染第 1 页
	if _, err := e.mgr.GetPage(e.id, 1); err != nil {
		t.Fatalf("get page: %v", err)
	}
	if got := readFile(t, e.pageFile(1)); got != "page-1" {
		t.Fatalf("初始内容 = %q", got)
	}

	// 磁盘上的 PDF 被笔记软件更新
	later := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(e.pdf, []byte("%PDF-1.4 updated and larger"), 0644); err != nil {
		t.Fatalf("update pdf: %v", err)
	}
	if err := os.Chtimes(e.pdf, later, later); err != nil {
		t.Fatalf("chtimes pdf: %v", err)
	}
	e.fake.content[1] = "page-1-UPDATED"

	if _, err := e.mgr.GetPage(e.id, 1); err != nil {
		t.Fatalf("get page after update: %v", err)
	}
	if got := readFile(t, e.pageFile(1)); got != "page-1-UPDATED" {
		t.Errorf("更新后内容 = %q, 期望重新渲染的新内容", got)
	}
}
