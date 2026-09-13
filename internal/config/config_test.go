package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// server.proxy_path 的规范化与校验
func TestValidateProxyPath(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "空值", input: "", want: ""},
		{name: "根路径等于不启用", input: "/", want: ""},
		{name: "标准前缀", input: "/proxy-index", want: "/proxy-index"},
		{name: "去掉结尾斜杠", input: "/proxy-index/", want: "/proxy-index"},
		{name: "去掉多余斜杠", input: "//proxy-index//", want: "/proxy-index"},
		{name: "缺少前导斜杠", input: "proxy-index", wantErr: true},
		{name: "包含上级目录", input: "/proxy/../index", wantErr: true},
		{name: "包含查询符", input: "/proxy?x=1", wantErr: true},
		{name: "与API冲突", input: "/api", wantErr: true},
		{name: "与API子路径冲突", input: "/api/pdfs", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Server.ProxyPath = tc.input
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("proxy_path = %q: 期望报错，实际通过", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("proxy_path = %q: 未预期的错误 %v", tc.input, err)
			}
			if cfg.Server.ProxyPath != tc.want {
				t.Errorf("proxy_path = %q, 期望 %q", cfg.Server.ProxyPath, tc.want)
			}
		})
	}
}

// 默认值与取值范围校验
func TestDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置校验失败: %v", err)
	}
	if cfg.Documents.ScanInterval.Value() != 5*time.Minute {
		t.Errorf("默认 scan_interval = %v, 期望 5m", cfg.Documents.ScanInterval)
	}
	if cfg.Documents.Retention.Value() != 30*24*time.Hour {
		t.Errorf("默认 retention = %v, 期望 30d", cfg.Documents.Retention)
	}
	if cfg.Documents.MaxFileSize.Value() != 1<<30 {
		t.Errorf("默认 max_file_size = %d, 期望 1G", cfg.Documents.MaxFileSize.Value())
	}
	if cfg.Database.VacuumInterval.Value() != 7*24*time.Hour {
		t.Errorf("默认 vacuum_interval = %v, 期望 7d", cfg.Database.VacuumInterval)
	}
	if cfg.Cache.Active.WatchDebounce.Value() != time.Hour {
		t.Errorf("默认 watch_debounce = %v, 期望 1h", cfg.Cache.Active.WatchDebounce)
	}

	negative := Default()
	negative.Documents.Retention = DurationOf(-time.Hour)
	if err := negative.Validate(); err == nil {
		t.Error("负数的 retention 应当报错")
	}

	zeroInterval := Default()
	zeroInterval.Documents.ScanInterval = DurationOf(0)
	if err := zeroInterval.Validate(); err == nil {
		t.Error("scan_interval 为 0 应当报错")
	}

	zeroSize := Default()
	zeroSize.Documents.MaxFileSize = ByteSizeOf(0)
	if err := zeroSize.Validate(); err == nil {
		t.Error("max_file_size 为 0 应当报错")
	}
}

// 时长支持 s/m/h/d 与组合写法
func TestParseDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"2h", 2 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"0.5h", 30 * time.Minute},
		{"90 seconds", 90 * time.Second},
		{"0", 0},
	}
	for _, tc := range cases {
		got, err := ParseDuration(tc.input)
		if err != nil {
			t.Errorf("ParseDuration(%q) 报错: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDuration(%q) = %v, 期望 %v", tc.input, got, tc.want)
		}
	}

	for _, bad := range []string{"", "abc", "5x", "m5"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) 应当报错", bad)
		}
	}
}

