// browserassist.go CDP 浏览器化登录：在网关机拉起独立 profile 浏览器完成授权，
// 被动捕获回环回调导航并喂给各渠道既有的结论通道（docs/全渠道登录授权方式梳理.md §4）。
//
// 为什么是「钩子」而不是改 Start/Poll：五个回环渠道的结论通道各不相同
// （trae=state 文件 / codearts&lobster&gemini=内存 session / qwenwork=内存 pending），
// 但它们都已是「多生产者」设计（HTTP 监听器 + SubmitCallbackURL 双入口）。
// 捕获作为第三个生产者接入，兑换/落盘路径零分叉；Start/Poll/监听器一行不改，
// 本机用户走默认浏览器（window.open）的路径完全不受影响。
//
// 生命周期：由 StartLoginFor 在 go pollLogin 之前异步启动；Shutdown 统一回收
// （browserCancel 通道 + Browser.Close，幂等）。浏览器与监听器并存——
// 本机用户窗口开浏览器直连监听器照样通，两路谁先到谁生效（保留先到者语义）。
package app

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/cdp"
	logincodearts "wild-work/internal/login_codearts"
	logingemini "wild-work/internal/login_gemini"
	loginlobsterai "wild-work/internal/login_lobsterai"
	loginqwenwork "wild-work/internal/login_qwenwork"
	logintrae "wild-work/internal/login_trae"
	"wild-work/internal/provider"
)

// browserAssist 一次 CDP 浏览器登录会话。
type browserAssist struct {
	mu      sync.Mutex
	browser *cdp.Browser
	cancel  context.CancelFunc
	done    chan struct{}
	kind    provider.Kind
}

// browserSess 当前会话（同一时刻至多一条登录流程，由 loginBusy 保证；防御性互斥）。
var (
	browserMu   sync.Mutex
	browserSess *browserAssist
)

// startBrowserAssist 拉起 CDP 浏览器并开始捕获。
//
// flow 描述该渠道的捕获参数（回调匹配前缀 + 凭据喂入函数）。阻塞直到
// 捕获命中 / 用户取消 / 超时；成功页展示后自动回收浏览器。
// 独立 goroutine 运行，失败只记日志——浏览器不可用不阻断既有监听器路径。
func startBrowserAssist(kind provider.Kind, authURL string, match func(raw string) bool, feed func(rawURL string), timeout time.Duration) {
	browserMu.Lock()
	if browserSess != nil {
		browserMu.Unlock() // 已有会话：loginBusy 已挡住，防御性忽略
		return
	}
	s := &browserAssist{done: make(chan struct{}), kind: kind}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	browserSess = s
	browserMu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			b := s.browser
			s.browser = nil
			s.mu.Unlock()
			if b != nil {
				_ = b.Close()
			}
			cancel()
			close(s.done)
			browserMu.Lock()
			if browserSess == s {
				browserSess = nil
			}
			browserMu.Unlock()
		}()

		b, err := cdp.Launch(ctx, cdp.Options{StartURL: authURL})
		if err != nil {
			log.Printf("%s CDP 登录浏览器启动失败（既有监听器路径不受影响）: %v", kind, err)
			return
		}
		s.mu.Lock()
		s.browser = b
		s.mu.Unlock()
		log.Printf("%s CDP 登录窗口已打开（网关机独立 profile），等待授权回调捕获…", kind)

		raw, cerr := b.CaptureAndWait(ctx, authURL, cdp.CaptureOptions{Match: match, Timeout: timeout})
		if cerr != nil {
			if ctx.Err() != nil {
				log.Printf("%s CDP 登录已取消", kind)
				return
			}
			log.Printf("%s CDP 回调捕获未完成: %v（既有监听器路径仍可用/可粘贴兜底）", kind, cerr)
			return
		}
		// 命中：喂给渠道结论通道（内部自带 state/session 校验与先到保留语义）。
		feed(raw)
		// 成功页安抚：回调地址打不开（无监听端口场景）会显示连接拒绝，导航到本地成功页。
		successURL := "data:text/html;charset=utf-8," + strings.ReplaceAll(callbackSuccessHTML, "#", "%23")
		if nerr := b.Navigate(context.Background(), successURL); nerr != nil {
			log.Printf("%s CDP 成功页导航失败（不影响登录）: %v", kind, nerr)
		}
		// 给用户 1.5s 看成功页再回收浏览器（对齐各渠道回调页 1.5s 自动关的节奏）。
		time.Sleep(1500 * time.Millisecond)
	}()
}

