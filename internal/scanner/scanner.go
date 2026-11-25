// 目录扫描器包
// 递归扫描配置的 PDF 目录，自动发现新增/变更/删除的 PDF 文件并更新数据库
// 支持定时扫描和首次启动全量扫描
package scanner

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/goodnotes"
	"note-web/internal/models"
	"note-web/internal/pdfoutline"
	"note-web/internal/renderer"
	"note-web/internal/util"
)

// Scanner 是目录扫描器，负责发现和索引 PDF 文件
type Scanner struct {
	db             *db.Database       // 数据库实例
	renderer       *renderer.Renderer // 渲染器（用于获取PDF页数和标题）
	cache          cacheInvalidator   // 缓存失效器
	dirs           []string           // 要扫描的目录列表
	interval       time.Duration      // 定时扫描间隔
	retention      time.Duration      // 文件消失后记录的保留时长（0 表示不保留）
	vacuumInterval time.Duration      // 自动 VACUUM 的最小间隔
	maxFileSize    int64              // 允许解析的 PDF 体积上限
	stopCh         chan struct{}      // 停止信号通道

	lastFound   int  // 上一轮扫描发现的 PDF 数量（用于识别异常骤降）
	hasBaseline bool // 是否已有上一轮扫描结果
}

type cacheInvalidator interface {
	InvalidatePDF(pdfID int64) error
	RemoveCacheDir(cacheKey string) error
	SweepOrphanCacheDirs(liveKeys map[string]bool) (int, int64, error)
}

// New 创建扫描器实例
func New(database *db.Database, r *renderer.Renderer, cfg config.DocumentsConfig, dbCfg config.DatabaseConfig, cache cacheInvalidator) *Scanner {
	return &Scanner{
		db:             database,
		renderer:       r,
		cache:          cache,
		dirs:           cfg.Dirs,
		interval:       cfg.ScanInterval.Value(),
		retention:      cfg.Retention.Value(),
		vacuumInterval: dbCfg.VacuumInterval.Value(),
		maxFileSize:    cfg.MaxFileSize.Value(),
		stopCh:         make(chan struct{}),
	}
}

// Scan 执行一次完整的目录扫描
// 1. 递归遍历所有配置目录，查找 .pdf 文件
// 2. 对每个文件获取页数和标题信息，写入或更新数据库
// 3. 将数据库中未在本次扫描中找到的 PDF 标记为已删除
func (s *Scanner) Scan() error {
	log.Printf("scanning %d directories for PDFs", len(s.dirs))

	foundPaths := make(map[string]bool)
	pdfFiles := make(map[string]scannedFile)
	goodNotesFiles := make(map[string]scannedFile)
	var failedDirs []string

	// 遍历每个配置目录
	for _, dir := range s.dirs {
		walkFailed := false
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				log.Printf("scan error at %s: %v", path, err)
				walkFailed = true
				return nil // 跳过错误，继续扫描
			}
			if d.IsDir() {
				return nil
			}
			// H2: 拒绝符号链接，防止路径穿越
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			extension := filepath.Ext(path)
			if !strings.EqualFold(extension, ".pdf") && !strings.EqualFold(extension, ".goodnotes") {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}
			// 跳过空文件（可能正在写入中）
			if info.Size() == 0 {
				return nil
			}

			file := scannedFile{path: path, info: info}
			key := companionKey(path)
			if strings.EqualFold(extension, ".pdf") {
				foundPaths[path] = true
				pdfFiles[key] = file
			} else {
				goodNotesFiles[key] = file
			}
			return nil
		})
		if err != nil {
			walkFailed = true
			log.Printf("scan failed for dir %s: %v", dir, err)
		}
		if walkFailed {
			failedDirs = append(failedDirs, dir)
		}
	}

	keys := make([]string, 0, len(pdfFiles))
	for key := range pdfFiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pdf := pdfFiles[key]
		companion, hasCompanion := goodNotesFiles[key]
		if hasCompanion {
			s.processPDF(pdf.path, pdf.info, &companion)
		} else {
			s.processPDF(pdf.path, pdf.info, nil)
		}
	}

	// 对账数据库与磁盘：软删除消失的文件、清理超期记录与孤儿缓存
	s.reconcile(foundPaths, failedDirs)

	count, _ := s.db.CountActivePDFs()
	log.Printf("scan complete: %d PDFs found", count)
	return nil
}

type scannedFile struct {
	path string
	info os.FileInfo
}

func companionKey(path string) string {
	extension := filepath.Ext(path)
	return strings.ToLower(filepath.Clean(strings.TrimSuffix(path, extension)))
}

