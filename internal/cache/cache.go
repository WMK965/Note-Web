// 缓存管理器包
// 管理PDF页面的两级缓存系统：
//   - 主动缓存（Active Cache）：已标记(pinned)的PDF，全量预渲染所有页面，永不淘汰
//   - 被动缓存（Passive Cache）：未标记的PDF，页面在首次访问时按需渲染，按LRU策略淘汰
//
// 缓存目录结构：
//
//	<cache.dir>/<cache_key>/p0001.jpg   <- 第1页的JPEG图片
//	<cache.dir>/<cache_key>/p0002.jpg   <- 第2页
//	<cache.dir>/<cache_key>.tmp/        <- 全量渲染时的临时目录
//	<cache.dir>/<cache_key>.old/        <- 旧缓存（渲染完成后被删除）
//
// cache_key 是PDF文件绝对路径的SHA256哈希前16位，即使数据库重建也不会冲突
package cache

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/models"
	"note-web/internal/renderer"
)

// pdfRenderer 是缓存管理器需要的渲染能力集合。
// 抽成接口便于测试时替换真实的外部命令渲染器。
type pdfRenderer interface {
	GetPDFInfo(pdfPath string) (*renderer.PDFInfo, error)
	RenderPage(pdfPath string, pageNum int, outputPrefix string) (string, error)
	RenderThumbnail(pdfPath string, pageNum int, outputPrefix string) (string, error)
}

// Manager 管理PDF页面缓存的生命周期
type Manager struct {
	db       *db.Database // 数据库实例
	renderer pdfRenderer  // 渲染器
	cacheDir string       // 缓存根目录

	maxPassiveSize int64         // 被动缓存最大磁盘占用（字节）
	renderSem      chan struct{} // 渲染并发信号量（限制同时运行的pdftoppm进程数）

	renderQueue   chan int64     // 主动缓存渲染队列（PDF ID）
	queuedRenders map[int64]bool // 去重集合：记录哪些PDF已在队列中
	queueMu       sync.Mutex     // 保护 queuedRenders 的互斥锁

	stopCh chan struct{} // 停止信号通道
}

