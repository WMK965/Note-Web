package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"note-web/internal/cache"
	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/renderer"
	"note-web/internal/util"
)

type testEnv struct {
	server *httptest.Server
	api    *Server
	db     *db.Database
	cfg    *config.Config
	client *http.Client
}

// newTestEnv 启动一个使用临时目录的真实 API 服务器（不依赖 pdftoppm）
func newTestEnv(t *testing.T, basePath string) *testEnv {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.Proxy.BasePath = basePath
	cfg.Database.Path = filepath.Join(dir, "note-web.db")
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.PDF.Dirs = []string{filepath.Join(dir, "documents")}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	if err := os.MkdirAll(cfg.PDF.Dirs[0], 0755); err != nil {
		t.Fatalf("mkdir documents: %v", err)
	}

	database, err := db.Init(cfg.Database.Path)
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	r := renderer.New(cfg.Render)
	cm := cache.New(database, r, cfg)
	t.Cleanup(cm.Stop)

	webRoot := os.DirFS(filepath.Join("..", "..", "web"))
	apiServer := New(database, cm, r, cfg, webRoot)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	return &testEnv{
		server: srv,
		api:    apiServer,
		db:     database,
		cfg:    cfg,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 不自动跟随重定向，便于断言
		}},
	}
}

func (e *testEnv) get(t *testing.T, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.server.URL+path, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s: %v", path, err)
	}
	return resp, string(body)
}

// addCachedPDF 写入一条 PDF 记录（不渲染），返回 cache_key
func (e *testEnv) addCachedPDF(t *testing.T, name string, pageCount int) (string, int64) {
	t.Helper()
	pdfPath := filepath.Join(e.cfg.PDF.Dirs[0], name)
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 minimal"), 0644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	stat, err := os.Stat(pdfPath)
	if err != nil {
		t.Fatalf("stat pdf: %v", err)
	}
	key := util.CacheKey(pdfPath)
	id, err := e.db.AddPDF(pdfPath, name, key, pageCount, stat.Size(), stat.ModTime().Unix())
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	return key, id
}

func (e *testEnv) writePage(t *testing.T, key string, page int, content string) string {
	t.Helper()
	path := filepath.Join(e.cfg.CacheDir, key, "p0001.jpg")
	if page != 1 {
		t.Fatalf("test helper only supports page 1")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write page: %v", err)
	}
	return path
}

