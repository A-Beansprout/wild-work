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
			name: "valid config",
			cfg: ChannelConfig{
				Name:    "StepFun",
				BaseURL: "https://api.stepfun.com/step_plan/v1/",
				APIKey:  "sk-test",
				Models:  []string{"step-5-preview", "step-5-preview"},
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
				if cfg.Name != "stepfun" {
					t.Errorf("Name should be lowercased, got %q", cfg.Name)
				}
				if strings.HasSuffix(cfg.BaseURL, "/") {
					t.Errorf("BaseURL should strip trailing slash, got %q", cfg.BaseURL)
				}
				if len(cfg.Models) != 1 {
					t.Errorf("Models should be deduplicated, got %v", cfg.Models)
				}
			}
		})
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

func TestCustomClientFetchModelsAndTestConnection(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"model-a","max_input_tokens":128000,"enable_vision_input":true},{"id":"model-b"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := New(ChannelConfig{
		Name:    "stepfun",
		BaseURL: ts.URL,
		Models:  []string{"model-c"},
	})

	models, err := client.FetchModels(nil)
	if err != nil {
		t.Fatalf("FetchModels failed: %v", err)
	}
	if len(models) != 3 { // model-a, model-b, and static model-c
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].ID != "model-a" || models[0].ContextWindow != 128000 || !models[0].SupportsImages {
		t.Errorf("model-a attributes mismatch: %+v", models[0])
	}

	testIDs, err := client.TestConnection()
	if err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
	if len(testIDs) != 2 || testIDs[0] != "model-a" {
		t.Errorf("unexpected test IDs: %v", testIDs)
	}
}
