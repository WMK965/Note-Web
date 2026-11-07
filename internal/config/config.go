// 配置管理包
// 从 YAML 配置文件加载配置，未指定的字段使用默认值
// 配置文件不存在时完全使用默认配置，便于 Docker 部署时零配置启动
//
// 配置结构：
//
//	server.host / server.port / server.proxy_path
//	documents.dirs / documents.scan_interval / documents.retention / documents.max_file_size
//	cache.dir / cache.max_size_mb / cache.active.* / cache.render.*
//	database.path / database.vacuum_interval
//
// 时间统一写成带单位的时长（s 秒、m 分、h 时、d 天），例如 "30s"、"5m"、"1h30m"、"7d"。
package config

import (
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是顶层配置结构
type Config struct {
	Server    ServerConfig    `yaml:"server"`    // HTTP 服务器与代理前缀
	Documents DocumentsConfig `yaml:"documents"` // 文档目录与扫描策略
	Cache     CacheConfig     `yaml:"cache"`     // 缓存目录、容量与渲染参数
	Database  DatabaseConfig  `yaml:"database"`  // 数据库路径与维护间隔

	// Deprecated: 以下为旧版配置字段，仅为兼容已有部署保留。
	// 解析后会合并进上面的新结构并打印告警，请尽快迁移。
	Proxy        DeprecatedProxy         `yaml:"proxy"`
	PDF          DeprecatedPDF           `yaml:"pdf"`
	Render       *RenderConfig           `yaml:"render"`
	ActiveCache  *ActiveCacheConfig      `yaml:"active_cache"`
	PassiveCache *DeprecatedPassiveCache `yaml:"passive_cache"`
	CacheDir     string                  `yaml:"cache_dir"`
}

// ServerConfig 是 HTTP 服务器配置
type ServerConfig struct {
	Host      string `yaml:"host"`       // 监听地址，默认 "0.0.0.0"
	Port      int    `yaml:"port"`       // 监听端口，默认 8080
	ProxyPath string `yaml:"proxy_path"` // 可选：额外挂载的访问前缀，例如 "/proxy-index"
}

// DocumentsConfig 是文档目录与扫描策略
type DocumentsConfig struct {
	Dirs         []string `yaml:"dirs"`          // 文档目录列表（支持多目录）
	ScanInterval Duration `yaml:"scan_interval"` // 定时扫描间隔，默认 5m
	Retention    Duration `yaml:"retention"`     // 文件消失后记录保留多久，默认 30d；0 表示下一轮扫描即清理
	MaxFileSize  ByteSize `yaml:"max_file_size"` // 允许解析的 PDF 体积上限，默认 1G
}

// CacheConfig 是缓存配置
type CacheConfig struct {
	Dir       string            `yaml:"dir"`         // 渲染缓存目录，默认 "/data/cache"
	MaxSizeMB int               `yaml:"max_size_mb"` // 被动缓存最大占用（MB），默认 2048
	Active    ActiveCacheConfig `yaml:"active"`      // 主动缓存
	Render    RenderConfig      `yaml:"render"`      // 页面渲染参数
}

// ActiveCacheConfig 是主动缓存配置
// 主动缓存用于已标记(pinned)的PDF，启动后全量预渲染所有页面
type ActiveCacheConfig struct {
	RenderConcurrency int      `yaml:"render_concurrency"` // 并发渲染线程数，默认 3
	WatchDebounce     Duration `yaml:"watch_debounce"`     // 文件变化后等待多久再重建缓存，默认 1h
	PollInterval      Duration `yaml:"poll_interval"`      // 文件变化轮询间隔，默认 30s
}

// RenderConfig 是页面渲染参数
type RenderConfig struct {
	JpegQuality int `yaml:"jpeg_quality"` // JPEG 压缩质量（1-100），默认 80
	ScaleWidth  int `yaml:"scale_width"`  // 页面短边目标宽度（像素），默认 1080
	ThumbWidth  int `yaml:"thumb_width"`  // 缩略图宽度（像素），默认 200
}

// DatabaseConfig 是数据库配置
type DatabaseConfig struct {
	Path           string   `yaml:"path"`            // SQLite 数据库文件路径，默认 "/data/note-web.db"
	VacuumInterval Duration `yaml:"vacuum_interval"` // 自动 VACUUM 的最小间隔，默认 7d
}

// DeprecatedProxy 是旧版 proxy 配置
type DeprecatedProxy struct {
	BasePath string `yaml:"base_path"`
}

// DeprecatedPDF 是旧版 pdf 配置
type DeprecatedPDF struct {
	Dirs          []string `yaml:"dirs"`
	ScanInterval  int      `yaml:"scan_interval"`
	RetentionDays int      `yaml:"retention_days"`
}

// DeprecatedPassiveCache 是旧版 passive_cache 配置
type DeprecatedPassiveCache struct {
	MaxSizeMB int `yaml:"max_size_mb"`
}

// Default 返回默认配置
// 这些默认值与 Docker 部署的 volume 挂载点对应：
//
//	/documents  -> 文档目录（只读挂载）
//	/data       -> 数据库与缓存目录
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Documents: DocumentsConfig{
			Dirs:         []string{"/documents"},
			ScanInterval: DurationOf(5 * time.Minute),
			Retention:    DurationOf(30 * 24 * time.Hour),
			MaxFileSize:  ByteSizeOf(1 << 30),
		},
		Cache: CacheConfig{
			Dir:       "/data/cache",
			MaxSizeMB: 2048,
			Active: ActiveCacheConfig{
				RenderConcurrency: 3,
				WatchDebounce:     DurationOf(1 * time.Hour),
				PollInterval:      DurationOf(30 * time.Second),
			},
			Render: RenderConfig{
				JpegQuality: 80,
				ScaleWidth:  1080,
				ThumbWidth:  200,
			},
		},
		Database: DatabaseConfig{
			Path:           "/data/note-web.db",
			VacuumInterval: DurationOf(7 * 24 * time.Hour),
		},
	}
}

