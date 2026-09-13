package config

import "testing"

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