// New 创建缓存管理器实例
func New(database *db.Database, r pdfRenderer, cfg *config.Config) *Manager {
	m := &Manager{
		db:             database,
		renderer:       r,
		cacheDir:       cfg.Cache.Dir,
		maxPassiveSize: int64(cfg.Cache.MaxSizeMB) * 1024 * 1024,
		renderSem:      make(chan struct{}, cfg.Cache.Active.RenderConcurrency), // 信号量大小 = 并发渲染数
		renderQueue:    make(chan int64, 100),                                   // 队列缓冲 100 个渲染任务
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

// RemoveCacheDir 删除某个 cache_key 对应的全部缓存产物（正式目录与 .tmp/.old 残片）。
func (m *Manager) RemoveCacheDir(cacheKey string) error {
	if !looksLikeCacheKey(cacheKey) {
		return fmt.Errorf("refusing to remove cache with unexpected key %q", cacheKey)
	}
	for _, dir := range []string{
		m.pdfDir(cacheKey),
		filepath.Join(m.cacheDir, cacheKey+".tmp"),
		filepath.Join(m.cacheDir, cacheKey+".old"),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// SweepOrphanCacheDirs 删除缓存根目录下不再对应任何存活记录的目录。
// liveKeys 是当前存活记录的 cache_key 集合；只有形如缓存键的目录才会被删除，
// 避免误删用户放在同一目录下的其它文件。返回删除的目录数与释放的字节数。
func (m *Manager) SweepOrphanCacheDirs(liveKeys map[string]bool) (int, int64, error) {
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	removed := 0
	var freed int64
	for _, entry := range entries {
		key := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".tmp"), ".old")
		if !looksLikeCacheKey(key) || liveKeys[key] {
			continue
		}
		full := filepath.Join(m.cacheDir, entry.Name())
		size := pathSize(full)
		if err := os.RemoveAll(full); err != nil {
			return removed, freed, err
		}
		removed++
		freed += size
	}
	return removed, freed, nil
}

// looksLikeCacheKey 校验目录名是否是 CacheKey 生成的形态（16位小写十六进制），
// 用于避免清扫时误删缓存目录里的无关内容。
func looksLikeCacheKey(key string) bool {
	if len(key) != 16 {
		return false
	}
	for _, r := range key {
		isDigit := r >= '0' && r <= '9'
		isHexLetter := r >= 'a' && r <= 'f'
		if !isDigit && !isHexLetter {
			return false
		}
	}
	return true
}

// pathSize 统计文件或目录占用的字节数，出错时按 0 处理。
func pathSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
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

func (m *Manager) refreshPDFMetadataIfChanged(pdf *models.PDF) (*models.PDF, error) {
	stat, err := os.Lstat(pdf.Path)
	if err != nil {
		return nil, err
	}
	if stat.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("PDF path is a symlink")
	}

	mtime := stat.ModTime().Unix()
	if pdf.FileMtime == mtime && pdf.FileSize == stat.Size() {
		return pdf, nil
	}

	info, err := m.renderer.GetPDFInfo(pdf.Path)
	if err != nil {
		return nil, err
	}
	if err := m.db.UpdatePDFMetadata(pdf.ID, info.Title, info.PageCount, stat.Size(), mtime); err != nil {
		return nil, err
	}

	if pdf.Pinned {
		m.QueueRender(pdf.ID)
	} else if err := m.invalidatePDFCache(pdf); err != nil {
		return nil, err
	}

	return m.db.GetPDF(pdf.ID)
}

// InvalidatePDF 删除某个 PDF 已生成的页面和缩略图缓存。
func (m *Manager) InvalidatePDF(pdfID int64) error {
	pdf, err := m.db.GetPDF(pdfID)
	if err != nil {
		return err
	}
	return m.invalidatePDFCache(pdf)
}

func (m *Manager) invalidatePDFCache(pdf *models.PDF) error {
	ck := pdf.CacheKey
	for _, dir := range []string{
		m.pdfDir(ck),
		filepath.Join(m.cacheDir, ck+".tmp"),
		filepath.Join(m.cacheDir, ck+".old"),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if err := m.db.ClearCacheStats(pdf.ID); err != nil {
		return err
	}
	// 被动缓存整份失效时无法得知具体页码，记录为“范围未知”，
	// 前端会因此刷新当前页，其余页面在访问时按需重新渲染。
	return m.db.RecordCacheChange(pdf.ID, nil)
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
	pdf, err = m.refreshPDFMetadataIfChanged(pdf)
	if err != nil {
		return "", fmt.Errorf("refresh PDF metadata: %w", err)
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
	pdf, err = m.refreshPDFMetadataIfChanged(pdf)
	if err != nil {
		return "", fmt.Errorf("refresh PDF metadata: %w", err)
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

// RenderAllPages 渲染主动缓存。
// 已有缓存时会逐页比较，只替换内容发生变化的页面，并移除对应缩略图。
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

	ck := pdf.CacheKey
	tmpDir := filepath.Join(m.cacheDir, ck+".tmp")
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.MkdirAll(m.pdfDir(ck), 0755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	processedCount := 0
	changedPages := []int{}
	// 首次填充缓存（此前没有任何缓存页面）不算“文档更新”，无需通知前端刷新
	notifyChange := pdf.CachedPages > 0
	// 渲染完成后的缓存统计（只统计最终目录中真实存在的页面文件）
	availablePages := 0
	cachedBytes := int64(0)

	for page := 1; page <= pdf.PageCount; page++ {
		// 检查是否收到停止信号
		select {
		case <-m.stopCh:
			return fmt.Errorf("render interrupted")
		default:
		}

		finalPath := m.pagePath(ck, page)
		thumbPath := m.thumbPath(ck, page)
		outputPrefix := filepath.Join(tmpDir, fmt.Sprintf("p%04d", page))
		tmpPath := outputPrefix + ".jpg"

		m.renderSem <- struct{}{}
		_, err := m.renderer.RenderPage(pdf.Path, page, outputPrefix)
		<-m.renderSem

		if err == nil && !fileExists(tmpPath) {
			err = fmt.Errorf("renderer produced no output")
		}
		if err != nil {
			log.Printf("  page %d failed: %v", page, err)
			os.Remove(finalPath)
			os.Remove(thumbPath)
			changedPages = append(changedPages, page)
			continue
		}

		same, err := filesEqual(finalPath, tmpPath)
		if err != nil {
			log.Printf("  page %d compare failed: %v", page, err)
		}
		if same {
			os.Remove(tmpPath)
		} else {
			if err := replaceFile(tmpPath, finalPath); err != nil {
				return fmt.Errorf("replace page %d: %w", page, err)
			}
			os.Remove(thumbPath)
			changedPages = append(changedPages, page)
		}

		processedCount++
		if stat, err := os.Stat(finalPath); err == nil {
			availablePages++
			cachedBytes += stat.Size()
		}

		// 每10页更新一次进度（前端可查询）
		if page%10 == 0 {
			m.db.UpdateCacheStats(pdfID, availablePages, cachedBytes)
			log.Printf("  PDF %d: %d/%d pages rendered", pdfID, page, pdf.PageCount)
		}
	}

	if removed, err := m.removePagesAfter(ck, pdf.PageCount); err != nil {
		log.Printf("failed to remove stale pages for PDF %d: %v", pdfID, err)
	} else if len(removed) > 0 {
		changedPages = append(changedPages, removed...)
	}

	changedPages = sortUniqueInts(changedPages)

	current, err := m.db.GetPDF(pdfID)
	if err == nil && current.Pinned {
		m.db.SetCacheActive(pdfID, availablePages >= current.PageCount)
	}
	m.db.UpdateCacheStats(pdfID, availablePages, cachedBytes)
	if len(changedPages) > 0 && notifyChange {
		// 记录本次真正发生变化的页码：前端只刷新这些页面，而不是整份文档
		if err := m.db.RecordCacheChange(pdfID, changedPages); err != nil {
			log.Printf("failed to record cache change for PDF %d: %v", pdfID, err)
		}
	}

	log.Printf("completed render for PDF %d: %d/%d pages available, %.1fMB, changed pages: %s",
		pdfID, availablePages, pdf.PageCount, float64(cachedBytes)/(1024*1024), formatPageList(changedPages))

	if processedCount == 0 && pdf.PageCount > 0 {
		return fmt.Errorf("no pages rendered")
	}

	return nil
}

func (m *Manager) removePagesAfter(cacheKey string, pageCount int) ([]int, error) {
	dir := m.pdfDir(cacheKey)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	removedPages := []int{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		page, ok := pageNumberFromCacheName(entry.Name())
		if !ok || page <= pageCount {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return removedPages, err
		}
		removedPages = append(removedPages, page)
	}
	return removedPages, nil
}

func pageNumberFromCacheName(name string) (int, bool) {
	if len(name) != len("p0001.jpg") || !strings.HasSuffix(name, ".jpg") {
		return 0, false
	}
	if name[0] != 'p' && name[0] != 't' {
		return 0, false
	}
	page, err := strconv.Atoi(name[1:5])
	if err != nil {
		return 0, false
	}
	return page, true
}

func filesEqual(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}

	leftHash, err := fileHash(left)
	if err != nil {
		return false, err
	}
	rightHash, err := fileHash(right)
	if err != nil {
		return false, err
	}
	return leftHash == rightHash, nil
}

func fileHash(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func replaceFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	backup := dst + ".old"
	os.Remove(backup)
	if fileExists(dst) {
		if err := os.Rename(dst, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err != nil {
		if fileExists(backup) {
			os.Rename(backup, dst)
		}
		return err
	}
	os.Remove(backup)
	return nil
}

func formatPageList(pages []int) string {
	if len(pages) == 0 {
		return "none"
	}
	limit := len(pages)
	if limit > 20 {
		limit = 20
	}
	parts := make([]string, 0, limit+1)
	for i := 0; i < limit; i++ {
		parts = append(parts, strconv.Itoa(pages[i]))
	}
	if len(pages) > limit {
		parts = append(parts, fmt.Sprintf("+%d more", len(pages)-limit))
	}
	return strings.Join(parts, ",")
}

// sortUniqueInts 去重并升序排序页码列表
// （缩略图和页面文件都会命中同一页码，需要合并）
func sortUniqueInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	sort.Ints(values)
	unique := values[:1]
	for _, v := range values[1:] {
		if v != unique[len(unique)-1] {
			unique = append(unique, v)
		}
	}
	return unique
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

// evict 执行一轮LRU淘汰。
// 已删除记录的清理已经移到扫描器（见 scanner.reconcile），
// 这里只负责被动缓存的容量控制。
func (m *Manager) evict() {
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
		os.RemoveAll(dir)            // 删除缓存文件
		m.db.ClearCacheStats(pdf.ID) // 清除数据库中的缓存统计

		size -= pdf.CacheSize
		log.Printf("evicted PDF %d (%s), freed %.1fMB", pdf.ID, pdf.Title, float64(pdf.CacheSize)/(1024*1024))
	}
}

// Stop 停止所有后台工作器
func (m *Manager) Stop() {
	close(m.stopCh)
}
