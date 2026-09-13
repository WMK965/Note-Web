// API 服务器包
// 提供 RESTful API 和嵌入式前端静态文件服务
//
// API 端点：
//
//	GET    /api/pdfs                      - 列出所有PDF（支持 ?pinned 过滤）
//	GET    /api/pdfs/{key}                - 获取单个PDF信息
//	GET    /api/pdfs/{key}/page/{n}       - 获取第n页的JPEG图片
//	GET    /api/pdfs/{key}/thumb/{n}      - 获取第n页的缩略图
//	PUT    /api/pdfs/{key}/pin            - 标记为主动缓存（触发全量预渲染）
//	DELETE /api/pdfs/{key}/pin            - 取消主动缓存标记
//	GET    /api/pdfs/{key}/cache          - 获取缓存状态
//	POST   /api/pdfs/{key}/render         - 强制重新渲染缓存
//	GET    /api/pdfs/{key}/marks          - 获取目录和标记列表
//	POST   /api/pdfs/{key}/marks          - 添加用户自定义标记
//	DELETE /api/pdfs/{key}/marks/{markId} - 删除用户自定义标记
//	POST   /api/pdfs/{key}/outline        - 重新提取PDF目录
//
// {key} 是 cache_key（路径哈希），不是数字ID
package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"note-web/internal/cache"
	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/models"
	"note-web/internal/renderer"
)

// Server 是HTTP API服务器，持有所有依赖组件
type Server struct {
	db        *db.Database
	cache     *cache.Manager
	renderer  *renderer.Renderer
	cfg       *config.Config
	webFS     fs.FS
	refresher outlineRefresher
}

// outlineRefresher 让接口层在用户切换目录深度扫描后立即重算目录
type outlineRefresher interface {
	RefreshOutline(pdfID int64) error
}

// New 创建API服务器。webFS 是前端静态文件的根目录（即 web/ 目录本身）。
func New(database *db.Database, cm *cache.Manager, r *renderer.Renderer, cfg *config.Config, webFS fs.FS) *Server {
	return &Server{
		db:       database,
		cache:    cm,
		renderer: r,
		cfg:      cfg,
		webFS:    webFS,
	}
}

// SetOutlineRefresher 注入扫描器，用于按需重算单个 PDF 的目录
func (s *Server) SetOutlineRefresher(refresher outlineRefresher) {
	s.refresher = refresher
}

// Handler 构建完整的 HTTP 处理器：路由 + 静态文件 + 安全响应头。
// 配置了 proxy.base_path 时，同一套路由和静态文件会额外挂载到该前缀下，
// 便于通过公网 Nginx 以 /proxy-index/ 访问，同时保留 / 的日常访问。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	s.registerRoutes(mux, "")
	if s.cfg.Proxy.BasePath != "" {
		s.registerRoutes(mux, s.cfg.Proxy.BasePath)
	}

	fileServer := http.FileServer(http.FS(s.webFS))
	if s.cfg.Proxy.BasePath != "" {
		proxyPath := s.cfg.Proxy.BasePath
		mux.HandleFunc("GET "+proxyPath, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, proxyPath+"/", http.StatusMovedPermanently)
		})
		mux.Handle(proxyPath+"/", http.StripPrefix(proxyPath+"/", fileServer))
	}
	mux.Handle("/", fileServer)

	// 安全响应头中间件（M2）
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
	return handler
}

func (s *Server) Start() error {
	// HTTP 服务器超时设置（H1: 防止 Slowloris DoS）
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("server starting on %s", addr)
	return srv.ListenAndServe()
}