// Load 从指定路径加载配置文件
// 如果文件不存在，返回默认配置（不报错）
// 如果文件存在，用文件内容覆盖默认值，并把旧版字段迁移到新结构
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
	cfg.applyDeprecated(data)

	return cfg, nil
}

// applyDeprecated 把旧版字段合并到新结构。
// 只有当配置里完全没有对应的新段落时才合并，避免覆盖用户已经迁移好的配置。
func (c *Config) applyDeprecated(data []byte) {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(data, &top); err != nil {
		return
	}
	_, hasDocuments := top["documents"]
	_, hasCache := top["cache"]

	var moved []string
	if !hasNestedKey(top, "server", "proxy_path") && strings.TrimSpace(c.Proxy.BasePath) != "" {
		c.Server.ProxyPath = c.Proxy.BasePath
		moved = append(moved, "proxy.base_path → server.proxy_path")
	}
	if !hasDocuments {
		if len(c.PDF.Dirs) > 0 {
			c.Documents.Dirs = c.PDF.Dirs
			moved = append(moved, "pdf.dirs → documents.dirs")
		}
		if c.PDF.ScanInterval > 0 {
			c.Documents.ScanInterval = DurationOf(time.Duration(c.PDF.ScanInterval) * time.Second)
			moved = append(moved, "pdf.scan_interval → documents.scan_interval")
		}
		if c.PDF.RetentionDays > 0 {
			c.Documents.Retention = DurationOf(time.Duration(c.PDF.RetentionDays) * 24 * time.Hour)
			moved = append(moved, "pdf.retention_days → documents.retention")
		}
	}
	if !hasCache {
		if c.Render != nil {
			c.Cache.Render = *c.Render
			moved = append(moved, "render.* → cache.render.*")
		}
		if c.ActiveCache != nil {
			c.Cache.Active = *c.ActiveCache
			moved = append(moved, "active_cache.* → cache.active.*")
		}
		if c.PassiveCache != nil && c.PassiveCache.MaxSizeMB > 0 {
			c.Cache.MaxSizeMB = c.PassiveCache.MaxSizeMB
			moved = append(moved, "passive_cache.max_size_mb → cache.max_size_mb")
		}
		if strings.TrimSpace(c.CacheDir) != "" {
			c.Cache.Dir = c.CacheDir
			moved = append(moved, "cache_dir → cache.dir")
		}
	}
	if len(moved) > 0 {
		log.Printf("config: 检测到旧版配置字段，已自动迁移（请尽快改为新结构）：%s", strings.Join(moved, "，"))
	}
}

