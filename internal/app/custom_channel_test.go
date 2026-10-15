package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

func TestCustomChannelsAPIAndHotReload(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)

	a, err := New(Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   map[provider.Kind]*Runtime{},
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	var synced []custom.ChannelConfig
	a.SetCustomChannelsSyncer(func(channels []custom.ChannelConfig) {
		synced = channels
	})

	mux := http.NewServeMux()
	a.HandleAPI(mux)

	// 1. 测试 POST /api/config/custom_channel_test（假上游）
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"step-5-preview"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	testReq := map[string]any{
		"name":     "stepfun",
		"base_url": ts.URL,
		"api_key":  "test-key",
	}
	rawTest, _ := json.Marshal(testReq)
	req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(rawTest))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("custom_channel_test status %d: %s", w.Code, w.Body.String())
	}
	var testResp struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &testResp); err != nil || !testResp.OK {
		t.Fatalf("expected ok true with models, got: %s", w.Body.String())
	}
	if len(testResp.Models) != 1 || testResp.Models[0] != "step-5-preview" {
		t.Errorf("unexpected models: %v", testResp.Models)
	}

	// 2. 测试 POST /api/config/custom_channels 保存配置与触发回调
	saveReq := map[string]any{
		"channels": []map[string]any{
			{
				"name":     "stepfun",
				"base_url": ts.URL,
				"api_key":  "key1",
				"models":   []string{"step-5-preview"},
			},
		},
	}
	rawSave, _ := json.Marshal(saveReq)
	reqSave := httptest.NewRequest("POST", "/api/config/custom_channels", bytes.NewReader(rawSave))
	wSave := httptest.NewRecorder()
	mux.ServeHTTP(wSave, reqSave)
	if wSave.Code != 200 {
		t.Fatalf("custom_channels save status %d: %s", wSave.Code, wSave.Body.String())
	}
	if len(synced) != 1 || synced[0].Name != "stepfun" {
		t.Fatalf("expected custom channels syncer called with stepfun, got: %+v", synced)
	}

	// 3. 模拟 main.go 装配 custom runtimes 并测试 accountViews
	p := pool.New(tmpDir + "/state-custom.json")
	cClient := custom.New(synced[0])
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("stepfun"): {
			Kind:     provider.Kind("stepfun"),
			Pool:     p,
			Upstream: cClient,
			Custom:   true,
		},
	})

	if !a.creditsNAKind("stepfun") {
		t.Errorf("expected stepfun to be CreditsNA")
	}
	if !a.noExplicitCheckin(provider.Kind("stepfun")) {
		t.Errorf("expected stepfun to be noExplicitCheckin")
	}

	// 4. 测试 GET /api/state 包含 custom_channels
	st := a.GetState()
	if len(st.CustomChannels) != 1 || st.CustomChannels[0].Name != "stepfun" {
		t.Errorf("expected CustomChannels in state, got %+v", st.CustomChannels)
	}
}