func (s *Server) registerRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/api/pdfs", s.handleListPDFs)
	mux.HandleFunc("GET "+prefix+"/api/pdfs/{key}", s.handleGetPDF)
	mux.HandleFunc("GET "+prefix+"/api/pdfs/{key}/page/{n}", s.handleGetPage)
	mux.HandleFunc("GET "+prefix+"/api/pdfs/{key}/thumb/{n}", s.handleGetThumb)
	mux.HandleFunc("PUT "+prefix+"/api/pdfs/{key}/pin", s.handlePin)
	mux.HandleFunc("DELETE "+prefix+"/api/pdfs/{key}/pin", s.handleUnpin)
	mux.HandleFunc("GET "+prefix+"/api/pdfs/{key}/cache", s.handleCacheStatus)
	mux.HandleFunc("POST "+prefix+"/api/pdfs/{key}/render", s.handleForceRender)
	mux.HandleFunc("GET "+prefix+"/api/pdfs/{key}/marks", s.handleListMarks)
	mux.HandleFunc("PUT "+prefix+"/api/pdfs/{key}/deep-scan", s.handleSetDeepScan)
	mux.HandleFunc("POST "+prefix+"/api/pdfs/{key}/marks", s.handleAddMark)
	mux.HandleFunc("DELETE "+prefix+"/api/pdfs/{key}/marks/{markId}", s.handleDeleteMark)
	mux.HandleFunc("PUT "+prefix+"/api/pdfs/{key}/marks/reorder", s.handleReorderMarks)
	mux.HandleFunc("POST "+prefix+"/api/pdfs/{key}/marks/batch", s.handleBatchAddMarks)
	mux.HandleFunc("POST "+prefix+"/api/pdfs/{key}/outline", s.handleExtractOutline)
	mux.HandleFunc("GET "+prefix+"/api/cache/stats", s.handleCacheStats)
}

// maxBodySize is the maximum JSON body size (H6: prevents memory exhaustion)
const maxBodySize = 1 << 20 // 1MB

// limitedBody wraps r.Body with MaxBytesReader and returns the limited reader.
func limitedBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
}

// maxTitleLen is the maximum title length (L4)
const maxTitleLen = 200

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// relativePath 根据配置的PDF根目录，计算文件的相对路径
// 不匹配任何根目录时返回 "<unknown>" 防止绝对路径泄漏（M5）
func (s *Server) relativePath(absPath string) string {
	for _, dir := range s.cfg.PDF.Dirs {
		dir = strings.TrimSuffix(dir, "/")
		if strings.HasPrefix(absPath, dir+"/") || absPath == dir {
			rel := strings.TrimPrefix(absPath, dir)
			return strings.TrimPrefix(rel, "/")
		}
	}
	return "<unknown>"
}

type pdfListItem struct {
	ID            int64  `json:"id"`
	CacheKey      string `json:"cache_key"`
	Title         string `json:"title"`
	RelativePath  string `json:"relative_path"`
	PageCount     int    `json:"page_count"`
	FileSize      int64  `json:"file_size"`
	Pinned        bool   `json:"pinned"`
	CachedPages   int    `json:"cached_pages"`
	CacheActive   bool   `json:"cache_active"`
	CacheSize     int64  `json:"cache_size_mb"`
	OutlineSource string `json:"outline_source,omitempty"`
	DeepScan      bool   `json:"deep_scan"`
}

// outlineSource 返回目录来源：goodnotes（GoodNotes 自建目录）、
// deep（深度扫描归档内嵌 PDF）、pdf（用 mutool 读 PDF 自身目录）。
// 没有目录时返回空串——不要因为存在 .goodnotes 就谎报来源。
func outlineSource(pdf *models.PDF) string {
	return pdf.OutlineOrigin
}

// pdfItem 把数据库记录转换成接口返回结构
func (s *Server) pdfItem(p *models.PDF) pdfListItem {
	return pdfListItem{
		ID:            p.ID,
		CacheKey:      p.CacheKey,
		Title:         p.Title,
		RelativePath:  s.relativePath(p.Path),
		PageCount:     p.PageCount,
		FileSize:      p.FileSize,
		Pinned:        p.Pinned,
		CachedPages:   p.CachedPages,
		CacheActive:   p.CacheActive,
		CacheSize:     p.CacheSize / (1024 * 1024),
		OutlineSource: outlineSource(p),
		DeepScan:      p.DeepScan != 0,
	}
}

func (s *Server) handleListPDFs(w http.ResponseWriter, r *http.Request) {
	pinnedOnly := r.URL.Query().Has("pinned")

	var pdfs []*models.PDF
	var err error
	if pinnedOnly {
		pdfs, err = s.db.ListPinnedPDFs()
	} else {
		pdfs, err = s.db.ListPDFs()
	}

	if err != nil {
		writeError(w, 500, "failed to list PDFs")
		return
	}

	items := make([]pdfListItem, 0, len(pdfs))
	for _, p := range pdfs {
		items = append(items, s.pdfItem(p))
	}
	writeJSON(w, 200, items)
}

