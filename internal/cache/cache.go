// 缓存管理器包
// 管理PDF页面的两级缓存系统：
//   - 主动缓存（Active Cache）：已标记(pinned)的PDF，全量预渲染所有页面，永不淘汰
//   - 被动缓存（Passive Cache）：未标记的PDF，页面在首次访问时按需渲染，按LRU策略淘汰
//
// 缓存目录结构：
//   {cache_dir}/{cache_key}/p0001.jpg   <- 第1页的JPEG图片
//   {cache_dir}/{cache_key}/p0002.jpg   <- 第2页
//   {cache_dir}/{cache_key}.tmp/        <- 全量渲染时的临时目录
//   {cache_dir}/{cache_key}.old/        <- 旧缓存（渲染完成后被删除）
//
// cache_key 是PDF文件绝对路径的SHA256哈希前16位，即使数据库重建也不会冲突
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

// Manager 管理PDF页面缓存的生命周期
type Manager struct {
	db       *db.Database     // 数据库实例
	renderer *renderer.Renderer // 渲染器
	cacheDir string           // 缓存根目录

	maxPassiveSize int64          // 被动缓存最大磁盘占用（字节）
	renderSem      chan struct{}  // 渲染并发信号量（限制同时运行的pdftoppm进程数）

	renderQueue  chan int64       // 主动缓存渲染队列（PDF ID）
	queuedRenders map[int64]bool  // 去重集合：记录哪些PDF已在队列中
	queueMu       sync.Mutex      // 保护 queuedRenders 的互斥锁

	stopCh chan struct{}          // 停止信号通道
}

// New 创建缓存管理器实例
func New(database *db.Database, r *renderer.Renderer, cfg *config.Config) *Manager {
	m := &Manager{
		db:             database,
		renderer:       r,
		cacheDir:       cfg.CacheDir,
		maxPassiveSize: int64(cfg.PassiveCache.MaxSizeMB) * 1024 * 1024,
		renderSem:      make(chan struct{}, cfg.ActiveCache.RenderConcurrency), // 信号量大小 = 并发渲染数
		renderQueue:    make(chan int64, 100), // 队列缓冲 100 个渲染任务
		queuedRenders:  make(map[int64]bool),
		stopCh:         make(chan struct{}),
	}

	os.MkdirAll(m.cacheDir, 0755)
	return m
}

// pdfDir 返回某个PDF的缓存目录路径
func (m *Manager) pdfDir(cacheKey string) string {
	return filepath.Join(m.cacheDir, cacheKey)
}

// pagePath 返回某页的缓存文件路径（格式：{cache_key}/p0001.jpg）
func (m *Manager) pagePath(cacheKey string, pageNum int) string {
	return filepath.Join(m.pdfDir(cacheKey), fmt.Sprintf("p%04d.jpg", pageNum))
}

// thumbPath 返回缩略图缓存文件路径（格式：{cache_key}/t0001.jpg）
func (m *Manager) thumbPath(cacheKey string, pageNum int) string {
	return filepath.Join(m.pdfDir(cacheKey), fmt.Sprintf("t%04d.jpg", pageNum))
}

// fileExists 检查文件是否存在
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// GetPage 获取指定页面的JPEG图片路径（按需渲染）
// 这是用户请求页面时的核心方法：
// 1. 先检查缓存是否已有此页（命中则直接返回）
// 2. 未命中则获取渲染信号量（限制并发），再次检查（防止重复渲染）
// 3. 调用 pdftoppm 渲染页面到缓存目录
// 4. 更新数据库的缓存统计
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

	// 快速路径：缓存命中，直接返回
	if fileExists(filePath) {
		m.db.TouchCacheAccess(pdfID) // 更新最后访问时间（用于LRU淘汰）
		return filePath, nil
	}

	// 慢路径：需要渲染，先获取信号量（限制并发pdftoppm进程数）
	m.renderSem <- struct{}{}
	defer func() { <-m.renderSem }()

	// 双重检查：等待信号量期间可能已被其他请求渲染
	if fileExists(filePath) {
		m.db.TouchCacheAccess(pdfID)
		return filePath, nil
	}

	// 渲染页面到缓存
	os.MkdirAll(filepath.Dir(filePath), 0755)
	outputPrefix := strings.TrimSuffix(filePath, ".jpg")

	if _, err := m.renderer.RenderPage(pdf.Path, pageNum, outputPrefix); err != nil {
		return "", fmt.Errorf("render failed: %w", err)
	}

	// 更新缓存统计
	m.db.TouchCacheAccess(pdfID)
	if stat, err := os.Stat(filePath); err == nil {
		m.db.IncrementCacheStats(pdfID, stat.Size())
	}

	return filePath, nil
}

