package cdp

// capture_test.go 锁定捕获层的纯逻辑：回环匹配器（五渠道的 path 前缀各不相同，
// host 形态有 127.0.0.1/localhost/[::1] 三种）与超时错误语义。
// WebSocket 事件泵的挂起/泄漏由 hang_test.go 的既有手法覆盖（本文件不重复）。

import (
	"errors"
	"testing"
	"time"
)

func TestLoopbackCallbackMatcher(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		raw    string
		want   bool
	}{
		{"traework 回调命中", "/authorize", "http://127.0.0.1:52883/authorize?isRedirect=true&authCodeInfo=%7B%7D", true},
		{"traework 端口不符仍命中（端口不是匹配器职责，会话自检在渠道层）", "/authorize", "http://127.0.0.1:9999/authorize?x=1", true},
		{"codearts 命中", "/oauth/callback", "http://127.0.0.1:18080/oauth/callback?code=abc", true},
		{"lobsterai 命中", "/auth/callback", "http://localhost:38055/auth/callback?code=abc&state=s", true},
		{"qwenwork 命中", "/callback", "http://127.0.0.1:8080/callback?code=abc", true},
		{"gemini localhost 命中", "/oauth/callback", "http://localhost:18090/oauth/callback?code=abc&state=s", true},
		{"gemini IPv6 命中（localhost 解析漂移坑）", "/oauth/callback", "http://[::1]:18090/oauth/callback?code=abc", true},
		{"非回环拒绝（上游域）", "/authorize", "https://www.trae.cn/authorization?x=1", false},
		{"非回环拒绝（局域网 IP）", "/callback", "http://10.127.127.10:8080/callback?code=abc", false},
		{"path 不匹配拒绝", "/authorize", "http://127.0.0.1:52883/other?x=1", false},
		{"path 前缀过界拒绝", "/callbackx", "http://127.0.0.1:8080/callback?code=abc", false},
		{"宽松模式（空前缀只看 host）", "", "http://127.0.0.1:1/anything", true},
		{"宽松模式仍拒绝非回环", "", "http://10.0.0.5/anything", false},
		{"坏 URL 拒绝", "/authorize", "://bad url", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := LoopbackCallbackMatcher(c.prefix)
			if got := m(c.raw); got != c.want {
				t.Fatalf("LoopbackCallbackMatcher(%q)(%q) = %v, want %v", c.prefix, c.raw, got, c.want)
			}
		})
	}
}

func TestCaptureTimeoutErrorIs(t *testing.T) {
	var err error = &CaptureTimeoutError{Timeout: 5 * time.Minute}
	if !errors.Is(err, ErrCaptureTimeout) {
		t.Fatal("CaptureTimeoutError 应可被 errors.Is(ErrCaptureTimeout) 识别")
	}
	if got := err.Error(); got == "" {
		t.Fatal("错误文案不得为空")
	}
}

func TestCaptureOptionsDefaults(t *testing.T) {
	// Match 为空必须立刻报错（fail early，不得静默等到超时）。
	b := &Browser{}
	if _, err := b.CaptureAndWait(t.Context(), "about:blank", CaptureOptions{}); err == nil {
		t.Fatal("Match 为空应报错")
	}
}