func (s *Server) handleGetPDF(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}
	writeJSON(w, 200, s.pdfItem(pdf))
}

func (s *Server) handleGetPage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	pageNum, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeError(w, 400, "invalid page number")
		return
	}

	filePath, err := s.cache.GetPage(pdf.ID, pageNum)
	if err != nil {
		writeError(w, 404, "page not available") // M13: 不泄漏内部错误
		return
	}

	serveCacheFile(w, r, filePath)

	s.cache.PreloadPages(pdf.ID, pageNum, 2)
}

func (s *Server) handleGetThumb(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	pageNum, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeError(w, 400, "invalid page number")
		return
	}

	filePath, err := s.cache.GetThumbnail(pdf.ID, pageNum)
	if err != nil {
		writeError(w, 404, "thumbnail not available")
		return
	}

	serveCacheFile(w, r, filePath)
}

// serveCacheFile 发送缓存页面图片。
// PDF 内容变化后缓存文件会被替换，因此不能长时间直接复用浏览器缓存
// （否则用户必须手动清空缓存才能看到更新）。这里改为每次向服务器校验：
// 未变化时返回 304（几乎无流量），变化时返回新图片。
func serveCacheFile(w http.ResponseWriter, r *http.Request, filePath string) {
	if stat, err := os.Stat(filePath); err == nil {
		// 文件大小 + 纳秒级修改时间足够区分同一秒内的重新渲染
		w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, stat.Size(), stat.ModTime().UnixNano()))
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeFile(w, r, filePath)
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	if err := s.cache.PinPDF(pdf.ID); err != nil {
		writeError(w, 500, "failed to pin PDF")
		return
	}

	log.Printf("PDF %d pinned: %s", pdf.ID, pdf.Title)
	writeJSON(w, 200, map[string]string{"status": "pinned"})
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	if err := s.cache.UnpinPDF(pdf.ID); err != nil {
		writeError(w, 500, "failed to unpin PDF")
		return
	}

	log.Printf("PDF %d unpinned", pdf.ID)
	writeJSON(w, 200, map[string]string{"status": "unpinned"})
}

func (s *Server) handleCacheStatus(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	status, err := s.db.GetCacheStatus(pdf.ID)
	if err != nil {
		writeError(w, 404, "PDF not found")
		return
	}

	writeJSON(w, 200, status)
}

func (s *Server) handleForceRender(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	if !pdf.Pinned {
		writeError(w, 400, "PDF is not pinned, pin it first")
		return
	}

	s.cache.QueueRender(pdf.ID)
	log.Printf("manual render triggered for PDF %d", pdf.ID)
	writeJSON(w, 202, map[string]string{"status": "queued"})
}

func (s *Server) handleListMarks(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	hasOutline, _ := s.db.HasOutlineMarks(pdf.ID)
	// 只有“没有归档文件”的 PDF 才用 mutool 读它自带的目录；
	// 有 .goodnotes 时目录一律由归档决定，是否读内嵌 PDF 由深度扫描开关控制，
	// 否则用户关掉深度扫描后又被 mutool 自动补回来。
	if pdf.GoodNotesPath == "" && !hasOutline && renderer.HasMutool() {
		s.extractOutline(pdf)
	}

	marks, err := s.db.ListMarks(pdf.ID)
	if err != nil {
		writeError(w, 500, "failed to list marks")
		return
	}
	if marks == nil {
		marks = []*models.PageMark{}
	}
	writeJSON(w, 200, marks)
}

// handleSetDeepScan 打开/关闭某个 PDF 的目录深度扫描，并立即重算目录。
// 深度扫描用于“导入的 PDF 自带目录”这种情况：归档里没有 GoodNotes 目录，
// 需要从内嵌 PDF 附件里读。
func (s *Server) handleSetDeepScan(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	limitedBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}

	mode := 0
	if body.Enabled {
		mode = 1
	}
	if err := s.db.SetDeepScan(pdf.ID, mode); err != nil {
		writeError(w, 500, "failed to update deep scan setting")
		return
	}
	if s.refresher != nil {
		if err := s.refresher.RefreshOutline(pdf.ID); err != nil {
			log.Printf("deep scan refresh failed for PDF %d: %v", pdf.ID, err)
			writeError(w, 500, "failed to refresh outline")
			return
		}
	}

	updated, err := s.db.GetPDF(pdf.ID)
	if err != nil {
		writeError(w, 500, "failed to load PDF")
		return
	}
	writeJSON(w, 200, s.pdfItem(updated))
}

