package cache

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/renderer"
)

type Manager struct {
	db       *db.Database
	renderer *renderer.Renderer
	cacheDir string

	maxPassiveSize int64
	renderSem      chan struct{}

	renderQueue  chan int64
	queuedRenders map[int64]bool
	queueMu       sync.Mutex

	stopCh chan struct{}
}

func New(database *db.Database, r *renderer.Renderer, cfg *config.Config) *Manager {
	m := &Manager{
		db:             database,
		renderer:       r,
		cacheDir:       cfg.CacheDir,
		maxPassiveSize: int64(cfg.PassiveCache.MaxSizeMB) * 1024 * 1024,
		renderSem:      make(chan struct{}, cfg.ActiveCache.RenderConcurrency),
		renderQueue:    make(chan int64, 100),
		queuedRenders:  make(map[int64]bool),
		stopCh:         make(chan struct{}),
	}

	os.MkdirAll(m.cacheDir, 0755)
	return m
}

func (m *Manager) pdfDir(cacheKey string) string {
	return filepath.Join(m.cacheDir, cacheKey)
}

func (m *Manager) pagePath(cacheKey string, pageNum int) string {
	return filepath.Join(m.pdfDir(cacheKey), fmt.Sprintf("p%04d.jpg", pageNum))
}

func (m *Manager) thumbPath(cacheKey string, pageNum int) string {
	return filepath.Join(m.pdfDir(cacheKey), fmt.Sprintf("t%04d.jpg", pageNum))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (m *Manager) GetPage(pdfID int64, pageNum int) (string, error) {
	pdf, err := m.db.GetPDF(pdfID)
	if err != nil {
		return "", fmt.Errorf("PDF not found: %w", err)
	}
	if pdf.Deleted {
		return "", fmt.Errorf("PDF not found")
	}
	if pageNum < 1 || pageNum > pdf.PageCount {
		return "", fmt.Errorf("page %d out of range (1-%d)", pageNum, pdf.PageCount)
	}

	filePath := m.pagePath(pdf.CacheKey, pageNum)

	if fileExists(filePath) {
		m.db.TouchCacheAccess(pdfID)
		return filePath, nil
	}

	m.renderSem <- struct{}{}
	defer func() { <-m.renderSem }()

	if fileExists(filePath) {
		m.db.TouchCacheAccess(pdfID)
		return filePath, nil
	}

	os.MkdirAll(filepath.Dir(filePath), 0755)
	outputPrefix := strings.TrimSuffix(filePath, ".jpg")

	if _, err := m.renderer.RenderPage(pdf.Path, pageNum, outputPrefix); err != nil {
		return "", fmt.Errorf("render failed: %w", err)
	}

	m.db.TouchCacheAccess(pdfID)
	if stat, err := os.Stat(filePath); err == nil {
		m.db.IncrementCacheStats(pdfID, stat.Size())
	}

	return filePath, nil
}

func (m *Manager) GetThumbnail(pdfID int64, pageNum int) (string, error) {
	pdf, err := m.db.GetPDF(pdfID)
	if err != nil {
		return "", fmt.Errorf("PDF not found: %w", err)
	}
	if pdf.Deleted {
		return "", fmt.Errorf("PDF not found")
	}
	if pageNum < 1 || pageNum > pdf.PageCount {
		return "", fmt.Errorf("page %d out of range (1-%d)", pageNum, pdf.PageCount)
	}

	thumbPath := m.thumbPath(pdf.CacheKey, pageNum)

	if fileExists(thumbPath) {
		return thumbPath, nil
	}

	m.renderSem <- struct{}{}
	defer func() { <-m.renderSem }()

	if fileExists(thumbPath) {
		return thumbPath, nil
	}

	os.MkdirAll(filepath.Dir(thumbPath), 0755)
	outputPrefix := strings.TrimSuffix(thumbPath, ".jpg")

	if _, err := m.renderer.RenderThumbnail(pdf.Path, pageNum, outputPrefix); err != nil {
		return "", fmt.Errorf("thumbnail render failed: %w", err)
	}

	return thumbPath, nil
}

func (m *Manager) PreloadPages(pdfID int64, currentPage, count int) {
	go func() {
		pdf, err := m.db.GetPDF(pdfID)
		if err != nil || pdf.Deleted {
			return
		}

		for i := 1; i <= count; i++ {
			page := currentPage + i
			if page > pdf.PageCount {
				break
			}

			filePath := m.pagePath(pdf.CacheKey, page)
			if fileExists(filePath) {
				continue
			}

			select {
			case m.renderSem <- struct{}{}:
			default:
				return
			}

			if !fileExists(filePath) {
				os.MkdirAll(filepath.Dir(filePath), 0755)
				outputPrefix := strings.TrimSuffix(filePath, ".jpg")
				if _, err := m.renderer.RenderPage(pdf.Path, page, outputPrefix); err == nil {
					if stat, err := os.Stat(filePath); err == nil {
						m.db.IncrementCacheStats(pdfID, stat.Size())
					}
				}
			}
			<-m.renderSem

			time.Sleep(50 * time.Millisecond)
		}
	}()
}

func (m *Manager) PinPDF(pdfID int64) error {
	if err := m.db.PinPDF(pdfID); err != nil {
		return err
	}
	m.QueueRender(pdfID)
	return nil
}

func (m *Manager) UnpinPDF(pdfID int64) error {
	return m.db.UnpinPDF(pdfID)
}

func (m *Manager) QueueRender(pdfID int64) {
	m.queueMu.Lock()
	if m.queuedRenders[pdfID] {
		m.queueMu.Unlock()
		return
	}
	m.queuedRenders[pdfID] = true
	m.queueMu.Unlock()

	select {
	case m.renderQueue <- pdfID:
	default:
		m.queueMu.Lock()
		delete(m.queuedRenders, pdfID)
		m.queueMu.Unlock()
		log.Printf("render queue full, PDF %d will be picked up next scan", pdfID)
	}
}

func (m *Manager) StartActiveCacheWorker() {
	go func() {
		for {
			select {
			case <-m.stopCh:
				return
			case pdfID := <-m.renderQueue:
				m.queueMu.Lock()
				delete(m.queuedRenders, pdfID)
				m.queueMu.Unlock()

				if err := m.RenderAllPages(pdfID); err != nil {
					log.Printf("active cache render failed for PDF %d: %v", pdfID, err)
				}
			}
		}
	}()
}

func (m *Manager) RenderAllPages(pdfID int64) error {
	pdf, err := m.db.GetPDF(pdfID)
	if err != nil {
		return err
	}
	if pdf.Deleted {
		return fmt.Errorf("PDF %d is deleted", pdfID)
	}

	log.Printf("starting full render for PDF %d (%s, %d pages)", pdfID, pdf.Title, pdf.PageCount)

	m.db.SetRebuilding(pdfID, true)
	defer m.db.SetRebuilding(pdfID, false)

	ck := pdf.CacheKey
	tmpDir := filepath.Join(m.cacheDir, ck+".tmp")
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}

	renderedCount := 0
	totalSize := int64(0)

	for page := 1; page <= pdf.PageCount; page++ {
		select {
		case <-m.stopCh:
			os.RemoveAll(tmpDir)
			return fmt.Errorf("render interrupted")
		default:
		}

		m.renderSem <- struct{}{}

		outputPrefix := filepath.Join(tmpDir, fmt.Sprintf("p%04d", page))
		_, err := m.renderer.RenderPage(pdf.Path, page, outputPrefix)
		<-m.renderSem

		if err != nil {
			log.Printf("  page %d failed: %v", page, err)
			continue
		}

		renderedCount++
		if stat, err := os.Stat(outputPrefix + ".jpg"); err == nil {
			totalSize += stat.Size()
		}

		if page%10 == 0 {
			m.db.UpdateCacheStats(pdfID, renderedCount, totalSize)
			log.Printf("  PDF %d: %d/%d pages rendered", pdfID, page, pdf.PageCount)
		}
	}

	finalDir := m.pdfDir(ck)
	oldDir := filepath.Join(m.cacheDir, ck+".old")

	os.RemoveAll(oldDir)
	if fileExists(finalDir) {
		os.Rename(finalDir, oldDir)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		log.Printf("swap failed, attempting recovery: %v", err)
		if fileExists(oldDir) {
			os.Rename(oldDir, finalDir)
		}
		return fmt.Errorf("swap failed: %w", err)
	}
	os.RemoveAll(oldDir)

	current, err := m.db.GetPDF(pdfID)
	if err == nil && current.Pinned {
		m.db.SetCacheActive(pdfID, true)
	}
	m.db.UpdateCacheStats(pdfID, renderedCount, totalSize)

	log.Printf("completed full render for PDF %d: %d/%d pages, %.1fMB",
		pdfID, renderedCount, pdf.PageCount, float64(totalSize)/(1024*1024))

	return nil
}

func (m *Manager) StartEviction() {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-m.stopCh:
				ticker.Stop()
				return
			case <-ticker.C:
				m.evict()
			}
		}
	}()
}

func (m *Manager) evict() {
	size, err := m.db.GetPassiveCacheSize()
	if err != nil {
		log.Printf("eviction: failed to get cache size: %v", err)
		return
	}

	if size <= m.maxPassiveSize {
		return
	}

	log.Printf("eviction: passive cache %.1fMB exceeds limit %.1fMB, evicting",
		float64(size)/(1024*1024), float64(m.maxPassiveSize)/(1024*1024))

	for size > m.maxPassiveSize {
		pdfs, err := m.db.GetLRUPDFs(1)
		if err != nil || len(pdfs) == 0 {
			break
		}

		pdf := pdfs[0]
		dir := m.pdfDir(pdf.CacheKey)
		os.RemoveAll(dir)
		m.db.ClearCacheStats(pdf.ID)

		size -= pdf.CacheSize
		log.Printf("evicted PDF %d (%s), freed %.1fMB", pdf.ID, pdf.Title, float64(pdf.CacheSize)/(1024*1024))
	}
}

func (m *Manager) Stop() {
	close(m.stopCh)
}
