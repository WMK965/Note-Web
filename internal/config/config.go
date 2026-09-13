// 配置管理包
// 从 YAML 配置文件加载配置，未指定的字段使用默认值
// 配置文件不存在时完全使用默认配置，便于 Docker 部署时零配置启动
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是顶层配置结构，包含所有子配置
type Config struct {
	Server       ServerConfig       `yaml:"server"`        // HTTP 服务器配置
	Proxy        ProxyConfig        `yaml:"proxy"`         // 反向代理访问配置
	PDF          PDFConfig          `yaml:"pdf"`           // PDF 目录扫描配置
	Render       RenderConfig       `yaml:"render"`        // 页面渲染参数
	ActiveCache  ActiveCacheConfig  `yaml:"active_cache"`  // 主动缓存配置（pinned PDF 预渲染）
	PassiveCache PassiveCacheConfig `yaml:"passive_cache"` // 被动缓存配置（按需渲染，LRU 淘汰）
	Database     DatabaseConfig     `yaml:"database"`      // SQLite 数据库路径
	CacheDir     string             `yaml:"cache_dir"`     // 渲染后的页面图片缓存目录
}

// ServerConfig 是 HTTP 服务器配置
type ServerConfig struct {
	Host string `yaml:"host"` // 监听地址，默认 "0.0.0.0"
	Port int    `yaml:"port"` // 监听端口，默认 8080
}

// ProxyConfig 是可选的子路径代理配置
// BasePath 设为 "/proxy-index" 后，会额外暴露 /proxy-index/ 和 /proxy-index/api/*
type ProxyConfig struct {
	BasePath string `yaml:"base_path"`
}

// PDFConfig 是 PDF 目录扫描配置
type PDFConfig struct {
	Dirs         []string `yaml:"dirs"`          // PDF 文件所在目录列表（支持多目录）
	ScanInterval int      `yaml:"scan_interval"` // 定时扫描间隔（秒），默认 300（5分钟）
}

// RenderConfig 是页面渲染参数
type RenderConfig struct {
	JpegQuality int `yaml:"jpeg_quality"` // JPEG 压缩质量（1-100），默认 80
	ScaleWidth  int `yaml:"scale_width"`  // 渲染宽度（像素），默认 1080
	ThumbWidth  int `yaml:"thumb_width"`  // 缩略图宽度（像素），默认 200
}

// ActiveCacheConfig 是主动缓存配置
// 主动缓存用于已标记(pinned)的PDF，启动后全量预渲染所有页面
type ActiveCacheConfig struct {
	RenderConcurrency int    `yaml:"render_concurrency"` // 并发渲染线程数，默认 3
	WatchDebounce     string `yaml:"watch_debounce"`     // 文件变化后等待多久再重建缓存，默认 "1h"
	PollInterval      string `yaml:"poll_interval"`      // 文件变化轮询间隔，默认 "30s"
}

// DebounceDuration 将 WatchDebounce 字符串解析为 time.Duration
// 解析失败或为0时返回默认值 1 小时
func (a ActiveCacheConfig) DebounceDuration() time.Duration {
	d, err := time.ParseDuration(a.WatchDebounce)
	if err != nil || d == 0 {
		return 1 * time.Hour
	}
	return d
}

// PollDuration 将 PollInterval 字符串解析为 time.Duration
// 解析失败或为0时返回默认值 30 秒
func (a ActiveCacheConfig) PollDuration() time.Duration {
	d, err := time.ParseDuration(a.PollInterval)
	if err != nil || d == 0 {
		return 30 * time.Second
	}
	return d
}

// PassiveCacheConfig 是被动缓存配置
// 被动缓存用于未标记的PDF，页面在首次访问时按需渲染
type PassiveCacheConfig struct {
	MaxSizeMB int `yaml:"max_size_mb"` // 被动缓存最大磁盘占用（MB），默认 2048（2GB）
}

// DatabaseConfig 是数据库配置
type DatabaseConfig struct {
	Path string `yaml:"path"` // SQLite 数据库文件路径，默认 "/data/note-web.db"
}

// Default 返回默认配置
// 这些默认值与 Docker 部署的 volume 挂载点对应：
//
//	/documents  -> PDF 文件目录（只读挂载）
//	/data       -> 数据库和缓存目录
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Proxy: ProxyConfig{
			BasePath: "",
		},
		PDF: PDFConfig{
			Dirs:         []string{"/documents"},
			ScanInterval: 300,
		},
		Render: RenderConfig{
			JpegQuality: 80,
			ScaleWidth:  1080,
			ThumbWidth:  200,
		},
		ActiveCache: ActiveCacheConfig{
			RenderConcurrency: 3,
			WatchDebounce:     "1h",
			PollInterval:      "30s",
		},
		PassiveCache: PassiveCacheConfig{
			MaxSizeMB: 2048,
		},
		Database: DatabaseConfig{
			Path: "/data/note-web.db",
		},
		CacheDir: "/data/cache",
	}
}

// Load 从指定路径加载配置文件
// 如果文件不存在，返回默认配置（不报错）
// 如果文件存在，用文件内容覆盖默认配置
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // 文件不存在，使用默认配置
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate 检查配置的合法性（M7: 防止极端值导致 panic 或资源耗尽）
func (c *Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Server.Port)
	}
	// 代理前缀：统一规范化为 "/xxx"；"/" 和空值都表示不启用额外入口
	basePath := strings.TrimSpace(c.Proxy.BasePath)
	if basePath != "" {
		if !strings.HasPrefix(basePath, "/") {
			return fmt.Errorf("proxy.base_path must start with /")
		}
		basePath = "/" + strings.Trim(basePath, "/")
	}
	if basePath == "/" {
		basePath = ""
	}
	if basePath != "" {
		if strings.Contains(basePath, "//") || strings.Contains(basePath, "..") ||
			strings.ContainsAny(basePath, `\?#`+" ") {
			return fmt.Errorf("invalid proxy.base_path: %q", basePath)
		}
		if basePath == "/api" || strings.HasPrefix(basePath, "/api/") {
			return fmt.Errorf("proxy.base_path must not overlap /api")
		}
	}
	c.Proxy.BasePath = basePath
	if len(c.PDF.Dirs) == 0 {
		return fmt.Errorf("pdf.dirs must not be empty")
	}
	if c.PDF.ScanInterval <= 0 {
		return fmt.Errorf("pdf.scan_interval must be positive")
	}
	if c.Render.JpegQuality < 1 || c.Render.JpegQuality > 100 {
		return fmt.Errorf("render.jpeg_quality must be 1-100")
	}
	if c.Render.ScaleWidth < 100 || c.Render.ScaleWidth > 10000 {
		return fmt.Errorf("render.scale_width must be 100-10000")
	}
	if c.ActiveCache.RenderConcurrency <= 0 || c.ActiveCache.RenderConcurrency > 16 {
		return fmt.Errorf("active_cache.render_concurrency must be 1-16")
	}
	if c.PassiveCache.MaxSizeMB <= 0 {
		return fmt.Errorf("passive_cache.max_size_mb must be positive")
	}
	if c.CacheDir == "" || strings.Contains(c.CacheDir, "..") {
		return fmt.Errorf("invalid cache_dir")
	}
	if c.Database.Path == "" || strings.Contains(c.Database.Path, "..") {
		return fmt.Errorf("invalid database.path")
	}
	return nil
}
