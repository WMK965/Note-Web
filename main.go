package main

import (
	"embed"
	"flag"
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

//go:embed web
var webFS embed.FS

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if err := renderer.CheckDependencies(); err != nil {
		log.Fatalf("dependency check failed: %v", err)
	}

	log.Printf("initializing database at %s", cfg.Database.Path)
	os.MkdirAll(cfg.CacheDir, 0755)
	os.MkdirAll(filepath.Dir(cfg.Database.Path), 0755)

	database, err := db.Init(cfg.Database.Path)
	if err != nil {
		log.Fatalf("failed to init database: %v", err)
	}
	defer database.Close()

	r := renderer.New(cfg.Render)

	cm := cache.New(database, r, cfg)

	s := scanner.New(database, r, cfg.PDF)
	if err := s.Scan(); err != nil {
		log.Printf("initial scan failed: %v", err)
	}
	s.Start()

	cm.StartActiveCacheWorker()
	cm.StartEviction()

	pinnedPDFs, _ := database.ListPinnedPDFs()
	for _, pdf := range pinnedPDFs {
		if pdf.CachedPages < pdf.PageCount {
			log.Printf("queueing pinned PDF for render: %s (%d/%d cached)", pdf.Title, pdf.CachedPages, pdf.PageCount)
			cm.QueueRender(pdf.ID)
		}
	}

	w := watcher.New(database, cm, cfg.ActiveCache)
	w.Start()

	server := api.New(database, cm, r, cfg, webFS)
	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("server failed: %v", err)
		}
	}()

	log.Printf("Note-Web ready: serving on %s:%d", cfg.Server.Host, cfg.Server.Port)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("shutting down...")
	w.Stop()
	s.Stop()
	cm.Stop()
}