// hasNestedKey 判断配置里是否显式写了 section.key
func hasNestedKey(top map[string]yaml.Node, section, key string) bool {
	node, ok := top[section]
	if !ok {
		return false
	}
	var inner map[string]yaml.Node
	if err := node.Decode(&inner); err != nil {
		return false
	}
	_, ok = inner[key]
	return ok
}

// Validate 检查配置的合法性（M7: 防止极端值导致 panic 或资源耗尽）
func (c *Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Server.Port)
	}
	// 代理前缀：统一规范化为 "/xxx"；"/" 和空值都表示不启用额外入口
	proxyPath := strings.TrimSpace(c.Server.ProxyPath)
	if proxyPath != "" {
		if !strings.HasPrefix(proxyPath, "/") {
			return fmt.Errorf("server.proxy_path must start with /")
		}
		proxyPath = "/" + strings.Trim(proxyPath, "/")
	}
	if proxyPath == "/" {
		proxyPath = ""
	}
	if proxyPath != "" {
		if strings.Contains(proxyPath, "//") || strings.Contains(proxyPath, "..") ||
			strings.ContainsAny(proxyPath, `\?#`+" ") {
			return fmt.Errorf("invalid server.proxy_path: %q", proxyPath)
		}
		if proxyPath == "/api" || strings.HasPrefix(proxyPath, "/api/") {
			return fmt.Errorf("server.proxy_path must not overlap /api")
		}
	}
	c.Server.ProxyPath = proxyPath

	if len(c.Documents.Dirs) == 0 {
		return fmt.Errorf("documents.dirs must not be empty")
	}
	if c.Documents.ScanInterval.Value() <= 0 {
		return fmt.Errorf("documents.scan_interval must be positive")
	}
	if c.Documents.Retention.Value() < 0 {
		return fmt.Errorf("documents.retention must not be negative")
	}
	if c.Documents.MaxFileSize.Value() <= 0 {
		return fmt.Errorf("documents.max_file_size must be positive")
	}
	if c.Cache.Render.JpegQuality < 1 || c.Cache.Render.JpegQuality > 100 {
		return fmt.Errorf("cache.render.jpeg_quality must be 1-100")
	}
	if c.Cache.Render.ScaleWidth < 100 || c.Cache.Render.ScaleWidth > 10000 {
		return fmt.Errorf("cache.render.scale_width must be 100-10000")
	}
	if c.Cache.Active.RenderConcurrency <= 0 || c.Cache.Active.RenderConcurrency > 16 {
		return fmt.Errorf("cache.active.render_concurrency must be 1-16")
	}
	if c.Cache.Active.WatchDebounce.Value() <= 0 {
		return fmt.Errorf("cache.active.watch_debounce must be positive")
	}
	if c.Cache.Active.PollInterval.Value() <= 0 {
		return fmt.Errorf("cache.active.poll_interval must be positive")
	}
	if c.Cache.MaxSizeMB <= 0 {
		return fmt.Errorf("cache.max_size_mb must be positive")
	}
	if c.Cache.Dir == "" || strings.Contains(c.Cache.Dir, "..") {
		return fmt.Errorf("invalid cache.dir")
	}
	if c.Database.Path == "" || strings.Contains(c.Database.Path, "..") {
		return fmt.Errorf("invalid database.path")
	}
	if c.Database.VacuumInterval.Value() <= 0 {
		return fmt.Errorf("database.vacuum_interval must be positive")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 时长与体积类型
// ---------------------------------------------------------------------------

// Duration 是配置里的时长，写成带单位的形式：s 秒、m 分、h 时、d 天，
// 支持组合（"1h30m"、"1d12h"）与小数（"0.5h"）。
// 纯数字按秒处理，便于兼容旧配置里的 scan_interval: 300。
type Duration struct {
	duration time.Duration
}

// DurationOf 用 time.Duration 构造配置时长
func DurationOf(d time.Duration) Duration { return Duration{duration: d} }

// Value 返回标准库的 time.Duration
func (d Duration) Value() time.Duration { return d.duration }

// String 返回配置可读的写法（整天的用 d，整小时用 h，其余用标准格式）
func (d Duration) String() string { return FormatDuration(d.duration) }

// UnmarshalYAML 解析时长
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		d.duration = time.Duration(seconds * float64(time.Second))
		return nil
	}
	parsed, err := ParseDuration(raw)
	if err != nil {
		return err
	}
	d.duration = parsed
	return nil
}

