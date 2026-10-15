// Package custom 实现自定义 OpenAI 兼容第三方渠道（R64）。
//
// 目标：允许用户在面板配置任意 OpenAI 兼容上游（如官方 StepFun、本地 CLIProxy、
// DeepSeek、SiliconFlow 等），由本工具统一进行模型名前缀路由（channel/model），
// 统一记录用量流水（ledger）与运行统计（stats，含 TTFB、耗时、吞吐与请求日志）。
//
// 原则（用户裁定）：
//   - 配什么，/v1/models 就返回什么；
//   - 绝不向上游 GET /models 发起拉取；
//   - 绝不做任何启发式推测。
package custom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var channelNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// ModelConfig 单个模型详细配置（配什么返回什么，不推测，不上游拉取）。
type ModelConfig struct {
	ID                     string   `json:"id"`
	ContextWindow          int64    `json:"context_window,omitempty"`          // 上下文大小（tokens），0 = 未设置
	MaxTokens              int64    `json:"max_tokens,omitempty"`              // 最大输出 tokens，0 = 未设置
	SupportsImages         bool     `json:"supports_images,omitempty"`         // 是否支持视觉/多模态
	SupportsReasoning      bool     `json:"supports_reasoning,omitempty"`      // 是否支持思考/推理
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`       // 思考档位，如 ["low", "medium", "high"]
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`// 默认思考档位，如 "high"
}

// UnmarshalJSON 兼容简写字符串（如 "step-5-preview"）与完整 JSON 对象。
func (m *ModelConfig) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		m.ID = strings.TrimSpace(s)
		return nil
	}
	type alias ModelConfig
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	a.ID = strings.TrimSpace(a.ID)
	*m = ModelConfig(a)
	return nil
}

// ChannelConfig 自定义 OpenAI 兼容渠道配置。
type ChannelConfig struct {
	Name    string        `json:"name"`              // 渠道标识/模型前缀，如 "stepfun", "proxycli", "cliproxy"
	BaseURL string        `json:"base_url"`          // API 基础地址，如 "https://api.stepfun.com/step_plan/v1"
	APIKey  string        `json:"api_key,omitempty"` // API 密钥（可选）
	Models  []ModelConfig `json:"models,omitempty"`  // 用户配置的模型列表（配什么，/v1/models 就返回什么）
	Enabled *bool         `json:"enabled,omitempty"` // 是否启用（默认 true）
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

	var cleanModels []ModelConfig
	seen := map[string]bool{}
	for _, m := range c.Models {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			cleanModels = append(cleanModels, m)
		}
	}
	c.Models = cleanModels
	return nil
}
