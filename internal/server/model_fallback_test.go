// 模型降级链行为锁定：主模型全冷 → 按链顶备胎（X-Model-Fallback 标注）；
// 未全冷不触发；链按顺序找第一个有容量的备胎；无规则保持原 503 行为。
// 背景：2026-10-13 workbuddyai/deepseek-v4.1-flash 时段性平台频控（14003）把
// 3 个账号全部打进模型级冷却，fail-fast 503 打断用户任务——本机制让用户可
// 预配置备胎链自动顶上，上游解冻后按请求自动切回（不粘滞）。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newFbHandler 装配两个渠道（oczen=主、workbuddy=备）的 handler，各一个账号。
func newFbHandler(t *testing.T, upA, upB *codeUpstream, rules []config.ModelFallbackRule) (*Handler, *pool.Pool, *pool.Pool) {
	t.Helper()
	pa := pool.New(t.TempDir() + "/state-a.json")
	pa.Add(oczen.AnonymousAuth())
	pb := pool.New(t.TempDir() + "/state-b.json")
	pb.Add(&auth.Auth{UID: "backup-uid"})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen:     {Kind: provider.Oczen, Pool: pa, Upstream: upA},
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: pb, Upstream: upB},
		},
		APIKey: "k", MaxRotate: 3,
		HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
		ModelFallbacks: rules,
	})
	return h, pa, pb
}

// callModel 发一次非流式 chat 请求（可指定模型），返回响应记录。
func callModel(h *Handler, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestModelFallbackTriggersWhenAllCooling 主模型全冷 → 备胎顶上：上游 B 收到请求、
// 响应 200、带 X-Model-Fallback 标注。
func TestModelFallbackTriggersWhenAllCooling(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	// 主渠道唯一账号对 big-pickle 模型级冷却（账号本身健康）
	// （返回值=是否升级为整号冷却，首次调用恒 false，不校验）
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("应命中降级链返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want oczen/big-pickle -> workbuddy/glm-free", got)
	}
	if upB.calls != 1 {
		t.Errorf("备胎渠道上游收到 %d 次请求，want 1", upB.calls)
	}
	if upA.calls != 0 {
		t.Errorf("主渠道上游不应收到请求（挑号阶段已全冷），实际 %d 次", upA.calls)
	}
}

// TestModelFallbackNotTriggeredWhenHealthy 未全冷 → 走原模型，不降级、无标注头。
func TestModelFallbackNotTriggeredWhenHealthy(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("健康时应正常返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Model-Fallback") != "" {
		t.Error("健康时不应有 X-Model-Fallback 头")
	}
	if upA.calls != 1 || upB.calls != 0 {
		t.Errorf("upA=%d upB=%d，want 1/0（未降级）", upA.calls, upB.calls)
	}
}

// TestModelFallbackChainOrder 链内按顺序找第一个有容量的备胎：
// 第一个备胎也在备胎渠道全冷 → 自动落到第二个。
func TestModelFallbackChainOrder(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, pb := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/m-cool", "workbuddy/glm-free"}},
	})
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")
	pb.CooldownModel("backup-uid", "m-cool", 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("链内应落到第二个备胎，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want 第二个备胎 glm-free", got)
	}
}

// TestModelFallbackNoRuleKeeps503 全冷但无规则 → 保持原 no_healthy_account 503 行为。
func TestModelFallbackNoRuleKeeps503(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, nil)
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无规则时应保持 503，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("应保持 no_healthy_account 语义：%s", rec.Body.String())
	}
}

// TestModelFallbackUnfreezeAutoSwitchBack 不粘滞：冷却过期后自动切回原模型
//（下一个请求按原模型重新判定，不再碰备胎）。
func TestModelFallbackUnfreezeAutoSwitchBack(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	// 冷却 50ms：先触发降级，随后过期自动切回
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 50*time.Millisecond, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Header().Get("X-Model-Fallback") == "" {
		t.Fatalf("冷却期内应降级，实际未降级：%d", rec.Code)
	}
	time.Sleep(80 * time.Millisecond)
	rec2 := callModel(h, "oczen/big-pickle")
	if rec2.Header().Get("X-Model-Fallback") != "" {
		t.Error("冷却过期后仍降级——违反「不粘滞、自动切回」契约")
	}
	if upA.calls != 1 {
		t.Errorf("解冻后原模型上游应收到 1 次请求，实际 %d", upA.calls)
	}
}

// TestModelFallbackFromOtherChannelIgnores 规则 From 渠道不匹配时忽略
//（oczen 的全冷不吃 workbuddy 规则，反之亦然）。
func TestModelFallbackFromOtherChannelIgnores(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "workbuddy/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("From 渠道不匹配的规则不应触发，应保持 503，实际 %d", rec.Code)
	}
}

// TestSetModelFallbacksHotReload 热更新：SetModelFallbacks 后立即按新链路由
//（无重启；面板保存即生效的契约）。
func TestSetModelFallbacksHotReload(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, nil)
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")
	if rec := callModel(h, "oczen/big-pickle"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无规则时应 503，实际 %d", rec.Code)
	}
	h.SetModelFallbacks([]config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Model-Fallback") == "" {
		t.Fatalf("热更新后应立即降级生效，实际 %d：%s", rec.Code, rec.Body.String())
	}
}