// MarshalYAML 输出带单位的写法
func (d Duration) MarshalYAML() (interface{}, error) { return d.String(), nil }

// ParseDuration 解析时长，支持 s/m/h/d 四个单位与组合写法
func ParseDuration(value string) (time.Duration, error) {
	text := strings.ToLower(strings.TrimSpace(value))
	if text == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	for text != "" {
		index := 0
		for index < len(text) && (text[index] == '.' || (text[index] >= '0' && text[index] <= '9')) {
			index++
		}
		if index == 0 {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		number, err := strconv.ParseFloat(text[:index], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		// 允许数字与单位之间留空格，例如 "90 seconds"
		for index < len(text) && (text[index] == ' ' || text[index] == '\t') {
			index++
		}
		unitEnd := index
		for unitEnd < len(text) && !(text[unitEnd] >= '0' && text[unitEnd] <= '9') && text[unitEnd] != '.' {
			unitEnd++
		}
		unit := strings.TrimSpace(text[index:unitEnd])
		var scale time.Duration
		switch unit {
		// 没有单位时按秒处理，兼容 "300" 这种旧写法
		case "", "s", "sec", "secs", "second", "seconds":
			scale = time.Second
		case "m", "min", "mins", "minute", "minutes":
			scale = time.Minute
		case "h", "hr", "hrs", "hour", "hours":
			scale = time.Hour
		case "d", "day", "days":
			scale = 24 * time.Hour
		default:
			return 0, fmt.Errorf("invalid duration unit %q in %q", unit, value)
		}
		total += time.Duration(number * float64(scale))
		text = strings.TrimSpace(text[unitEnd:])
	}
	return total, nil
}

// FormatDuration 把时长格式化成配置里推荐的写法
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

// ByteSize 是配置里的文件体积，支持 "512MB"、"1G"、纯字节数
type ByteSize struct {
	bytes int64
}

// ByteSizeOf 用字节数构造配置体积
func ByteSizeOf(n int64) ByteSize { return ByteSize{bytes: n} }

// Value 返回字节数
func (b ByteSize) Value() int64 { return b.bytes }

// String 返回配置可读的写法
func (b ByteSize) String() string {
	switch {
	case b.bytes >= 1<<30 && b.bytes%(1<<30) == 0:
		return strconv.FormatInt(b.bytes/(1<<30), 10) + "G"
	case b.bytes >= 1<<20 && b.bytes%(1<<20) == 0:
		return strconv.FormatInt(b.bytes/(1<<20), 10) + "M"
	case b.bytes >= 1<<10 && b.bytes%(1<<10) == 0:
		return strconv.FormatInt(b.bytes/(1<<10), 10) + "K"
	default:
		return strconv.FormatInt(b.bytes, 10)
	}
}

// UnmarshalYAML 解析体积
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return nil
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		b.bytes = value
		return nil
	}
	upper := strings.ToUpper(raw)
	units := []struct {
		suffix string
		scale  int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"B", 1},
	}
	for _, unit := range units {
		if !strings.HasSuffix(upper, unit.suffix) {
			continue
		}
		number, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(upper, unit.suffix)), 64)
		if err != nil {
			return fmt.Errorf("invalid size %q", raw)
		}
		if number < 0 || number*float64(unit.scale) > math.MaxInt64 {
			return fmt.Errorf("size out of range: %q", raw)
		}
		b.bytes = int64(number * float64(unit.scale))
		return nil
	}
	return fmt.Errorf("invalid size %q", raw)
}

// MarshalYAML 输出带单位的写法
func (b ByteSize) MarshalYAML() (interface{}, error) { return b.String(), nil }
