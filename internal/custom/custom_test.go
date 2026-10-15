package custom

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ChannelConfig
		wantErr bool
	}{
		{
			name: "valid config with string models",
			cfg: ChannelConfig{
				Name:    "StepFun",
				BaseURL: "https://api.stepfun.com/step_plan/v1/",
				APIKey:  "sk-test",
				Models: []ModelConfig{
					{ID: "step-5-preview"},
					{ID: "step-5-preview"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid config with full model objects",
			cfg: ChannelConfig{
				Name:    "proxycli",
				BaseURL: "http://127.0.0.1:8317/v1",
				Models: []ModelConfig{
					{
						ID:                     "gemini-3.8-flash-high",
						ContextWindow:          1000000,
						MaxTokens:              64000,
						SupportsImages:         true,
						SupportsReasoning:      true,
						ReasoningEfforts:       []string{"low", "medium", "high"},
						DefaultReasoningEffort: "high",
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "empty name",
			cfg:     ChannelConfig{Name: "", BaseURL: "http://127.0.0.1:8000/v1"},
			wantErr: true,
		},
		{
			name:    "invalid name chars",
			cfg:     ChannelConfig{Name: "step/fun", BaseURL: "http://127.0.0.1:8000/v1"},
			wantErr: true,
		},
		{
			name:    "invalid url scheme",
			cfg:     ChannelConfig{Name: "stepfun", BaseURL: "ftp://127.0.0.1:8000/v1"},
			wantErr: true,
		},
		{
			name:    "empty url",
			cfg:     ChannelConfig{Name: "stepfun", BaseURL: ""},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if cfg.Name != strings.ToLower(tc.cfg.Name) {
					t.Errorf("Name should be lowercased, got %q", cfg.Name)
				}
				if strings.HasSuffix(cfg.BaseURL, "/") {
					t.Errorf("BaseURL should strip trailing slash, got %q", cfg.BaseURL)
				}
			}
		})
	}
}

func TestModelConfigPolymorphicJSON(t *testing.T) {
	// 支持简写字符串数组与完整对象数组
	rawJSON := `[
		"step-5-preview",
		{
			"id": "gemini-3.8-flash-high",
			"context_window": 1000000,
			"max_tokens": 64000,
			"supports_images": true,
			"supports_reasoning": true,
			"reasoning_efforts": ["high"],
			"default_reasoning_effort": "high"
		}
	]`
	var models []ModelConfig
	if err := json.Unmarshal([]byte(rawJSON), &models); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "step-5-preview" || models[0].ContextWindow != 0 {
		t.Errorf("model 0 mismatch: %+v", models[0])
	}
	if models[1].ID != "gemini-3.8-flash-high" || models[1].ContextWindow != 1000000 || !models[1].SupportsImages || models[1].DefaultReasoningEffort != "high" {
		t.Errorf("model 1 mismatch: %+v", models[1])
	}
}

func TestFetchModelsReturnsConfiguredVerbatimWithoutUpstreamCallOrInference(t *testing.T) {
	// 配的是什么就返回什么，不上游拉取，不推测
	client := New(ChannelConfig{
		Name:    "stepfun",
		BaseURL: "http://127.0.0.1:9999", // 故意给不存在的端口，证明绝不向上游发网络请求
		Models: []ModelConfig{
			{
				ID:                     "step-5-preview",
				ContextWindow:          1000000,
				MaxTokens:              64000,
				SupportsImages:         true,
				SupportsReasoning:      true,
				ReasoningEfforts:       []string{"low", "medium", "high"},
				DefaultReasoningEffort: "high",
			},
			{
				ID: "custom-plain-model", // 没配的字段如实为 0 / false，绝不推测
			},
		},
	})

	models, err := client.FetchModels(nil)
	if err != nil {
		t.Fatalf("FetchModels failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	// 验证第一款模型：完全符合用户配置
	m1 := models[0]
	if m1.ID != "step-5-preview" || m1.ContextWindow != 1000000 || m1.MaxTokens != 64000 {
		t.Errorf("m1 attributes mismatch: %+v", m1)
	}
	if !m1.SupportsImages || !m1.SupportsReasoning || m1.DefaultReasoningEffort != "high" {
		t.Errorf("m1 capabilities mismatch: %+v", m1)
	}

	// 验证第二款模型：没配的绝不推测
	m2 := models[1]
	if m2.ID != "custom-plain-model" || m2.ContextWindow != 0 || m2.SupportsImages || m2.SupportsReasoning {
		t.Errorf("m2 should not guess fields: %+v", m2)
	}
}

func TestCustomClientChatStreamAndStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := New(ChannelConfig{
		Name:    "mychannel",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	})

	rc, status, _, err := client.ChatStream(nil, []byte(`{"model":"upstream-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	usage, err := client.Stream(rec, rc, "mychannel/upstream-model")
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if usage == nil || usage["prompt_tokens"] != float64(10) {
		t.Errorf("expected usage prompt_tokens 10, got %v", usage)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"model":"mychannel/upstream-model"`) {
		t.Errorf("expected rewritten model name in output, got: %s", body)
	}
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "world") {
		t.Errorf("expected content in output, got: %s", body)
	}
}

func TestCustomClientAggregateNonStreamJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-2",
			"object":  "chat.completion",
			"model":   "upstream-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "non-stream answer"}}},
			"usage":   map[string]any{"prompt_tokens": 8, "completion_tokens": 12, "total_tokens": 20},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := New(ChannelConfig{
		Name:    "mychannel",
		BaseURL: ts.URL,
	})

	rc, status, _, err := client.ChatStream(nil, []byte(`{"model":"upstream-model","stream":false}`))
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	defer rc.Close()

	resp, err := client.Aggregate(rc, "mychannel/upstream-model")
	if err != nil {
		t.Fatalf("Aggregate failed: %v", err)
	}
	if resp["model"] != "mychannel/upstream-model" {
		t.Errorf("expected model mychannel/upstream-model, got %v", resp["model"])
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("expected choices, got %v", resp)
	}
}
