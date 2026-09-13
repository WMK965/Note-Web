// Note-Web 主程序入口
// 负责初始化所有组件（配置、数据库、渲染器、缓存管理器、扫描器、监视器、API服务器）
// 并在收到 SIGINT/SIGTERM 信号时优雅关闭
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"note-web/internal/api"
	"note-web/internal/cache"
	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/renderer"
	"note-web/internal/scanner"
	"note-web/internal/watcher"
)

// 将 web/ 目录下的前端文件（HTML/CSS/JS）嵌入到二进制中
// 编译后不需要额外携带前端文件
//
//go:embed web
var webFS embed.FS

func main() {
	// 解析命令行参数：-config 指定配置文件路径，默认 "config.yaml"
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// 加载配置：先读取配置文件覆盖默认值，文件不存在则使用默认配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// 检查系统依赖：pdftoppm 是必需的（用于渲染PDF页面为JPEG）
	if err := renderer.CheckDependencies(); err != nil {
		log.Fatalf("dependency check failed: %v", err)
	}

	// 创建缓存目录和数据库目录（如果不存在）
	log.Printf("initializing database at %s", cfg.Database.Path)
	os.MkdirAll(cfg.CacheDir, 0755)
	os.MkdirAll(filepath.Dir(cfg.Database.Path), 0755)

	// 初始化 SQLite 数据库（使用 WAL 模式提高并发性能）
	database, err := db.Init(cfg.Database.Path)
	if err != nil {
		log.Fatalf("failed to init database: %v", err)
	}
	defer database.Close()

	// 创建 PDF 渲染器（封装 pdftoppm 和 pdfinfo 命令）
	r := renderer.New(cfg.Render)

	// 创建缓存管理器（管理主动缓存和被动缓存两级缓存）
	cm := cache.New(database, r, cfg)

	// 创建目录扫描器（递归扫描配置目录，自动发现PDF文件）
	s := scanner.New(database, r, cfg.PDF, cfg.Database, cm)
	if err := s.Scan(); err != nil { // 启动时立即执行一次完整扫描
		log.Printf("initial scan failed: %v", err)
	}
	s.Start() // 启动后台定时扫描

	// 启动缓存后台工作器
	cm.StartActiveCacheWorker() // 主动缓存预渲染工作器
	cm.StartEviction()          // 被动缓存 LRU 淘汰器

	// 启动时检查已标记缓存的 PDF，如果缓存不完整则排队重新渲染
	pinnedPDFs, _ := database.ListPinnedPDFs()
	for _, pdf := range pinnedPDFs {
		if pdf.CachedPages < pdf.PageCount {
			log.Printf("queueing pinned PDF for render: %s (%d/%d cached)", pdf.Title, pdf.CachedPages, pdf.PageCount)
			cm.QueueRender(pdf.ID)
		}
	}

	// 创建文件监视器（监视已标记缓存的PDF文件变化，1小时无更新后触发缓存重建）
	w := watcher.New(database, cm, cfg.ActiveCache)
	w.Start()

	// 启动 HTTP API 服务器（同时提供前端静态文件服务）
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("failed to load embedded web assets: %v", err)
	}
	server := api.New(database, cm, r, cfg, webRoot)
	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("server failed: %v", err)
		}
	}()

	log.Printf("Note-Web ready: serving on %s:%d", cfg.Server.Host, cfg.Server.Port)

	// 等待退出信号（Ctrl+C 或 docker stop）
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	// 优雅关闭：停止各组件
	log.Printf("shutting down...")
	w.Stop()
	s.Stop()
	cm.Stop()
}
