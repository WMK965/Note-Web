package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	PDF          PDFConfig          `yaml:"pdf"`
	Render       RenderConfig       `yaml:"render"`
	ActiveCache  ActiveCacheConfig  `yaml:"active_cache"`
	PassiveCache PassiveCacheConfig `yaml:"passive_cache"`
	Database     DatabaseConfig     `yaml:"database"`
	CacheDir     string             `yaml:"cache_dir"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type PDFConfig struct {
	Dirs         []string `yaml:"dirs"`
	ScanInterval int      `yaml:"scan_interval"`
}

type RenderConfig struct {
	JpegQuality int `yaml:"jpeg_quality"`
	ScaleWidth  int `yaml:"scale_width"`
	ThumbWidth  int `yaml:"thumb_width"`
}

type ActiveCacheConfig struct {
	RenderConcurrency int    `yaml:"render_concurrency"`
	WatchDebounce     string `yaml:"watch_debounce"`
	PollInterval      string `yaml:"poll_interval"`
}

func (a ActiveCacheConfig) DebounceDuration() time.Duration {
	d, err := time.ParseDuration(a.WatchDebounce)
	if err != nil || d == 0 {
		return 1 * time.Hour
	}
	return d
}

func (a ActiveCacheConfig) PollDuration() time.Duration {
	d, err := time.ParseDuration(a.PollInterval)
	if err != nil || d == 0 {
		return 30 * time.Second
	}
	return d
}

type PassiveCacheConfig struct {
	MaxSizeMB int `yaml:"max_size_mb"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
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

func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
