// Package custom 实现自定义 OpenAI 兼容第三方渠道（R64）。
//
// 目标：允许用户在面板配置任意 OpenAI 兼容上游（如官方 StepFun、本地 CLIProxy、
// DeepSeek、SiliconFlow 等），由本工具统一进行模型名前缀路由（channel/model），
// 统一记录用量流水（ledger）与运行统计（stats，含 TTFB、耗时、吞吐与请求日志）。
package custom

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var channelNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// ChannelConfig 自定义 OpenAI 兼容渠道配置。
type ChannelConfig struct {
	Name    string   `json:"name"`              // 渠道标识/模型前缀，如 "stepfun", "proxycli", "cliproxy"
	BaseURL string   `json:"base_url"`          // API 基础地址，如 "https://api.stepfun.com/step_plan/v1"
	APIKey  string   `json:"api_key,omitempty"` // API 密钥（可选）
	Models  []string `json:"models,omitempty"`  // 预配置/首选模型名列表（可选，留空从 /models 自动拉取）
	Enabled *bool    `json:"enabled,omitempty"` // 是否启用（默认 true）
}

// IsEnabled 报告该渠道是否启用（nil 视为 true）。
func (c ChannelConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Validate 校验配置合法性并规整字段。
func (c *ChannelConfig) Validate() error {
	c.Name = strings.TrimSpace(strings.ToLower(c.Name))
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.APIKey = strings.TrimSpace(c.APIKey)

	if c.Name == "" {
		return fmt.Errorf("channel name cannot be empty")
	}
	if !channelNameRegex.MatchString(c.Name) {
		return fmt.Errorf("channel name %q contains invalid characters (only letters, numbers, _, - allowed)", c.Name)
	}
	if c.BaseURL == "" {
		return fmt.Errorf("channel %s: base_url cannot be empty", c.Name)
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("channel %s: invalid base_url %q (must be http:// or https://)", c.Name, c.BaseURL)
	}

	var cleanModels []string
	seen := map[string]bool{}
	for _, m := range c.Models {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			cleanModels = append(cleanModels, m)
		}
	}
	c.Models = cleanModels
	return nil
}