// YAML 里的时长既支持带单位写法，也兼容旧的纯数字（按秒）
func TestDurationUnmarshal(t *testing.T) {
	var cfg struct {
		A Duration `yaml:"a"`
		B Duration `yaml:"b"`
		C Duration `yaml:"c"`
	}
	if err := yaml.Unmarshal([]byte("a: 30s\nb: 300\nc: 7d\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.A.Value() != 30*time.Second {
		t.Errorf("a = %v, 期望 30s", cfg.A)
	}
	if cfg.B.Value() != 300*time.Second {
		t.Errorf("b = %v, 期望 300s（纯数字按秒）", cfg.B)
	}
	if cfg.C.Value() != 7*24*time.Hour {
		t.Errorf("c = %v, 期望 7d", cfg.C)
	}
}

// 体积支持 K/M/G 后缀与纯字节数
func TestByteSizeUnmarshal(t *testing.T) {
	var cfg struct {
		A ByteSize `yaml:"a"`
		B ByteSize `yaml:"b"`
		C ByteSize `yaml:"c"`
		D ByteSize `yaml:"d"`
	}
	if err := yaml.Unmarshal([]byte("a: 512MB\nb: 1G\nc: 2048\nd: 1.5G\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]int64{"a": 512 << 20, "b": 1 << 30, "c": 2048, "d": 3 << 29}
	got := map[string]int64{"a": cfg.A.Value(), "b": cfg.B.Value(), "c": cfg.C.Value(), "d": cfg.D.Value()}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %d, 期望 %d", key, got[key], value)
		}
	}

	var bad struct {
		A ByteSize `yaml:"a"`
	}
	if err := yaml.Unmarshal([]byte("a: 5XB\n"), &bad); err == nil {
		t.Error("非法体积应当报错")
	}
}

// 旧版配置字段要能自动迁移到新结构
func TestLegacyConfigMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	legacy := `
server:
  host: "127.0.0.1"
  port: 9090
proxy:
  base_path: /proxy-index
pdf:
  dirs:
    - /documents
  scan_interval: 60
  retention_days: 7
render:
  jpeg_quality: 70
  scale_width: 1200
  thumb_width: 150
active_cache:
  render_concurrency: 2
  watch_debounce: 2h
  poll_interval: 15s
passive_cache:
  max_size_mb: 512
cache_dir: /data/render-cache
database:
  path: /data/note-web.db
  vacuum_interval: 48h
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if cfg.Server.ProxyPath != "/proxy-index" {
		t.Errorf("proxy_path = %q, 期望 /proxy-index", cfg.Server.ProxyPath)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port = %d, 期望 9090", cfg.Server.Port)
	}
	if len(cfg.Documents.Dirs) != 1 || cfg.Documents.Dirs[0] != "/documents" {
		t.Errorf("dirs = %v", cfg.Documents.Dirs)
	}
	if cfg.Documents.ScanInterval.Value() != time.Minute {
		t.Errorf("scan_interval = %v, 期望 1m（旧值 60 秒）", cfg.Documents.ScanInterval)
	}
	if cfg.Documents.Retention.Value() != 7*24*time.Hour {
		t.Errorf("retention = %v, 期望 7d", cfg.Documents.Retention)
	}
	if cfg.Cache.Render.JpegQuality != 70 || cfg.Cache.Render.ScaleWidth != 1200 {
		t.Errorf("render 未迁移: %+v", cfg.Cache.Render)
	}
	if cfg.Cache.Active.RenderConcurrency != 2 || cfg.Cache.Active.PollInterval.Value() != 15*time.Second {
		t.Errorf("active_cache 未迁移: %+v", cfg.Cache.Active)
	}
	if cfg.Cache.MaxSizeMB != 512 {
		t.Errorf("max_size_mb = %d, 期望 512", cfg.Cache.MaxSizeMB)
	}
	if cfg.Cache.Dir != "/data/render-cache" {
		t.Errorf("cache.dir = %q", cfg.Cache.Dir)
	}
	if cfg.Database.VacuumInterval.Value() != 48*time.Hour {
		t.Errorf("vacuum_interval = %v, 期望 48h", cfg.Database.VacuumInterval)
	}
}

// 新结构配置不应被旧字段影响
func TestNewConfigWinsOverLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mixed := `
server:
  proxy_path: /new-path
proxy:
  base_path: /old-path
documents:
  dirs:
    - /docs
  scan_interval: 2m
  retention: 3d
  max_file_size: 300MB
cache:
  dir: /cache-new
  max_size_mb: 100
  active:
    render_concurrency: 1
    watch_debounce: 5m
    poll_interval: 10s
  render:
    jpeg_quality: 60
    scale_width: 800
    thumb_width: 100
cache_dir: /cache-old
passive_cache:
  max_size_mb: 999
database:
  path: /data/note-web.db
  vacuum_interval: 1d
`
	if err := os.WriteFile(path, []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Server.ProxyPath != "/new-path" {
		t.Errorf("proxy_path = %q, 期望 /new-path", cfg.Server.ProxyPath)
	}
	if cfg.Cache.Dir != "/cache-new" || cfg.Cache.MaxSizeMB != 100 {
		t.Errorf("cache = %+v, 新配置应优先", cfg.Cache)
	}
	if cfg.Documents.MaxFileSize.Value() != 300<<20 {
		t.Errorf("max_file_size = %d, 期望 300MB", cfg.Documents.MaxFileSize.Value())
	}
	if cfg.Database.VacuumInterval.Value() != 24*time.Hour {
		t.Errorf("vacuum_interval = %v, 期望 1d", cfg.Database.VacuumInterval)
	}
}
