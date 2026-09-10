// 文件监视器包
// 监视已标记缓存(pinned)的PDF文件变化
// 当文件被修改时（如笔记软件更新备份），启动 debounce 计时器
// 在 debounce 期间（默认1小时）无进一步修改才触发缓存重建
// 这样可以避免在笔记过程中频繁重建缓存
package watcher

import (
	"log"
	"os"
	"sync"
	"time"

	"note-web/internal/cache"
	"note-web/internal/config"
	"note-web/internal/db"
)

// Watcher 监视 pinned PDF 文件的变化
type Watcher struct {
	db       *db.Database    // 数据库实例
	cache    *cache.Manager  // 缓存管理器（用于触发重建）
	debounce time.Duration   // debounce 等待时间（默认1小时）
	interval time.Duration   // 轮询间隔（默认30秒）

	timers map[int64]*time.Timer // 每个 PDF ID 对应的 debounce 计时器
	mtimes map[int64]int64       // 每个 PDF ID 上次记录的文件修改时间
	mu     sync.Mutex            // 保护 timers 和 mtimes

	stopCh chan struct{}         // 停止信号
}

// New 创建监视器实例
func New(database *db.Database, cm *cache.Manager, cfg config.ActiveCacheConfig) *Watcher {
	return &Watcher{
		db:       database,
		cache:    cm,
		debounce: cfg.DebounceDuration(), // 默认 1h
		interval: cfg.PollDuration(),     // 默认 30s
		timers:   make(map[int64]*time.Timer),
		mtimes:   make(map[int64]int64),
		stopCh:   make(chan struct{}),
	}
}

// Start 启动后台轮询 goroutine
func (w *Watcher) Start() {
	go w.poll()
}

// Stop 停止监视器，清理所有计时器
func (w *Watcher) Stop() {
	close(w.stopCh)
	w.mu.Lock()
	for _, timer := range w.timers {
		timer.Stop()
	}
	w.timers = make(map[int64]*time.Timer)
	w.mu.Unlock()
}

// poll 定时轮询所有 pinned PDF 的文件修改时间
// 这是监视的主循环，每 interval（默认30秒）执行一次
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

// checkFiles 检查所有 pinned PDF 文件是否发生了变化
// 1. 从数据库获取所有 pinned PDF 列表
// 2. 清理已取消 pinned 的 PDF 的计时器和记录
// 3. 对每个 pinned PDF 检查文件修改时间
// 4. 如果修改时间变化，触发 debounce
func (w *Watcher) checkFiles() {
	pdfs, err := w.db.ListWatchablePDFs()
	if err != nil {
		log.Printf("watcher: failed to list pinned PDFs: %v", err)
		return
	}

	// 清理已取消 pinned 的 PDF 的监视记录
	w.mu.Lock()
	currentIDs := make(map[int64]bool)
	for _, pdf := range pdfs {
		currentIDs[pdf.ID] = true
	}
	for id := range w.mtimes {
		if !currentIDs[id] {
			// PDF 已不再 pinned，清理计时器和记录
			if timer, exists := w.timers[id]; exists {
				timer.Stop()
				delete(w.timers, id)
			}
			delete(w.mtimes, id)
		}
	}
	w.mu.Unlock()

	// 检查每个 pinned PDF 的文件修改时间
	for _, pdf := range pdfs {
		stat, err := os.Lstat(pdf.Path) // H2: Lstat 不跟随符号链接
		if err != nil || stat.Mode()&os.ModeSymlink != 0 {
			continue // 文件暂时不可访问，跳过
		}

		mtime := stat.ModTime().Unix()

		w.mu.Lock()
		oldMtime, exists := w.mtimes[pdf.ID]
		w.mtimes[pdf.ID] = mtime // 更新记录的修改时间
		w.mu.Unlock()

		if !exists {
			continue // 首次记录，不触发 debounce
		}

		// 修改时间变化 -> 触发 debounce
		if mtime != oldMtime {
			log.Printf("watcher: change detected for PDF %d (%s)", pdf.ID, pdf.Title)
			w.triggerDebounce(pdf.ID)
		}
	}
}

// triggerDebounce 触发或重置 debounce 计时器
// 如果该 PDF 已有计时器在运行，重置为完整的 debounce 时长
// 如果没有计时器，创建一个新的
// 计时器到期后，触发缓存重建
func (w *Watcher) triggerDebounce(pdfID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 已有计时器 -> 重置（重新开始计时）
	if timer, exists := w.timers[pdfID]; exists {
		timer.Reset(w.debounce)
		log.Printf("watcher: PDF %d debounce reset to %s", pdfID, w.debounce)
		return
	}

	// 创建新计时器
	timer := time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, pdfID)
		w.mu.Unlock()

		log.Printf("watcher: debounce expired, rebuilding cache for PDF %d", pdfID)
		w.cache.QueueRender(pdfID) // 触发缓存重建
	})
	w.timers[pdfID] = timer

	log.Printf("watcher: PDF %d debounce started (%s)", pdfID, w.debounce)
}