// GetThumbnail 获取缩略图路径（逻辑同 GetPage，但使用更小尺寸和更低质量）
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

// PreloadPages 在后台预加载当前页之后的若干页
// 用于翻页时提前渲染下一页，使翻页体验更流畅
// 每渲染一页后暂停50ms，避免占用全部CPU资源
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
				continue // 已缓存，跳过
			}

			// 非阻塞获取信号量，如果繁忙则放弃预加载
			select {
			case m.renderSem <- struct{}{}:
			default:
				return // 信号量已满，放弃预加载
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

			time.Sleep(50 * time.Millisecond) // 暂停一下，让出CPU给用户请求
		}
	}()
}

// PinPDF 标记PDF为主动缓存，并排队进行全量预渲染
func (m *Manager) PinPDF(pdfID int64) error {
	if err := m.db.PinPDF(pdfID); err != nil {
		return err
	}
	m.QueueRender(pdfID) // 加入渲染队列
	return nil
}

// UnpinPDF 取消主动缓存标记，缓存转为被动模式（可被LRU淘汰）
func (m *Manager) UnpinPDF(pdfID int64) error {
	return m.db.UnpinPDF(pdfID)
}

// QueueRender 将PDF加入主动缓存渲染队列
// 使用 queuedRenders 集合去重，同一PDF不会重复排队
func (m *Manager) QueueRender(pdfID int64) {
	m.queueMu.Lock()
	if m.queuedRenders[pdfID] {
		m.queueMu.Unlock()
		return // 已在队列中，不重复排队
	}
	m.queuedRenders[pdfID] = true
	m.queueMu.Unlock()

	// 非阻塞写入队列，队列满时放弃（下次定时扫描会重新检查）
	select {
	case m.renderQueue <- pdfID:
	default:
		m.queueMu.Lock()
		delete(m.queuedRenders, pdfID)
		m.queueMu.Unlock()
		log.Printf("render queue full, PDF %d will be picked up next scan", pdfID)
	}
}

// StartActiveCacheWorker 启动主动缓存渲染工作器
// 从渲染队列取出PDF ID，逐个执行全量渲染
func (m *Manager) StartActiveCacheWorker() {
	go func() {
		for {
			select {
			case <-m.stopCh:
				return
			case pdfID := <-m.renderQueue:
				// 从去重集合中移除
				m.queueMu.Lock()
				delete(m.queuedRenders, pdfID)
				m.queueMu.Unlock()

				// 执行全量渲染
				if err := m.RenderAllPages(pdfID); err != nil {
					log.Printf("active cache render failed for PDF %d: %v", pdfID, err)
				}
			}
		}
	}()
}

