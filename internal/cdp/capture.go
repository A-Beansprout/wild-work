// capture.go CDP 被动捕获回环回调导航（登录浏览器化改造的核心，见
// docs/全渠道登录授权方式梳理.md §4）。
//
// 原理：回环回调渠道（traework/codearts/lobsterai/qwenwork/gemini）的授权页最后一步
// 是把浏览器 302 到 `http://127.0.0.1:<port>/...`，**凭据已在 URL query 里**；浏览器
// 能否连上该端口上游不感知。故在网关机拉起 CDP 浏览器打开授权页后，只需监听导航事件、
// 从 URL 提取凭据，**无需任何端口监听**。
//
// 为什么是被动监听（requestWillBeSent）而不是 Fetch.requestPaused 主动拦截：
// 被动捕获不触碰请求生命周期，零版本兼容风险；代价只是浏览器闪一下「连接拒绝」页，
// 随即由本包 Navigate 到成功页安抚。主动拦截是纯增复杂度，明确不做（同 §4.3）。
//
// 与既有链路的关系：捕获结果写回各渠道既有 state/session（与 HTTP 监听器、
// SubmitCallbackURL 三者同一条结论通道），Poll 兑换路径零分叉。
package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// CaptureOptions 一次回环回调捕获的参数。
type CaptureOptions struct {
	// Match 判定该导航是否是回调。收到true即停（通常判 host=127.0.0.1/localhost + path 前缀）。
	// raw 是完整导航 URL（CDP 原文，未解码）。
	Match func(raw string) bool
	// Timeout 整个捕获的时长上限；到点返回 ErrCaptureTimeout。
	Timeout time.Duration
}

// ErrCaptureTimeout 捕获超时（用户未在时限内完成授权，或回调形状不匹配）。
type CaptureTimeoutError struct{ Timeout time.Duration }

func (e *CaptureTimeoutError) Error() string {
	return fmt.Sprintf("cdp: %.0f 分钟内未捕获到授权回调（请在登录窗口完成授权）", e.Timeout.Minutes())
}

// Is 支撑 errors.Is(err, ErrCaptureTimeout)——同类型即命中（哨兵仅作类型锚点）。
func (e *CaptureTimeoutError) Is(target error) bool {
	_, ok := target.(*CaptureTimeoutError)
	return ok
}

// ErrCaptureTimeout 的哨兵，供 errors.Is 判定（与渠道包 ErrPending 同款习惯）。
var ErrCaptureTimeout = &CaptureTimeoutError{}

// CaptureAndWait 打开目标 URL 并等待匹配的导航出现（阻塞）。
//
// 流程：Launch（调用方持有 Browser 负责回收）→ 在**初始 tab** 上接 WebSocket →
// Network.enable → 循环等 requestWillBeSent → Match(raw) 命中即返回该 URL。
// 本函数**不关闭浏览器**：调用方拿到 URL 后通常还要 Navigate 成功页再回收。
//
// 为什么监听浏览器级而非页面级：初始 tab 的 WS 就够（授权全程同 tab 重定向，
// 五渠道均无 window.open 弹窗回调——若有，该渠道退回 HTTP 监听器模式）。
// 这里仍用 pageWSURL 拿第一个 page target，行为等价且复用既有代码。
func (b *Browser) CaptureAndWait(ctx context.Context, openURL string, opt CaptureOptions) (string, error) {
	if opt.Match == nil {
		return "", fmt.Errorf("cdp: CaptureOptions.Match 不能为空")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	dlCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	wsURL, err := b.pageWSURL(dlCtx, "") // 初始 tab 就是授权页（Launch StartURL）
	if err != nil {
		return "", fmt.Errorf("cdp: 找不到授权页 target: %w", err)
	}
	ws, err := DialWebSocket(wsURL, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("cdp: 连接授权页失败: %w", err)
	}
	defer ws.Close()

	// 命中通道：onEvent 回调在 readLoop goroutine 里触发，经 channel 交给本 goroutine 判定。
	// 缓冲 1：命中后 readLoop 不再阻塞。
	hitCh := make(chan string, 1)
	ws.onEvent = func(method string, params json.RawMessage) {
		if method != "Network.requestWillBeSent" {
			return
		}
		var ev struct {
			Request struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		if opt.Match(ev.Request.URL) {
			select {
			case hitCh <- ev.Request.URL:
			default:
			}
		}
	}
	if _, err := ws.Call("Network.enable", nil, 10*time.Second); err != nil {
		return "", fmt.Errorf("cdp: Network.enable 失败: %w", err)
	}

	// 等待三路退出：命中 / ctx 取消（用户关闭登录） / 超时。
	// 注：ws.Call 阻塞在 pending map，事件回调不受影响，无需额外泵。
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case raw := <-hitCh:
			return raw, nil
		case <-dlCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err() // 外层取消（登录关闭）优先透传
			}
			return "", &CaptureTimeoutError{Timeout: timeout}
		case <-timer.C:
			return "", &CaptureTimeoutError{Timeout: timeout}
		}
	}
}

// Navigate 让初始 tab 跳到指定 URL（捕获命中后展示成功页用；也用于打开授权页重试）。
func (b *Browser) Navigate(ctx context.Context, rawURL string) error {
	wsURL, err := b.pageWSURL(ctx, "")
	if err != nil {
		return err
	}
	ws, err := DialWebSocket(wsURL, 10*time.Second)
	if err != nil {
		return err
	}
	defer ws.Close()
	_, err = ws.Call("Page.navigate", map[string]any{"url": rawURL}, 10*time.Second)
	return err
}

// LoopbackCallbackMatcher 生成回环回调匹配器：host ∈ {127.0.0.1, localhost, ::1}
// 且 path 前缀匹配（path 前缀比对的是**解码后**的 path，query 不参与）。
// prefix 如 "/authorize"（traework）、"/oauth/callback"（codearts/gemini）、
// "/auth/callback"（lobsterai）、"/callback"（qwenwork）。
// pathPrefix 为空 = 只看 host（宽松模式，仅测试用）。
func LoopbackCallbackMatcher(pathPrefix string) func(raw string) bool {
	return func(raw string) bool {
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		host := strings.ToLower(u.Hostname())
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "[::1]" {
			return false
		}
		if pathPrefix == "" {
			return true
		}
		return strings.HasPrefix(u.Path, pathPrefix)
	}
}
