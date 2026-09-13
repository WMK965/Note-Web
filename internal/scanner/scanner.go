// 目录扫描器包
// 递归扫描配置的 PDF 目录，自动发现新增/变更/删除的 PDF 文件并更新数据库
// 支持定时扫描和首次启动全量扫描
package scanner

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/renderer"
	"note-web/internal/util"
)

// Scanner 是目录扫描器，负责发现和索引 PDF 文件
type Scanner struct {
	db       *db.Database       // 数据库实例
	renderer *renderer.Renderer // 渲染器（用于获取PDF页数和标题）
	cache    cacheInvalidator   // 缓存失效器
	dirs     []string           // 要扫描的目录列表
	interval time.Duration      // 定时扫描间隔
	stopCh   chan struct{}      // 停止信号通道
}

type cacheInvalidator interface {
	InvalidatePDF(pdfID int64) error
}

// New 创建扫描器实例
func New(database *db.Database, r *renderer.Renderer, cfg config.PDFConfig, cache cacheInvalidator) *Scanner {
	return &Scanner{
		db:       database,
		renderer: r,
		cache:    cache,
		dirs:     cfg.Dirs,
		interval: time.Duration(cfg.ScanInterval) * time.Second,
		stopCh:   make(chan struct{}),
	}
}

// Scan 执行一次完整的目录扫描
// 1. 递归遍历所有配置目录，查找 .pdf 文件
// 2. 对每个文件获取页数和标题信息，写入或更新数据库
// 3. 将数据库中未在本次扫描中找到的 PDF 标记为已删除
func (s *Scanner) Scan() error {
	log.Printf("scanning %d directories for PDFs", len(s.dirs))

	foundPaths := make(map[string]bool)

	// 遍历每个配置目录
	for _, dir := range s.dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				log.Printf("scan error at %s: %v", path, err)
				return nil // 跳过错误，继续扫描
			}
			if d.IsDir() {
				return nil
			}
			// H2: 拒绝符号链接，防止路径穿越
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !strings.EqualFold(filepath.Ext(path), ".pdf") {
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

			foundPaths[path] = true
			s.processPDF(path, info)
			return nil
		})
		if err != nil {
			log.Printf("scan failed for dir %s: %v", dir, err)
		}
	}

	// 将扫描结果中不存在的数据库记录标记为已删除
	if err := s.db.MarkDeletedExcept(mapKeys(foundPaths)); err != nil {
		log.Printf("failed to mark deleted: %v", err)
	}

	count, _ := s.countActivePDFs()
	log.Printf("scan complete: %d PDFs found", count)
	return nil
}

// processPDF 处理单个 PDF 文件
// 如果文件是新的或已变更（mtime/size 不同），获取其页数和标题并写入数据库
// 如果未 pinned 的 PDF 发生变更，清除旧缓存，避免继续显示旧页面。
func (s *Scanner) processPDF(path string, info os.FileInfo) {
	mtime := info.ModTime().Unix()

	// 检查数据库中是否已有此文件且未变更
	existing, err := s.db.GetPDFByPath(path)
	if err == nil && existing != nil && !existing.Deleted {
		// mtime 和 size 都没变，跳过
		if existing.FileMtime == mtime && existing.FileSize == info.Size() {
			return
		}
	}

	// 调用 pdfinfo 或 rsc.io/pdf 获取页数和标题
	pdfInfo, err := s.renderer.GetPDFInfo(path)
	if err != nil {
		log.Printf("failed to get info for %s: %v", path, err)
		return
	}

	// 写入数据库，同时计算并存储 cache_key（路径的SHA256哈希前16位）
	id, err := s.db.AddPDF(path, pdfInfo.Title, util.CacheKey(path), pdfInfo.PageCount, info.Size(), mtime)
	if err != nil {
		log.Printf("failed to add/update PDF %s: %v", path, err)
		return
	}

	// 日志输出（L5: 清理标题中的换行和ANSI转义序列）
	safeTitle := strings.ReplaceAll(pdfInfo.Title, "\n", " ")
	safeTitle = strings.ReplaceAll(safeTitle, "\r", "")

	if existing != nil && (existing.FileMtime != mtime || existing.FileSize != info.Size()) {
		// 文件内容变了，PDF 自带目录也可能变了：清除旧目录项，下次打开时重新提取
		if err := s.db.DeleteOutlineMarks(existing.ID); err != nil {
			log.Printf("failed to reset outline for updated PDF %d: %v", existing.ID, err)
		}
		if existing.Pinned {
			// pinned PDF 由 watcher 的 debounce 负责触发增量重渲染。
			log.Printf("pinned PDF changed: %s (id=%d), page count: %d -> %d",
				path, id, existing.PageCount, pdfInfo.PageCount)
		} else if existing.CachedPages > 0 && s.cache != nil {
			if err := s.cache.InvalidatePDF(existing.ID); err != nil {
				log.Printf("failed to invalidate cache for updated PDF %d: %v", existing.ID, err)
			} else {
				log.Printf("invalidated cache for updated PDF %d: %s", existing.ID, safeTitle)
			}
		}
	}

	if id > 0 && existing == nil {
		log.Printf("new PDF: %s (%d pages, %.1fMB)", safeTitle, pdfInfo.PageCount, float64(info.Size())/(1024*1024))
	} else if existing != nil && existing.FileMtime != mtime {
		log.Printf("updated PDF: %s (%d pages)", safeTitle, pdfInfo.PageCount)
	}
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

// countActivePDFs 统计数据库中未删除的PDF数量
func (s *Scanner) countActivePDFs() (int, error) {
	pdfs, err := s.db.ListPDFs()
	if err != nil {
		return 0, err
	}
	return len(pdfs), nil
}

// mapKeys 将 map 的键提取为 slice
func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