func (s *Server) handleAddMark(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	var body struct {
		Page  int    `json:"page"`
		Title string `json:"title"`
	}
	limitedBody(w, r) // H6
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	if body.Title == "" {
		writeError(w, 400, "title is required")
		return
	}
	if len(body.Title) > maxTitleLen { // L4
		body.Title = body.Title[:maxTitleLen]
	}
	if body.Page < 0 || body.Page > pdf.PageCount {
		writeError(w, 400, "invalid page")
		return
	}

	// 自动归类：解析标题中的编号模式
	displayTitle, parentID, level := classifyTitle(body.Title, nil)

	// 如果自动归类未找到父级，尝试从已有标记中查找
	if parentID == 0 {
		existing, _ := s.db.ListMarks(pdf.ID)
		displayTitle, parentID, level = classifyTitle(body.Title, existing)
	}

	// 排序值：追加到末尾
	maxOrder, _ := s.db.GetMaxSortOrder(pdf.ID)
	sortOrder := maxOrder + 1

	id, err := s.db.AddMark(pdf.ID, body.Page, displayTitle, parentID, level, sortOrder)
	if err != nil {
		writeError(w, 500, "failed to add mark")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"id": id, "status": "added", "title": displayTitle, "parent_id": parentID, "level": level})
}

// classifyTitle 解析标题中的编号模式，自动确定父子关系
// 规则：
//   - 标题包含 "/." -> 转义，将 "/." 替换为 "."，不自动归类（level=0, parentID=0）
//   - 标题以 "数字.数字..." 开头 -> 自动归类为父编号的子项
//     例如 "1.5 引言" -> 父编号 "1"，在已有标记中查找标题以 "1" 开头的标记作为父级
//   - 其他标题 -> 顶级（level=0, parentID=0）
func classifyTitle(title string, existing []*models.PageMark) (displayTitle string, parentID int64, level int) {
	// 转义检查：/. -> . ，不自动归类
	if strings.Contains(title, "/.") {
		displayTitle = strings.ReplaceAll(title, "/.", ".")
		return displayTitle, 0, 0
	}

	// 匹配编号前缀：如 "1", "1.5", "1.5.3"
	numRe := regexp.MustCompile(`^(\d+)((?:\.\d+)*)`)
	match := numRe.FindStringSubmatch(title)
	if match == nil {
		return title, 0, 0
	}

	fullPrefix := match[0] // 如 "1.5"
	// 如果只有一个数字（没有点），是顶级
	if match[2] == "" {
		return title, 0, 0
	}

	// 提取父前缀：最后一个点之前的部分
	lastDot := strings.LastIndex(fullPrefix, ".")
	parentPrefix := fullPrefix[:lastDot] // 如 "1"

	// 在已有标记中查找父前缀匹配的标记
	if existing != nil {
		for _, m := range existing {
			mMatch := numRe.FindString(m.Title)
			if mMatch == parentPrefix {
				return title, m.ID, m.Level + 1
			}
		}
	}

	// 未找到父级，作为顶级
	return title, 0, 0
}