// RenderAllPages 全量渲染PDF的所有页面到缓存
// 这是主动缓存的核心方法，流程：
// 1. 在临时目录 {cache_key}.tmp/ 中逐页渲染
// 2. 渲染完成后，原子替换：旧目录改名为 .old，临时目录改名为正式目录，删除 .old
// 3. 更新数据库缓存统计
//
// 原子替换确保渲染期间旧缓存继续可用，不受影响
func (m *Manager) RenderAllPages(pdfID int64) error {
	pdf, err := m.db.GetPDF(pdfID)
	if err != nil {
		return err
	}
	if pdf.Deleted {
		return fmt.Errorf("PDF %d is deleted", pdfID)
	}

	log.Printf("starting full render for PDF %d (%s, %d pages)", pdfID, pdf.Title, pdf.PageCount)

	// 标记为正在重建（前端可显示进度）
	m.db.SetRebuilding(pdfID, true)
	defer m.db.SetRebuilding(pdfID, false)

	// 创建临时目录
	ck := pdf.CacheKey
	tmpDir := filepath.Join(m.cacheDir, ck+".tmp")
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}

	renderedCount := 0
	totalSize := int64(0)

	// 逐页渲染到临时目录
	for page := 1; page <= pdf.PageCount; page++ {
		// 检查是否收到停止信号
		select {
		case <-m.stopCh:
			os.RemoveAll(tmpDir)
			return fmt.Errorf("render interrupted")
		default:
		}

		// 获取渲染信号量
		m.renderSem <- struct{}{}

		outputPrefix := filepath.Join(tmpDir, fmt.Sprintf("p%04d", page))
		_, err := m.renderer.RenderPage(pdf.Path, page, outputPrefix)
		<-m.renderSem

		if err != nil {
			log.Printf("  page %d failed: %v", page, err)
			continue // 单页失败不影响其他页
		}

		renderedCount++
		if stat, err := os.Stat(outputPrefix + ".jpg"); err == nil {
			totalSize += stat.Size()
		}

		// 每10页更新一次进度（前端可查询）
		if page%10 == 0 {
			m.db.UpdateCacheStats(pdfID, renderedCount, totalSize)
			log.Printf("  PDF %d: %d/%d pages rendered", pdfID, page, pdf.PageCount)
		}
	}

	// 原子替换：旧目录 -> .old，临时目录 -> 正式目录，删除 .old
	finalDir := m.pdfDir(ck)
	oldDir := filepath.Join(m.cacheDir, ck+".old")

	os.RemoveAll(oldDir)
	if fileExists(finalDir) {
		os.Rename(finalDir, oldDir) // 旧目录改名
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		// 替换失败，尝试恢复旧目录
		log.Printf("swap failed, attempting recovery: %v", err)
		if fileExists(oldDir) {
			os.Rename(oldDir, finalDir)
		}
		return fmt.Errorf("swap failed: %w", err)
	}
	os.RemoveAll(oldDir) // 清理旧目录

	// 如果PDF仍然被pinned（可能渲染期间被取消），标记为主动缓存
	current, err := m.db.GetPDF(pdfID)
	if err == nil && current.Pinned {
		m.db.SetCacheActive(pdfID, true)
	}
	m.db.UpdateCacheStats(pdfID, renderedCount, totalSize)

	log.Printf("completed full render for PDF %d: %d/%d pages, %.1fMB",
		pdfID, renderedCount, pdf.PageCount, float64(totalSize)/(1024*1024))

	return nil
}

// StartEviction 启动被动缓存LRU淘汰器
// 每5分钟检查一次，当被动缓存总大小超过限制时，淘汰最久未访问的PDF缓存
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

// evict 执行一轮LRU淘汰与软删除清理
// M9: 先清理已删除PDF的缓存文件和标记，再检查缓存大小
func (m *Manager) evict() {
	// 清理已删除PDF的缓存（M9）
	ids := m.db.CleanupDeletedPDFs()
	for _, id := range ids {
		// 获取cache_key来删除缓存目录
		pdf, err := m.db.GetPDF(id)
		if err != nil {
			continue
		}
		dir := m.pdfDir(pdf.CacheKey)
		os.RemoveAll(dir)
	}

	size, err := m.db.GetTotalCacheSize()
	if err != nil {
		log.Printf("eviction: failed to get cache size: %v", err)
		return
	}

	if size <= m.maxPassiveSize {
		return // 未超限，无需淘汰
	}

	log.Printf("eviction: passive cache %.1fMB exceeds limit %.1fMB, evicting",
		float64(size)/(1024*1024), float64(m.maxPassiveSize)/(1024*1024))

	// 循环淘汰直到低于限制
	for size > m.maxPassiveSize {
		pdfs, err := m.db.GetLRUPDFs(1) // 获取最久未访问的1个PDF
		if err != nil || len(pdfs) == 0 {
			break
		}

		pdf := pdfs[0]
		dir := m.pdfDir(pdf.CacheKey)
		os.RemoveAll(dir)              // 删除缓存文件
		m.db.ClearCacheStats(pdf.ID)   // 清除数据库中的缓存统计

		size -= pdf.CacheSize
		log.Printf("evicted PDF %d (%s), freed %.1fMB", pdf.ID, pdf.Title, float64(pdf.CacheSize)/(1024*1024))
	}
}

// Stop 停止所有后台工作器
func (m *Manager) Stop() {
	close(m.stopCh)
}