// processPDF 处理单个 PDF 文件
// 如果文件是新的或已变更（mtime/size 不同），获取其页数和标题并写入数据库
// 如果未 pinned 的 PDF 发生变更，清除旧缓存，避免继续显示旧页面。
func (s *Scanner) processPDF(path string, info os.FileInfo, companion *scannedFile) {
	mtime := info.ModTime().Unix()

	// PDF 和同名 GoodNotes 任意一个变化时都要继续处理。
	existing, err := s.db.GetPDFByPath(path)
	pdfChanged := err != nil || existing == nil || existing.Deleted || existing.FileMtime != mtime || existing.FileSize != info.Size()
	goodNotesChanged := false
	if companion != nil {
		goodNotesChanged = existing == nil || existing.GoodNotesPath != companion.path ||
			existing.GoodNotesMtime != companion.info.ModTime().UnixNano() || existing.GoodNotesSize != companion.info.Size()
	} else if existing != nil {
		goodNotesChanged = existing.GoodNotesPath != ""
	}
	outlineNeedsRefresh := existing != nil && existing.ActiveOutlineSources == ""
	if !pdfChanged && !goodNotesChanged && !outlineNeedsRefresh {
		return
	}

	id := int64(0)
	pageCount := 0
	title := ""
	if pdfChanged {
		// 调用 pdfinfo 或 rsc.io/pdf 获取页数和标题
		pdfInfo, infoErr := s.renderer.GetPDFInfo(path)
		if infoErr != nil {
			log.Printf("failed to get info for %s: %v", path, infoErr)
			return
		}
		pageCount = pdfInfo.PageCount
		title = pdfInfo.Title

		// 写入数据库，同时计算并存储 cache_key（路径的SHA256哈希前16位）
		id, err = s.db.AddPDF(path, title, util.CacheKey(path), pageCount, info.Size(), mtime)
		if err != nil {
			log.Printf("failed to add/update PDF %s: %v", path, err)
			return
		}
	} else {
		id = existing.ID
		pageCount = existing.PageCount
		title = existing.Title
	}

	// 日志输出（L5: 清理标题中的换行和ANSI转义序列）
	safeTitle := strings.ReplaceAll(title, "\n", " ")
	safeTitle = strings.ReplaceAll(safeTitle, "\r", "")

	if pdfChanged || goodNotesChanged || outlineNeedsRefresh {
		if err := s.applyOutline(id, pageCount, path, companion, safeTitle); err != nil {
			// 自动备份可能正在写入；保留上一次成功结果并在下轮扫描重试。
			log.Printf("failed to load outline for %s: %v", path, err)
		}
	}

	if existing != nil && pdfChanged {
		if existing.Pinned {
			// pinned PDF 由 watcher 的 debounce 负责触发增量重渲染。
			log.Printf("pinned PDF changed: %s (id=%d), page count: %d -> %d",
				path, id, existing.PageCount, pageCount)
		} else if existing.CachedPages > 0 && s.cache != nil {
			if err := s.cache.InvalidatePDF(existing.ID); err != nil {
				log.Printf("failed to invalidate cache for updated PDF %d: %v", existing.ID, err)
			} else {
				log.Printf("invalidated cache for updated PDF %d: %s", existing.ID, safeTitle)
			}
		}
	}

	if id > 0 && (existing == nil || existing.Deleted) {
		log.Printf("new PDF: %s (%d pages, %.1fMB)", safeTitle, pageCount, float64(info.Size())/(1024*1024))
	} else if existing != nil && pdfChanged {
		log.Printf("updated PDF: %s (%d pages)", safeTitle, pageCount)
	}
}