func (s *Server) handleDeleteMark(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	markID, err := strconv.ParseInt(r.PathValue("markId"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid mark id")
		return
	}

	if err := s.db.DeleteMark(markID, pdf.ID); err != nil {
		writeError(w, 500, "failed to delete mark")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// handleReorderMarks 批量更新标记的排序和父子关系（用于拖拽排序）
func (s *Server) handleReorderMarks(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	var items []db.ReorderItem
	limitedBody(w, r) // H6
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	if len(items) > 1000 { // cap array length
		writeError(w, 400, "too many items")
		return
	}

	if err := s.db.ReorderMarks(pdf.ID, items); err != nil {
		writeError(w, 500, "failed to reorder marks")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "reordered"})
}

// handleBatchAddMarks 批量添加标记
// 请求体：[{title: "1 呼吸", page: 1}, {title: "1.1 肺通气", page: 3}, ...]
// 按顺序逐个处理，自动归类（前一个条目可作为后一个的父级）
func (s *Server) handleBatchAddMarks(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	var input []struct {
		Title string `json:"title"`
		Page  int    `json:"page"`
	}
	limitedBody(w, r) // H6
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	if len(input) == 0 {
		writeError(w, 400, "empty batch")
		return
	}
	if len(input) > 500 { // cap array length
		writeError(w, 400, "too many items (max 500)")
		return
	}

	// 获取已有标记（用于查找父级）
	existing, _ := s.db.ListMarks(pdf.ID)
	allMarks := make([]*models.PageMark, len(existing))
	copy(allMarks, existing)

	maxOrder, _ := s.db.GetMaxSortOrder(pdf.ID)
	sortOrder := maxOrder
	count := 0

	// 逐个处理：分类 -> 插入 -> 获取真实ID -> 加入列表供后续查找
	for _, item := range input {
		if item.Title == "" {
			continue
		}
		if len(item.Title) > maxTitleLen { // L4
			item.Title = item.Title[:maxTitleLen]
		}
		if item.Page < 0 || item.Page > pdf.PageCount {
			item.Page = 0
		}

		displayTitle, parentID, level := classifyTitle(item.Title, allMarks)
		sortOrder += 1

		id, err := s.db.AddMark(pdf.ID, item.Page, displayTitle, parentID, level, sortOrder)
		if err != nil {
			log.Printf("batch add mark failed: %v", err)
			continue
		}

		allMarks = append(allMarks, &models.PageMark{
			ID:      id,
			PageNum: item.Page,
			Title:   displayTitle,
			Level:   level,
		})
		count++
	}

	if count == 0 {
		writeError(w, 400, "no valid items added")
		return
	}
	log.Printf("batch added %d marks for PDF %d", count, pdf.ID)
	writeJSON(w, 200, map[string]interface{}{"status": "added", "count": count})
}

func (s *Server) handleExtractOutline(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pdf, err := s.db.GetPDFByCacheKey(key)
	if err != nil || pdf.Deleted {
		writeError(w, 404, "PDF not found")
		return
	}

	if !renderer.HasMutool() {
		writeError(w, 400, "mutool not available")
		return
	}

	count := s.extractOutline(pdf)
	writeJSON(w, 200, map[string]int{"outline_entries": count})
}

// extractOutline 提取PDF目录并存储到数据库
// 使用 mutool show 命令，失败时静默返回0
func (s *Server) extractOutline(pdf *models.PDF) int {
	entries, err := s.renderer.GetOutline(pdf.Path)
	if err != nil {
		log.Printf("outline extraction failed for PDF %d: %v", pdf.ID, err)
		return 0
	}
	if len(entries) == 0 {
		return 0
	}
	// 目标页不存在的目录项保留标题，但不参与跳转
	if pdf.PageCount > 0 {
		for i := range entries {
			if entries[i].Page > pdf.PageCount {
				entries[i].Page = 0
			}
		}
	}
	s.db.DeleteOutlineMarks(pdf.ID)
	if err := s.db.AddOutlineMarks(pdf.ID, entries); err != nil {
		log.Printf("failed to store outline for PDF %d: %v", pdf.ID, err)
		return 0
	}
	// 手动从 PDF 提取的目录与归档来源互斥：清掉归档签名并标记来源为 pdf，
	// 这样下一轮扫描会重新以 .goodnotes 为准，避免"来源显示 GoodNotes、内容是 PDF 目录"。
	if pdf.GoodNotesPath != "" || pdf.OutlineOrigin != "" {
		if err := s.db.SetOutlineSource(pdf.ID, "", 0, 0, "pdf"); err != nil {
			log.Printf("failed to record PDF outline source for %d: %v", pdf.ID, err)
		}
	}
	log.Printf("extracted %d outline entries for PDF %d", len(entries), pdf.ID)
	return len(entries)
}

// handleCacheStats 返回全局缓存使用统计
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	total, err := s.db.GetTotalCacheSize()
	if err != nil {
		writeError(w, 500, "failed to get cache stats")
		return
	}
	maxMB := int64(s.cfg.PassiveCache.MaxSizeMB)
	writeJSON(w, 200, map[string]interface{}{
		"total_mb": total / (1024 * 1024),
		"max_mb":   maxMB,
		"pct":      float64(total) / float64(maxMB*1024*1024) * 100,
	})
}
