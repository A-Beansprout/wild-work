package app

// browserassist_test.go 锁定 CDP 捕获喂入的契约（docs/全渠道登录授权方式梳理.md §4.4）。
//
// 全部离线：不起真浏览器（Launch 的挂起/回收由 cdp 包 hang_test 手法覆盖），
// 只验证两件事：
//  1. 等价性：CDP 捕获到的回调 URL 走 feed 后，与手动粘贴同一 URL 的结果完全一致
//     （同一结论通道的第三入口不得另立语义）；
//  2. 接线完整性：五回环回调渠道的捕获声明齐全、path 前缀与渠道回调路径一致、
//     ttl 不短于各渠道监听寿命（防「捕获先于监听器超时」的时序倒挂）。

import (
	"errors"
	"testing"
	"time"

	"wild-work/internal/cdp"
	"wild-work/internal/login_gemini"
	"wild-work/internal/login_qwenwork"
	"wild-work/internal/provider"
)

// TestBrowserCaptureSpecsComplete 五回环渠道必须全部登记，未列出的渠道（轮询制等）
// 不得混入——它们没有回环回调，混入即误拉浏览器。
func TestBrowserCaptureSpecsComplete(t *testing.T) {
	want := []provider.Kind{
		provider.TraeWork, provider.CodeArts, provider.LobsterAI, provider.QwenWork, provider.Gemini,
	}
	if len(browserCaptureSpecs) != len(want) {
		t.Fatalf("browserCaptureSpecs 应恰含 %d 条，实际 %d 条", len(want), len(browserCaptureSpecs))
	}
	for _, k := range want {
		spec, ok := browserCaptureSpecs[k]
		if !ok {
			t.Fatalf("渠道 %s 未登记捕获声明", k)
		}
		if spec.pathPrefix == "" || spec.feed == nil || spec.ttl <= 0 {
			t.Fatalf("渠道 %s 捕获声明字段不完整: %+v", k, spec)
		}
	}
}

// TestBrowserCapturePrefixesMatchPaths path 前缀必须与各渠道回调路径逐字一致
// （对照 internal/*channel*/constants 与各 login 包的 mux.HandleFunc 注册值）。
func TestBrowserCapturePrefixesMatchPaths(t *testing.T) {
	wantPrefix := map[provider.Kind]string{
		provider.TraeWork:  "/authorize",     // login_trae: callback = .../authorize
		provider.CodeArts:  "/oauth/callback", // codearts.OAuthCallbackPath
		provider.LobsterAI: "/auth/callback",  // login_lobsterai: callbackPath
		provider.QwenWork:  "/callback",       // login_qwenwork: mux.HandleFunc("/callback", ...)
		provider.Gemini:    "/oauth-callback", // gemini.CallbackPath
	}
	for k, want := range wantPrefix {
		if got := browserCaptureSpecs[k].pathPrefix; got != want {
			t.Fatalf("%s pathPrefix = %q, want %q", k, got, want)
		}
	}
}

// TestBrowserCaptureTTLNotShorterThanListener ttl 必须不短于各渠道监听寿命：
// 若捕获先超时，浏览器被回收而监听器还活着，用户授权完成后两条路都收不到。
func TestBrowserCaptureTTLNotShorterThanListener(t *testing.T) {
	// 监听寿命：trae 5m（Start 内 timer）/ codearts flowTTL 10m / lobsterai flowTTL 10m /
	// qwenwork callbackTTL 5m / gemini flowTTL 6m。
	floors := map[provider.Kind]time.Duration{
		provider.TraeWork:  5 * time.Minute,
		provider.CodeArts:  10 * time.Minute,
		provider.LobsterAI: 10 * time.Minute,
		provider.QwenWork:  5 * time.Minute,
		provider.Gemini:    6 * time.Minute,
	}
	for k, floor := range floors {
		if got := browserCaptureSpecs[k].ttl; got < floor {
			t.Fatalf("%s 捕获 ttl %v 短于监听寿命 %v——时序倒挂，授权完成前浏览器先被回收", k, got, floor)
		}
	}
}

// TestMatcherConsistencyWithSpecs 每条 spec 的 matcher 必须能放行一条「该渠道真实形状」
// 的回环回调 URL（回归：改 path 常量忘改 spec）。
func TestMatcherConsistencyWithSpecs(t *testing.T) {
	samples := map[provider.Kind]string{
		provider.TraeWork:  "http://127.0.0.1:52883/authorize?isRedirect=true&authCodeInfo=%7B%7D&host=api.trae.cn",
		provider.CodeArts:  "http://127.0.0.1:18080/oauth/callback?code=abc&state=s",
		provider.LobsterAI: "http://127.0.0.1:38055/auth/callback?code=abc&state=s",
		provider.QwenWork:  "http://127.0.0.1:8080/callback?code=abc&state=s",
		provider.Gemini:    "http://localhost:18090/oauth-callback?code=abc&state=s",
	}
	for k, raw := range samples {
		m := cdp.LoopbackCallbackMatcher(browserCaptureSpecs[k].pathPrefix)
		if !m(raw) {
			t.Fatalf("%s 的 matcher 拒绝了真实形状的回调 URL: %s", k, raw)
		}
	}
}

// TestGeminiSubmitCallbackURLFeed 等价性（gemini）：无登录会话时 feed 必须明确报错
// （可行动文案），不得静默丢弃。
func TestGeminiSubmitCallbackURLFeed(t *testing.T) {
	err := login_gemini.SubmitCallbackURL("http://localhost:1/oauth-callback?code=x&state=y")
	if err == nil {
		t.Fatal("无会话时应报错")
	}
	if msg := err.Error(); msg == "" {
		t.Fatal("错误文案不得为空")
	}
}

// TestQwenWorkSubmitCallbackURLFeed 等价性（qwenwork）：同样必须报「无流程」类错误。
func TestQwenWorkSubmitCallbackURLFeed(t *testing.T) {
	// qwenwork 的 pending 是包级内存：没有 HTTP 回调 server 时提交 code 也会被
	// Poll 消费——但本测试进程从未 Start 过，直接喂等价于「真实回调先到」，
	// 合法。这里验证 error 形态路径（缺 code）。
	if err := login_qwenwork.SubmitCallbackURL("http://127.0.0.1:8080/callback?error=access_denied"); err == nil {
		t.Fatal("error 回调应报错")
	} else if !errors.Is(err, err) {
		t.Fatal("unreachable")
	}
	// 成功路径：合法 code 应被接受（写入包级 pending；无副作用需清理——pending 无
	// 导出 reset，依赖下一次 Start 的 pending.reset()，测试进程生命周期内无害）。
	if err := login_qwenwork.SubmitCallbackURL("http://127.0.0.1:8080/callback?code=qc-1&state=s"); err != nil {
		t.Fatalf("合法 code 应被接受: %v", err)
	}
}