// 代理前缀下必须同时提供前端页面和 /api 接口，且根路径不受影响
func TestProxyBasePathServesAppAndApi(t *testing.T) {
	env := newTestEnv(t, "/proxy-index")

	cases := []struct {
		path       string
		wantStatus int
		wantSubstr string
		wantCType  string
	}{
		{path: "/", wantStatus: 200, wantSubstr: "Note-Web"},
		{path: "/proxy-index/", wantStatus: 200, wantSubstr: "Note-Web"},
		{path: "/proxy-index/style.css", wantStatus: 200, wantCType: "text/css"},
		{path: "/proxy-index/app.js", wantStatus: 200, wantCType: "text/javascript"},
		{path: "/proxy-index/api/pdfs", wantStatus: 200, wantSubstr: "[]"},
		{path: "/api/pdfs", wantStatus: 200, wantSubstr: "[]"},
	}
	for _, tc := range cases {
		resp, body := env.get(t, tc.path, nil)
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("GET %s: status = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
		}
		if tc.wantSubstr != "" && !strings.Contains(body, tc.wantSubstr) {
			t.Errorf("GET %s: body does not contain %q", tc.path, tc.wantSubstr)
		}
		if tc.wantCType != "" && !strings.HasPrefix(resp.Header.Get("Content-Type"), tc.wantCType) {
			t.Errorf("GET %s: content-type = %q, want %q", tc.path, resp.Header.Get("Content-Type"), tc.wantCType)
		}
	}

	// 不带斜杠的前缀重定向到带斜杠形式
	resp, _ := env.get(t, "/proxy-index", nil)
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("GET /proxy-index: status = %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/proxy-index/" {
		t.Errorf("GET /proxy-index: Location = %q, want /proxy-index/", loc)
	}

	// 静态资源必须以相对路径引用，否则代理前缀下会 404
	_, body := env.get(t, "/", nil)
	if strings.Contains(body, `src="/app.js"`) || strings.Contains(body, `href="/style.css"`) {
		t.Error("index.html 使用了根路径绝对地址，代理前缀下会加载失败")
	}
}

// 未配置代理前缀时，同名路径应保持 404，避免意外暴露入口
func TestProxyBasePathDisabledByDefault(t *testing.T) {
	env := newTestEnv(t, "")

	if resp, _ := env.get(t, "/proxy-index/", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /proxy-index/: status = %d, want 404", resp.StatusCode)
	}
	if resp, _ := env.get(t, "/proxy-index/api/pdfs", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /proxy-index/api/pdfs: status = %d, want 404", resp.StatusCode)
	}
	if resp, body := env.get(t, "/api/pdfs", nil); resp.StatusCode != 200 || !strings.Contains(body, "[]") {
		t.Errorf("GET /api/pdfs: status = %d body = %q", resp.StatusCode, body)
	}
}

// 页面图片必须可校验：内容变化后客户端能拿到新图，而不是一直看浏览器里的旧缓存
func TestPageImageRevalidation(t *testing.T) {
	env := newTestEnv(t, "")
	key, _ := env.addCachedPDF(t, "doc.pdf", 1)
	pagePath := env.writePage(t, key, 1, "page-content-v1")

	resp, body := env.get(t, "/api/pdfs/"+key+"/page/1", nil)
	if resp.StatusCode != 200 || body != "page-content-v1" {
		t.Fatalf("first GET: status = %d body = %q", resp.StatusCode, body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("响应缺少 ETag，客户端无法判断页面是否变化")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, no-cache")
	}

	// 未变化的页面：条件请求应返回 304
	resp, _ = env.get(t, "/api/pdfs/"+key+"/page/1", map[string]string{"If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("未变化时 status = %d, want 304", resp.StatusCode)
	}

	// 页面被重新渲染：条件请求必须返回新内容
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(pagePath, []byte("page-content-v2-changed"), 0644); err != nil {
		t.Fatalf("rewrite page: %v", err)
	}
	if err := os.Chtimes(pagePath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	resp, body = env.get(t, "/api/pdfs/"+key+"/page/1", map[string]string{"If-None-Match": etag})
	if resp.StatusCode != 200 || body != "page-content-v2-changed" {
		t.Errorf("变化后 status = %d body = %q, want 200 与新内容", resp.StatusCode, body)
	}
}

func TestPDFResponseReportsGoodNotesOutline(t *testing.T) {
	env := newTestEnv(t, "")
	key, id := env.addCachedPDF(t, "book.pdf", 2)
	if err := env.db.SetOutlineSource(id, filepath.Join(env.cfg.PDF.Dirs[0], "book.goodnotes"), 123, 456, "goodnotes"); err != nil {
		t.Fatal(err)
	}

	resp, body := env.get(t, "/api/pdfs/"+key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"outline_source":"goodnotes"`) {
		t.Fatalf("response does not report GoodNotes outline: %s", body)
	}
}

// 目录来自归档内嵌 PDF 时，接口要如实报告来源，前端才能显示成 PDF 目录
func TestPDFResponseReportsPDFOutlineSource(t *testing.T) {
	env := newTestEnv(t, "")
	key, id := env.addCachedPDF(t, "book.pdf", 2)
	if err := env.db.SetOutlineSource(id, filepath.Join(env.cfg.PDF.Dirs[0], "book.goodnotes"), 123, 456, "pdf"); err != nil {
		t.Fatal(err)
	}

	resp, body := env.get(t, "/api/pdfs/"+key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"outline_source":"pdf"`) {
		t.Fatalf("response does not report PDF outline source: %s", body)
	}
}

// fakeRefresher 记录接口层是否触发了目录重算
type fakeRefresher struct {
	ids []int64
	err error
}

func (f *fakeRefresher) RefreshOutline(pdfID int64) error {
	f.ids = append(f.ids, pdfID)
	return f.err
}

// 深度扫描开关：写入偏好、立即重算目录，并把最新状态返回给前端
func TestSetDeepScanEndpoint(t *testing.T) {
	env := newTestEnv(t, "")
	key, id := env.addCachedPDF(t, "book.pdf", 2)
	refresher := &fakeRefresher{}
	env.api.SetOutlineRefresher(refresher)

	req, err := http.NewRequest(http.MethodPut, env.server.URL+"/api/pdfs/"+key+"/deep-scan",
		strings.NewReader(`{"enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("PUT deep-scan: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"deep_scan":false`) {
		t.Errorf("response does not report the new state: %s", body)
	}

	pdf, err := env.db.GetPDF(id)
	if err != nil {
		t.Fatal(err)
	}
	if pdf.DeepScan != 0 {
		t.Errorf("stored deep scan mode = %d, want 0", pdf.DeepScan)
	}
	if len(refresher.ids) != 1 || refresher.ids[0] != id {
		t.Errorf("refresher calls = %v, want [%d]", refresher.ids, id)
	}

	// 再次开启
	req, err = http.NewRequest(http.MethodPut, env.server.URL+"/api/pdfs/"+key+"/deep-scan",
		strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp2, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("PUT deep-scan (enable): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("enable status = %d", resp2.StatusCode)
	}
	pdf, err = env.db.GetPDF(id)
	if err != nil {
		t.Fatal(err)
	}
	if pdf.DeepScan != 1 {
		t.Errorf("stored deep scan mode = %d, want 1", pdf.DeepScan)
	}
}
