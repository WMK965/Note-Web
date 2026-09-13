package config

import (
	"testing"
	"time"
)

// 新增的清理与 VACUUM 配置：默认值、解析回退与校验
func TestMaintenanceDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	if cfg.PDF.RetentionDays != 30 {
		t.Errorf("默认 retention_days = %d, 期望 30", cfg.PDF.RetentionDays)
	}
	if got := cfg.PDF.RetentionDuration(); got != 30*24*time.Hour {
		t.Errorf("RetentionDuration() = %v, 期望 720h", got)
	}
	if cfg.Database.VacuumInterval != "168h" {
		t.Errorf("默认 vacuum_interval = %q, 期望 168h", cfg.Database.VacuumInterval)
	}
	if got := cfg.Database.VacuumDuration(); got != 7*24*time.Hour {
		t.Errorf("VacuumDuration() = %v, 期望 168h", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置校验失败: %v", err)
	}

	zero := Default()
	zero.PDF.RetentionDays = 0
	if got := zero.PDF.RetentionDuration(); got != 0 {
		t.Errorf("retention_days=0 时 RetentionDuration() = %v, 期望 0", got)
	}
	if err := zero.Validate(); err != nil {
		t.Errorf("retention_days=0 应当合法: %v", err)
	}

	negative := Default()
	negative.PDF.RetentionDays = -1
	if err := negative.Validate(); err == nil {
		t.Error("retention_days 为负数时应当报错")
	}

	badInterval := Default()
	badInterval.Database.VacuumInterval = "7d" // Go duration 不支持天
	if err := badInterval.Validate(); err == nil {
		t.Error("无法解析的 vacuum_interval 应当报错")
	}

	fallback := DatabaseConfig{VacuumInterval: "not-a-duration"}
	if got := fallback.VacuumDuration(); got != 7*24*time.Hour {
		t.Errorf("解析失败时 VacuumDuration() = %v, 期望回退为 168h", got)
	}
}

// proxy.base_path 的规范化与校验
func TestValidateProxyBasePath(t *testing.T) {
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
			cfg.Proxy.BasePath = tc.input
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("base_path = %q: 期望报错，实际通过", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("base_path = %q: 未预期的错误 %v", tc.input, err)
			}
			if cfg.Proxy.BasePath != tc.want {
				t.Errorf("base_path = %q, 期望 %q", cfg.Proxy.BasePath, tc.want)
			}
		})
	}
}