// applyOutline 按用户来源偏好解析目录并写入数据库，pageCount 用于把越界页码降级为纯标题。
func (s *Scanner) applyOutline(pdfID int64, pageCount int, pdfPath string, companion *scannedFile, title string) error {
	start := time.Now()
	pdf, err := s.db.GetPDF(pdfID)
	if err != nil {
		return err
	}

	preference := strings.TrimSpace(pdf.OutlineSources)
	if preference == "" {
		preference = models.OutlineSourcesAuto
	}
	auto := preference == models.OutlineSourcesAuto
	selected := models.ParseOutlineSources(preference)
	needsNotes := auto || containsOutlineSource(selected, models.OutlineSourceNotes)
	needsSourcePDF := auto || containsOutlineSource(selected, models.OutlineSourceSourcePDF)
	needsPDF := containsOutlineSource(selected, models.OutlineSourcePDF) || (auto && companion == nil)

	entriesBySource := make(map[string][]models.OutlineEntry)
	available := make(map[string]bool)
	if companion != nil && (needsNotes || needsSourcePDF) {
		archiveOutline, err := goodnotes.ParseSources(companion.path, goodnotes.Options{MaxFileSize: s.maxFileSize})
		if err != nil {
			return err
		}
		if needsNotes {
			entriesBySource[models.OutlineSourceNotes] = archiveOutline.Notes
			available[models.OutlineSourceNotes] = len(archiveOutline.Notes) > 0
		}
		if needsSourcePDF {
			entriesBySource[models.OutlineSourceSourcePDF] = archiveOutline.SourcePDF
			available[models.OutlineSourceSourcePDF] = len(archiveOutline.SourcePDF) > 0
		}
	}

	if needsPDF {
		info, statErr := os.Stat(pdfPath)
		if statErr != nil || (s.maxFileSize > 0 && info.Size() > s.maxFileSize) {
			log.Printf("skipping PDF body outline for %s: file exceeds size limit or is unavailable", pdfPath)
		} else {
			entries, parseErr := pdfoutline.ParseFile(pdfPath, pdfoutline.Options{MaxFileSize: s.maxFileSize})
			if parseErr != nil && s.renderer != nil {
				entries, parseErr = s.renderer.GetOutline(pdfPath)
			}
			if parseErr != nil {
				log.Printf("failed to parse PDF body outline for %s: %v", pdfPath, parseErr)
			} else {
				entriesBySource[models.OutlineSourcePDF] = entries
				available[models.OutlineSourcePDF] = len(entries) > 0
			}
		}
	}

	active := selected
	if auto {
		active = nil
		if available[models.OutlineSourceNotes] {
			active = []string{models.OutlineSourceNotes}
		} else if available[models.OutlineSourceSourcePDF] {
			active = []string{models.OutlineSourceSourcePDF}
		} else if companion == nil {
			active = []string{models.OutlineSourcePDF}
		}
	}

	for source, entries := range entriesBySource {
		if !containsOutlineSource(active, source) {
			delete(entriesBySource, source)
			continue
		}
		for i := range entries {
			if entries[i].Page > pageCount {
				entries[i].Page = 0
			}
		}
		entriesBySource[source] = entries
	}

	companionPath := ""
	var fileSize, fileMtime int64
	if companion != nil {
		companionPath = companion.path
		fileSize = companion.info.Size()
		fileMtime = companion.info.ModTime().UnixNano()
	}
	activeRaw := models.EncodeOutlineSources(active)
	if err := s.db.ReplaceOutlineMarks(pdfID, entriesBySource, companionPath, fileSize, fileMtime, preference, activeRaw); err != nil {
		return err
	}
	total := 0
	for _, entries := range entriesBySource {
		total += len(entries)
	}
	if total > 0 {
		log.Printf("loaded %d outline entries from %s for %s (%.1fs)", total, activeRaw, title, time.Since(start).Seconds())
	} else {
		log.Printf("no outline entries found for %s (%.1fs)", title, time.Since(start).Seconds())
	}
	return nil
}

func containsOutlineSource(sources []string, want string) bool {
	for _, source := range sources {
		if source == want {
			return true
		}
	}
	return false
}

// RefreshOutline 立即按当前来源选择重新解析目录，不必等下一轮定时扫描。
func (s *Scanner) RefreshOutline(pdfID int64) error {
	pdf, err := s.db.GetPDF(pdfID)
	if err != nil {
		return err
	}
	if pdf.Deleted {
		return fmt.Errorf("pdf %d is marked as deleted", pdfID)
	}
	var companion *scannedFile
	if companionPath, info, ok := findCompanion(pdf.Path); ok {
		companion = &scannedFile{path: companionPath, info: info}
	}
	return s.applyOutline(pdf.ID, pdf.PageCount, pdf.Path, companion, pdf.Title)
}

// findCompanion 查找与 PDF 同目录同名的 .goodnotes 文件（大小写不敏感）
func findCompanion(pdfPath string) (string, os.FileInfo, bool) {
	base := strings.TrimSuffix(pdfPath, filepath.Ext(pdfPath))
	for _, extension := range []string{".goodnotes", ".GoodNotes", ".GOODNOTES"} {
		candidate := base + extension
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, info, true
		}
	}
	// 其它大小写组合：同目录逐个比对
	dir := filepath.Dir(pdfPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, false
	}
	want := strings.ToLower(filepath.Base(base) + ".goodnotes")
	for _, entry := range entries {
		if entry.IsDir() || strings.ToLower(entry.Name()) != want {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		return filepath.Join(dir, entry.Name()), info, true
	}
	return "", nil, false
}

// Start 启动后台定时扫描 goroutine
// 每隔 interval 时间执行一次 Scan
func (s *Scanner) Start() {
	go func() {
		ticker := time.NewTicker(s.interval)
		for {
			select {
			case <-s.stopCh:
				ticker.Stop()
				return
			case <-ticker.C:
				if err := s.Scan(); err != nil {
					log.Printf("periodic scan failed: %v", err)
				}
			}
		}
	}()
}

// Stop 停止定时扫描
func (s *Scanner) Stop() {
	close(s.stopCh)
}
