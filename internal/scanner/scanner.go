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
func New(database *db.Database, r *renderer.Renderer, cfg config.PDFConfig, dbCfg config.DatabaseConfig, cache cacheInvalidator) *Scanner {
	return &Scanner{
		db:             database,
		renderer:       r,
		cache:          cache,
		dirs:           cfg.Dirs,
		interval:       time.Duration(cfg.ScanInterval) * time.Second,
		retention:      cfg.RetentionDuration(),
		vacuumInterval: dbCfg.VacuumDuration(),
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
	if !pdfChanged && !goodNotesChanged {
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

	if companion != nil {
		// 目录深度扫描：磁盘上的 PDF 没有 GoodNotes 目录时，读归档内嵌 PDF 的书签
		if err := s.applyOutline(id, pageCount, companion.path, companion.info, deepScanEnabled(existing), safeTitle); err != nil {
			// 自动备份可能正在写入；保留上一次成功结果并在下轮扫描重试。
			log.Printf("failed to load outline from %s: %v", companion.path, err)
		}
	} else if existing != nil && existing.GoodNotesPath != "" {
		if err := s.db.DeleteOutlineMarks(id); err != nil {
			log.Printf("failed to clear removed GoodNotes outline for PDF %d: %v", id, err)
		} else if err := s.db.SetOutlineSource(id, "", 0, 0, ""); err != nil {
			log.Printf("failed to clear GoodNotes source for PDF %d: %v", id, err)
		}
	} else if pdfChanged && existing != nil {
		// 没有 GoodNotes 伴随文件时，PDF 变化会让原 PDF 目录失效。
		if err := s.db.DeleteOutlineMarks(id); err != nil {
			log.Printf("failed to reset outline for updated PDF %d: %v", id, err)
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

// applyOutline 解析归档目录并写入数据库，pageCount 用于把越界页码降级为纯标题
func (s *Scanner) applyOutline(pdfID int64, pageCount int, companionPath string, info os.FileInfo, deepScan bool, title string) error {
	outline, err := goodnotes.ParseFileWithOptions(companionPath, goodnotes.Options{DeepScan: deepScan})
	if err != nil {
		return err
	}
	entries := outline.Entries
	for i := range entries {
		if entries[i].Page > pageCount {
			entries[i].Page = 0
		}
	}
	if err := s.db.ReplaceOutlineMarksFromArchive(pdfID, entries, companionPath, info.Size(), info.ModTime().UnixNano(), outline.Source); err != nil {
		return err
	}
	if len(entries) > 0 {
		log.Printf("loaded %d %s outline entries for %s", len(entries), outline.Source, title)
	} else {
		log.Printf("no outline found in %s", companionPath)
	}
	return nil
}

// deepScanEnabled 读取单个 PDF 的深度扫描偏好：1 开启、0 关闭、-1 自动开启
func deepScanEnabled(pdf *models.PDF) bool {
	if pdf == nil {
		return true
	}
	return pdf.DeepScan != 0
}

// RefreshOutline 立即按当前偏好重新解析某个 PDF 的归档目录。
// 用户切换“目录深度扫描”后调用，不必等下一轮定时扫描。
func (s *Scanner) RefreshOutline(pdfID int64) error {
	pdf, err := s.db.GetPDF(pdfID)
	if err != nil {
		return err
	}
	if pdf.Deleted {
		return fmt.Errorf("pdf %d is marked as deleted", pdfID)
	}
	companionPath, info, ok := findCompanion(pdf.Path)
	if !ok {
		// 没有归档文件时没有可解析的目录来源，保持现状
		return nil
	}
	return s.applyOutline(pdf.ID, pdf.PageCount, companionPath, info, deepScanEnabled(pdf), pdf.Title)
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