// stopBrowserAssist 收掉当前 CDP 会话（幂等；Shutdown 链调用）。
func stopBrowserAssist() {
	browserMu.Lock()
	s := browserSess
	browserMu.Unlock()
	if s == nil {
		return
	}
	s.cancel() // 令 CaptureAndWait 退出；goroutine defer 负责关浏览器与清 sess
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		log.Printf("browser assist 关闭超时（不强杀，随进程退出回收）")
	}
}

// callbackSuccessHTML 捕获成功后的安抚页（与各渠道 callbackPage 同款视觉约定，精简版）。
const callbackSuccessHTML = `<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>授权完成</title>` +
	`<style>body{display:flex;align-items:center;justify-content:center;height:100vh;margin:0;` +
	`font-family:system-ui,sans-serif;background:#0b1220;color:#e2e8f0}` +
	`.card{text-align:center;padding:32px 40px;border-radius:12px;background:#111a2e}` +
	`h1{font-size:20px;margin:0 0 8px;color:#4ade80}p{margin:0;color:#94a3b8;font-size:13px}</style></head>` +
	`<body><div class="card"><h1>&#10003; 授权完成</h1><p>凭据已捕获，正在换取登录态…本窗口可关闭</p></div></body></html>`

// browserCaptureSpec 渠道捕获声明（接线表，见梳理文档 §4.2）。
type browserCaptureSpec struct {
	pathPrefix string                    // 回调 path 前缀（LoopbackCallbackMatcher 用）
	feed       func(rawURL string) error // 喂给既有结论通道；返回 err 仅记日志（捕获方不重试）
	ttl        time.Duration             // 捕获超时：对齐各渠道 flowTTL（不得短于监听器寿命）
}

// browserCaptureSpecs 五回环回调渠道的捕获声明。
// feed 一律走「与 SubmitCallbackURL 相同」的入口函数——那正是为「URL 在别处生成」
// 场景准备的解析路径，state 校验/端口自检/先到保留语义全部现成。
var browserCaptureSpecs = map[provider.Kind]browserCaptureSpec{
	provider.TraeWork: {
		pathPrefix: "/authorize",
		feed:       func(raw string) error { return logintrae.SubmitCallbackURL(a_loginStateFP(), raw) },
		ttl:        5 * time.Minute, // trae 监听 5m 同款
	},
	provider.CodeArts: {
		pathPrefix: "/oauth/callback",
		feed:       logincodearts.SubmitCallbackURL,
		ttl:        10 * time.Minute, // flowTTL 同款
	},
	provider.LobsterAI: {
		pathPrefix: "/auth/callback",
		feed:       loginlobsterai.SubmitCallbackURL,
		ttl:        10 * time.Minute, // flowTTL 同款
	},
	provider.QwenWork: {
		pathPrefix: "/callback",
		feed:       loginqwenwork.SubmitCallbackURL,
		ttl:        5 * time.Minute, // callbackTTL 同款
	},
	provider.Gemini: {
		pathPrefix: "/oauth-callback",
		feed:       logingemini.SubmitCallbackURL,
		ttl:        6 * time.Minute, // flowTTL 同款
	},
}

// a_loginStateFP 包级转发：捕获声明是包级 map，不能闭包持有 a；由 App 方法转发真实路径。
// loginBusy 保证同一时刻只有一条流程，读到的是本次登录的 state 文件。
func a_loginStateFP() string {
	appMu.Lock()
	cur := currentApp
	appMu.Unlock()
	if cur == nil {
		return ""
	}
	return cur.loginStateFP
}

var (
	appMu      sync.Mutex
	currentApp *App
)

// setCurrentApp init 时登记唯一 App 实例（main 装配点调用一次）。
func setCurrentApp(a *App) {
	appMu.Lock()
	currentApp = a
	appMu.Unlock()
}

// maybeStartBrowserAssist 发起登录后按需拉起 CDP 捕获（StartLoginFor 调用）。
// spec 未登记的渠道零动作；浏览器启动失败只记日志，不阻断既有路径。
func (a *App) maybeStartBrowserAssist(k provider.Kind, authURL string) {
	spec, ok := browserCaptureSpecs[k]
	if !ok {
		return
	}
	kind := k
	startBrowserAssist(kind, authURL, cdp.LoopbackCallbackMatcher(spec.pathPrefix),
		func(raw string) {
			if err := spec.feed(raw); err != nil {
				log.Printf("%s CDP 捕获喂入失败: %v", kind, err)
			}
		}, spec.ttl)
}
