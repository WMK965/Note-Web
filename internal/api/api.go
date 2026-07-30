// API 服务器包
// 提供 RESTful API 和嵌入式前端静态文件服务
//
// API 端点：
//   GET    /api/pdfs                      - 列出所有PDF（支持 ?pinned 过滤）
//   GET    /api/pdfs/{key}                - 获取单个PDF信息
//   GET    /api/pdfs/{key}/page/{n}       - 获取第n页的JPEG图片
//   GET    /api/pdfs/{key}/thumb/{n}      - 获取第n页的缩略图
//   PUT    /api/pdfs/{key}/pin            - 标记为主动缓存（触发全量预渲染）
//   DELETE /api/pdfs/{key}/pin            - 取消主动缓存标记
//   GET    /api/pdfs/{key}/cache          - 获取缓存状态
//   POST   /api/pdfs/{key}/render         - 强制重新渲染缓存
//   GET    /api/pdfs/{key}/marks          - 获取目录和标记列表
//   POST   /api/pdfs/{key}/marks          - 添加用户自定义标记
//   DELETE /api/pdfs/{key}/marks/{markId} - 删除用户自定义标记
//   POST   /api/pdfs/{key}/outline        - 重新提取PDF目录
//
// {key} 是 cache_key（路径哈希），不是数字ID
package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"

	"note-web/internal/cache"
	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/models"
	"note-web/internal/renderer"
)

// Server 是HTTP API服务器，持有所有依赖组件
type Server struct {
	db       *db.Database
	cache    *cache.Manager
	renderer *renderer.Renderer
	cfg      *config.Config
	webFS    embed.FS
}

func New(database *db.Database, cm *cache.Manager, r *renderer.Renderer, cfg *config.Config, webFS embed.FS) *Server {
	return &Server{
		db:       database,
		cache:    cm,
		renderer: r,
		cfg:      cfg,
		webFS:    webFS,
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/pdfs", s.handleListPDFs)
	mux.HandleFunc("GET /api/pdfs/{key}", s.handleGetPDF)
	mux.HandleFunc("GET /api/pdfs/{key}/page/{n}", s.handleGetPage)
	mux.HandleFunc("GET /api/pdfs/{key}/thumb/{n}", s.handleGetThumb)
	mux.HandleFunc("PUT /api/pdfs/{key}/pin", s.handlePin)
	mux.HandleFunc("DELETE /api/pdfs/{key}/pin", s.handleUnpin)
	mux.HandleFunc("GET /api/pdfs/{key}/cache", s.handleCacheStatus)
	mux.HandleFunc("POST /api/pdfs/{key}/render", s.handleForceRender)
	mux.HandleFunc("GET /api/pdfs/{key}/marks", s.handleListMarks)
	mux.HandleFunc("POST /api/pdfs/{key}/marks", s.handleAddMark)
	mux.HandleFunc("DELETE /api/pdfs/{key}/marks/{markId}", s.handleDeleteMark)
	mux.HandleFunc("POST /api/pdfs/{key}/outline", s.handleExtractOutline)

	fsys, err := fs.Sub(s.webFS, "web")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(fsys)))

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	log.Printf("server starting on %s", addr)
	return http.ListenAndServe(addr, mux)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// relativePath 根据配置的PDF根目录，计算文件的相对路径
// 例如：绝对路径 /documents/1/note.pdf，根目录 /documents
//      -> 相对路径 "1/note.pdf"（用于前端文件树展示）
func (s *Server) relativePath(absPath string) string {
	for _, dir := range s.cfg.PDF.Dirs {
		dir = strings.TrimSuffix(dir, "/")
		if strings.HasPrefix(absPath, dir+"/") || absPath == dir {
			rel := strings.TrimPrefix(absPath, dir)
			return strings.TrimPrefix(rel, "/")
		}
	}
	return absPath
}

type pdfListItem struct {
	ID           int64  `json:"id"`
	CacheKey     string `json:"cache_key"`
	Title        string `json:"title"`
	RelativePath string `json:"relative_path"`
	PageCount    int    `json:"page_count"`
	FileSize     int64  `json:"file_size"`
	Pinned       bool   `json:"pinned"`
	CachedPages  int    `json:"cached_pages"`
	CacheActive  bool   `json:"cache_active"`
	CacheSize    int64  `json:"cache_size_mb"`
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
		items = append(items, pdfListItem{
			ID:           p.ID,
			CacheKey:     p.CacheKey,
			Title:        p.Title,
			RelativePath: s.relativePath(p.Path),
			PageCount:    p.PageCount,
			FileSize:     p.FileSize,
			Pinned:       p.Pinned,
			CachedPages:  p.CachedPages,
			CacheActive:  p.CacheActive,
			CacheSize:    p.CacheSize / (1024 * 1024),
		})
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
	writeJSON(w, 200, pdfListItem{
		ID:           pdf.ID,
		CacheKey:     pdf.CacheKey,
		Title:        pdf.Title,
		RelativePath: s.relativePath(pdf.Path),
		PageCount:    pdf.PageCount,
		FileSize:     pdf.FileSize,
		Pinned:       pdf.Pinned,
		CachedPages:  pdf.CachedPages,
		CacheActive:  pdf.CacheActive,
		CacheSize:    pdf.CacheSize / (1024 * 1024),
	})
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
		writeError(w, 404, err.Error())
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, filePath)

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
		writeError(w, 404, err.Error())
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=604800")
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
	if !hasOutline && renderer.HasMutool() {
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	if body.Title == "" || body.Page < 1 || body.Page > pdf.PageCount {
		writeError(w, 400, "invalid page or title")
		return
	}

	id, err := s.db.AddMark(pdf.ID, body.Page, body.Title)
	if err != nil {
		writeError(w, 500, "failed to add mark")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"id": id, "status": "added"})
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
	s.db.DeleteOutlineMarks(pdf.ID)
	if err := s.db.AddOutlineMarks(pdf.ID, entries); err != nil {
		log.Printf("failed to store outline for PDF %d: %v", pdf.ID, err)
		return 0
	}
	log.Printf("extracted %d outline entries for PDF %d", len(entries), pdf.ID)
	return len(entries)
}
