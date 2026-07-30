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

type Scanner struct {
	db       *db.Database
	renderer *renderer.Renderer
	dirs     []string
	interval time.Duration
	stopCh   chan struct{}
}

func New(database *db.Database, r *renderer.Renderer, cfg config.PDFConfig) *Scanner {
	return &Scanner{
		db:       database,
		renderer: r,
		dirs:     cfg.Dirs,
		interval: time.Duration(cfg.ScanInterval) * time.Second,
		stopCh:   make(chan struct{}),
	}
}

func (s *Scanner) Scan() error {
	log.Printf("scanning %d directories for PDFs", len(s.dirs))

	foundPaths := make(map[string]bool)

	for _, dir := range s.dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				log.Printf("scan error at %s: %v", path, err)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.EqualFold(filepath.Ext(path), ".pdf") {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}
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

	if err := s.db.MarkDeletedExcept(mapKeys(foundPaths)); err != nil {
		log.Printf("failed to mark deleted: %v", err)
	}

	count, _ := s.countActivePDFs()
	log.Printf("scan complete: %d PDFs found", count)
	return nil
}

func (s *Scanner) processPDF(path string, info os.FileInfo) {
	mtime := info.ModTime().Unix()

	existing, err := s.db.GetPDFByPath(path)
	if err == nil && existing != nil && !existing.Deleted {
		if existing.FileMtime == mtime && existing.FileSize == info.Size() {
			return
		}
	}

	pdfInfo, err := s.renderer.GetPDFInfo(path)
	if err != nil {
		log.Printf("failed to get info for %s: %v", path, err)
		return
	}

	id, err := s.db.AddPDF(path, pdfInfo.Title, util.CacheKey(path), pdfInfo.PageCount, info.Size(), mtime)
	if err != nil {
		log.Printf("failed to add/update PDF %s: %v", path, err)
		return
	}

	if existing != nil && existing.Pinned && (existing.FileMtime != mtime || existing.FileSize != info.Size()) {
		log.Printf("pinned PDF changed: %s (id=%d), page count: %d -> %d",
			path, id, existing.PageCount, pdfInfo.PageCount)
		s.db.UpdatePDFMetadata(id, pdfInfo.PageCount, info.Size(), mtime)
	}

	if id > 0 && existing == nil {
		log.Printf("new PDF: %s (%d pages, %.1fMB)", pdfInfo.Title, pdfInfo.PageCount, float64(info.Size())/(1024*1024))
	} else if existing != nil && existing.FileMtime != mtime {
		log.Printf("updated PDF: %s (%d pages)", pdfInfo.Title, pdfInfo.PageCount)
	}
}

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

func (s *Scanner) Stop() {
	close(s.stopCh)
}

func (s *Scanner) countActivePDFs() (int, error) {
	pdfs, err := s.db.ListPDFs()
	if err != nil {
		return 0, err
	}
	return len(pdfs), nil
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
