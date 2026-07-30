package watcher

import (
	"log"
	"os"
	"sync"
	"time"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/cache"
)

type Watcher struct {
	db       *db.Database
	cache    *cache.Manager
	debounce time.Duration
	interval time.Duration

	timers map[int64]*time.Timer
	mtimes map[int64]int64
	mu     sync.Mutex

	stopCh chan struct{}
}

func New(database *db.Database, cm *cache.Manager, cfg config.ActiveCacheConfig) *Watcher {
	return &Watcher{
		db:       database,
		cache:    cm,
		debounce: cfg.DebounceDuration(),
		interval: cfg.PollDuration(),
		timers:   make(map[int64]*time.Timer),
		mtimes:   make(map[int64]int64),
		stopCh:   make(chan struct{}),
	}
}

func (w *Watcher) Start() {
	go w.poll()
}

func (w *Watcher) Stop() {
	close(w.stopCh)
	w.mu.Lock()
	for _, timer := range w.timers {
		timer.Stop()
	}
	w.timers = make(map[int64]*time.Timer)
	w.mu.Unlock()
}

func (w *Watcher) poll() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.checkFiles()
		}
	}
}

func (w *Watcher) checkFiles() {
	pdfs, err := w.db.ListWatchablePDFs()
	if err != nil {
		log.Printf("watcher: failed to list pinned PDFs: %v", err)
		return
	}

	w.mu.Lock()
	currentIDs := make(map[int64]bool)
	for _, pdf := range pdfs {
		currentIDs[pdf.ID] = true
	}

	for id := range w.mtimes {
		if !currentIDs[id] {
			if timer, exists := w.timers[id]; exists {
				timer.Stop()
				delete(w.timers, id)
			}
			delete(w.mtimes, id)
		}
	}
	w.mu.Unlock()

	for _, pdf := range pdfs {
		stat, err := os.Stat(pdf.Path)
		if err != nil {
			continue
		}

		mtime := stat.ModTime().Unix()

		w.mu.Lock()
		oldMtime, exists := w.mtimes[pdf.ID]
		w.mtimes[pdf.ID] = mtime
		w.mu.Unlock()

		if !exists {
			continue
		}

		if mtime != oldMtime {
			log.Printf("watcher: change detected for PDF %d (%s)", pdf.ID, pdf.Title)
			w.triggerDebounce(pdf.ID)
		}
	}
}

func (w *Watcher) triggerDebounce(pdfID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if timer, exists := w.timers[pdfID]; exists {
		timer.Reset(w.debounce)
		log.Printf("watcher: PDF %d debounce reset to %s", pdfID, w.debounce)
		return
	}

	timer := time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, pdfID)
		w.mu.Unlock()

		log.Printf("watcher: debounce expired, rebuilding cache for PDF %d", pdfID)
		w.cache.QueueRender(pdfID)
	})
	w.timers[pdfID] = timer

	log.Printf("watcher: PDF %d debounce started (%s)", pdfID, w.debounce)
}
