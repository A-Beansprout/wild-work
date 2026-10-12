package cline

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// ---------------------------------------------------------------------------
// 真实抓包形态（2026-10-10，经官方 sk_ Key 直测）
// ---------------------------------------------------------------------------

// errFrame 生成一帧上游 error。
// 真实 wire 形态是「嵌套 JSON 塞进 message 字符串」，手写转义极易漏闭合引号
// （本测试初版就是这么被 streamCore 判成空流的），故由 Marshal 生成、原文照抄。
func errFrame(code, msg string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	return "data: " + string(b) + "\n\ndata: [DONE]\n\n"
}

// dailyMsg / tempMsg 是抓包到的上游原文（勿改措辞，锁的就是上游实际写法）。
const dailyMsg = "Failed to create stream: inference request failed: failed to generate stream " +
	"from OpenRouter: failed to invoke model 'thinkingmachines/inkling-small:free' with streaming: " +
	`request failed with status 429: {"error":{"message":"Rate limit exceeded: ` +
	`limit_rpd/thinkingmachines/inkling-small-20260730/e27a9a6d. Daily limit reached for ` +
	`thinkingmachines/inkling-small:free via Thinking Machines. Credits don't affect this cap.",` +
	`"code":429}}`

const tempMsg = "Failed to create stream: inference request failed: failed to generate stream " +
	"from OpenRouter: failed to invoke model 'google/gemma-4-26b-a4b-it:free' with streaming: " +
	`request failed with status 429: {"error":{"message":"Provider returned error","code":429,` +
	`"metadata":{"raw":"google/gemma-4-26b-a4b-it:free is temporarily rate-limited upstream. ` +
	`Please retry shortly."}}}`

// capFrameDaily：HTTP 200 + 一帧 error + **上游自己发 [DONE]**（R40 家族的完美样本）。
var capFrameDaily = errFrame("stream_initialization_failed", dailyMsg)

// capFrameTemp：短时上游限流，实测数十秒后可自愈（gemma-4-26b 一轮 429、一轮 200）。
var capFrameTemp = errFrame("stream_initialization_failed", tempMsg)

// numericCodeFrame：另一种形态，code 是数字串 "502"。
var numericCodeFrame = errFrame("502", "Bad gateway from upstream provider")

// nsBody429 是**非流式**真实形态：外层被压平成 HTTP 500，内层写着 status 429。
const nsBody429 = `{"error":"inference request failed: failed to invoke model ` +
	`'google/gemma-4-31b-it:free' from Openrouter: request failed with status 429: ` +
	`{\"error\":{\"message\":\"Provider returned error\",\"code\":429,` +
	`\"metadata\":{\"raw\":\"google/gemma-4-31b-it:free is temporarily rate-limited upstream. ` +
	`Please retry shortly, or add your own key to accumulate your rate limits\",` +
	`\"provider_name\":\"Google AI Studio\"}}}"}`

// ---------------------------------------------------------------------------
// Classify：顺序敏感 + 必须从压平的 500 里挖内层状态码
// ---------------------------------------------------------------------------

func TestClassifyOrderAndInnerStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		// 1) 共享出口上限排在一切账号级判定之前，否则 OpenRouter 日限会把整渠道账号轮一遍
		{"压平500+日限", 500, `{"error":"… from Openrouter: request failed with status 429: Daily limit reached for x:free. Credits don't affect this cap."}`, provider.ErrPassthrough},
		{"压平500+limit_rpd", 500, `Rate limit exceeded: limit_rpd/thinkingmachines/inkling`, provider.ErrPassthrough},
		{"压平500+临时限流", 500, nsBody429, provider.ErrPassthrough},
		{"云池不支持", 403, `{"error":{"code":"API_REQUEST_ERROR_CODE","message":"Error 403: This request is not supported."}}`, provider.ErrPassthrough},
		{"订阅池", 403, `{"error":{"code":"ENTITLEMENT_ERROR","message":"Error 403: the user is not subscribed"}}`, provider.ErrPassthrough},

		// 2) 无上限文案的 429 才是真账号级（配额键名 …UsageCostUSDPerUser）
		{"纯429", 429, `{"error":"too many requests"}`, provider.ErrSoftRate},
		{"压平500内层429", 500, `foo failed with status 429: quota busy`, provider.ErrSoftRate},

		// 3) 登录态：Key 被删/失效（本渠道不可刷新，只能人工换）
		{"401", 401, `{"error":"Unauthorized: Please make sure you're using the latest version of Cline"}`, provider.ErrSessionDead},
		{"压平500内层401", 500, `failed with status 401: unauthorized`, provider.ErrSessionDead},

		// 4) 本端参数/头造成的失败，不得计入账号健康度
		{"空内容", 500, `{"error":"empty response content","success":false}`, provider.ErrBadParams},
		{"低于最小值", 500, `Invalid 'max_output_tokens': integer below minimum value`, provider.ErrBadParams},
		{"漏发X-CLIENT-TYPE", 403, `{"error":{"code":"API_REQUEST_ERROR_CODE","message":"Error 403: cline-free/x"}}`, provider.ErrBadParams},

		// 5) 下架/未知/欠费/5xx
		{"404下架", 500, `from Openrouter: failed with status 404: {"error":"please check the model you provided"}`, provider.ErrNotFound},
		{"真404", 404, `not found`, provider.ErrNotFound},
		{"5xx无内层码", 502, `bad gateway`, provider.ErrServer},
		{"欠费", 500, `failed with status 402: insufficient credit balance`, provider.ErrHardCredit},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: Classify(%d,…)=%s 期望 %s\n  body=%.120s", tc.name, tc.status, got, tc.want, tc.body)
		}
	}
}

// TestRateLimitNotBucketedAsServerError 锁死不变量 15：429 必须优先于 5xx/硬冷却。
// 去掉 Classify 里「内层码优先」这条即失败。
func TestRateLimitNotBucketedAsServerError(t *testing.T) {
	if k := Classify(500, nsBody429); k == provider.ErrServer {
		t.Fatal("压平成 500 的 429 被归成 ErrServer —— 共享限流会被误判成上游故障并冷却账号")
	}
	if k := Classify(500, `failed with status 429: too many requests`); k != provider.ErrSoftRate {
		t.Fatalf("内层 429 应归 ErrSoftRate，实得 %s", k)
	}
}

func TestEffectiveStatusUnflattens(t *testing.T) {
	if got := effectiveStatus(500, nsBody429); got != 429 {
		t.Errorf("应把压平的 500 还原成 429，实得 %d", got)
	}
	if got := effectiveStatus(500, `{"error":"bad gateway"}`); got != 500 {
		t.Errorf("无内层码时不得改动状态码，实得 %d", got)
	}
	if got := effectiveStatus(429, `nothing`); got != 429 {
		t.Errorf("4xx 不应被改写，实得 %d", got)
	}
}

// TestRouteOf 锁死 gen-/gen_ 路由判据（实测 9 个 ID 零反例）。
func TestRouteOf(t *testing.T) {
	cases := map[string]string{
		"gen-1791646899-lbPmS4LwoNu8lE9uTyGG": "openrouter",    // 别名与 :free 的真实响应
		"gen_01M4K7MJKQG5BEPQ86N7ACGPQ3":      "cline_gateway", // 厂商真实名的真实响应
		"chatcmpl-abc":                        "unknown",
		"":                                    "unknown",
	}
	for id, want := range cases {
		if got := RouteOf(id); got != want {
			t.Errorf("RouteOf(%q)=%s 期望 %s", id, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 流内 error 帧：不得被伪装成「模型回了个空答案」（R40）
// ---------------------------------------------------------------------------

func streamFrom(t *testing.T, body string) (map[string]any, error, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	usage, err := New().Stream(rec, strings.NewReader(body), "cline/x:free")
	return usage, err, rec.Body.String()
}

func TestStreamRealErrorFrameReturnsError(t *testing.T) {
	for name, frame := range map[string]string{"日限": capFrameDaily, "临时限流": capFrameTemp} {
		usage, err, out := streamFrom(t, frame)
		if err == nil {
			t.Fatalf("%s: 流内 error 帧被吞掉了 —— 客户端会看到空回答且不报错", name)
		}
		var sec provider.StreamErrorClassifier
		if !errors.As(err, &sec) {
			t.Fatalf("%s: err 未实现 StreamErrorClassifier（handler 无法据此处置）：%v", name, err)
		}
		if k := sec.Kind(); k != provider.ErrPassthrough {
			t.Errorf("%s: 共享上限应 ErrPassthrough（不罚账号），实得 %s", name, k)
		}
		// 上游原文必须仍透出给客户端（不得被 normalizeFrame 白名单剥掉 error 字段）
		if !strings.Contains(out, `"error"`) || !strings.Contains(out, "stream_initialization_failed") {
			t.Errorf("%s: error 帧未原样透传：%.200s", name, out)
		}
		if usage != nil {
			t.Errorf("%s: 错误流不应有 usage，实得 %v", name, usage)
		}
	}
}

// TestStreamMarksCappedModel 验证撞共享上限只标记模型、不动账号。
func TestStreamMarksCappedModel(t *testing.T) {
	c := New()
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader(capFrameDaily), "stepfun/step-5-preview")
	if err == nil {
		t.Fatal("期望返回 error")
	}
	if !c.isCapped("stepfun/step-5-preview") {
		t.Error("该模型应被标记为暂时不可用（期间从 /v1/models 摘除）")
	}
	if c.isCapped("xiaomi/mimo-v2.6-flash") {
		t.Error("不得波及其他模型")
	}
}

// TestStreamNumericErrorCode 锁 "502" 这类非语义 code 也能被识别。
func TestStreamNumericErrorCode(t *testing.T) {
	_, err, _ := streamFrom(t, numericCodeFrame)
	if err == nil {
		t.Fatal("数字 code 的 error 帧也必须返回 error")
	}
	var se *StreamError
	if !errors.As(err, &se) || se.Code != "502" {
		t.Fatalf("应解析出 code=502，实得 %v", err)
	}
}

func TestAggregateDetectsErrorFrame(t *testing.T) {
	res, err := New().Aggregate(strings.NewReader(capFrameDaily), "cline/inkling:free")
	if err == nil {
		t.Fatal("非流式聚合路径同样不得吞掉 error 帧（handler 的 stream=false 分支走这里）")
	}
	if res != nil && res["model"] != "cline/inkling:free" {
		t.Errorf("Aggregate 应回填调用方原始模型名（R14），实得 %v", res["model"])
	}
}

// TestStreamMalformedErrorFrame 帧非法时 streamCore 判空流并返回 error，不得伪装成功。
func TestStreamMalformedErrorFrame(t *testing.T) {
	_, err, _ := streamFrom(t, "data: {\"error\": broken\n\n")
	if err == nil {
		t.Fatal("非法帧不得伪装成正常收尾")
	}
	if !upstream.IsEmptyStreamError(err) {
		t.Logf("形态：%v（非空流错误也可接受，只要不伪装成功）", err)
	}
}

// TestStreamNormalCompletionUnchanged 对照：正常流必须零改动、且能拿到 usage。
func TestStreamNormalCompletionUnchanged(t *testing.T) {
	body := `data: {"id":"gen_01M4K","object":"chat.completion.chunk","model":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	usage, err, out := streamFrom(t, body)
	if err != nil {
		t.Fatalf("正常流不应报错：%v", err)
	}
	if usage == nil || usage["completion_tokens"] != float64(2) {
		t.Errorf("末帧 usage 未被捕获：%v", usage)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("内容未透传：%.200s", out)
	}
}

// ---------------------------------------------------------------------------
// 请求头与 max_tokens
// ---------------------------------------------------------------------------

// TestChatStreamAlwaysSendsClientTypeHeader：删掉那个头即失败。
// 危害特征与 R34 同族 —— 不报错不崩溃，只是免费模型全部 403、面板毫无异常。
func TestChatStreamAlwaysSendsClientTypeHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	a := &auth.Auth{Kind: "cline", AccessToken: "sk_testkey", ExpiresAt: FarFutureExpiry()}
	_, status, _, err := c.ChatStream(a,
		[]byte(`{"model":"stepfun/step-5-preview","stream":true,"messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("假上游应通：status=%d err=%v", status, err)
	}
	if got.Get(HeaderClientType) == "" {
		t.Fatal("必须携带非空 X-CLIENT-TYPE，否则 cline-free/* 一律 403（值随意，但不得缺省）")
	}
	if got.Get("Authorization") != "Bearer sk_testkey" {
		t.Errorf("Authorization 形态错：%q", got.Get("Authorization"))
	}
}

func TestClampMaxTokens(t *testing.T) {
	get := func(b []byte) (any, bool) {
		var o map[string]any
		if json.Unmarshal(b, &o) != nil {
			return nil, false
		}
		v, ok := o["max_tokens"]
		return v, ok
	}
	if v, ok := get(clampMaxTokens([]byte(`{"model":"x"}`))); ok {
		t.Errorf("客户端未给 max_tokens 时不得补值（实测不传最稳），实得 %v", v)
	}
	if v, _ := get(clampMaxTokens([]byte(`{"max_tokens":16}`))); v != float64(minUpstreamMaxTokens) {
		t.Errorf("16 应抬到 %d（实测 16 → 500 empty response content），实得 %v", minUpstreamMaxTokens, v)
	}
	if v, _ := get(clampMaxTokens([]byte(`{"max_tokens":1000000}`))); v != float64(maxUpstreamMaxTokens) {
		t.Errorf("1e6 应压到 %d（实测被拒），实得 %v", maxUpstreamMaxTokens, v)
	}
	if v, _ := get(clampMaxTokens([]byte(`{"max_tokens":8000}`))); v != float64(8000) {
		t.Errorf("合法值不得改动，实得 %v", v)
	}
	if _, ok := get(clampMaxTokens([]byte(`{"max_tokens":0}`))); ok {
		t.Error("0 视同未给，应删除")
	}
}

// TestChatStreamRejectsUnknownModelLocally：本地校验集刻意宽松，但完全未知的名字要挡住。
func TestChatStreamRejectsUnknownModelLocally(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New()
	c.Base = srv.URL
	_, status, body, _ := c.ChatStream(&auth.Auth{Kind: "cline", AccessToken: "sk_x"},
		[]byte(`{"model":"totally/unknown-model","stream":true,"messages":[]}`))
	if status != http.StatusNotFound || hits != 0 {
		t.Errorf("未知模型应本地 404 且不打上游，实得 status=%d hits=%d body=%.100s", status, hits, body)
	}
}

// ---------------------------------------------------------------------------
// 目录：只暴露 Cline 官方免费池（暴露保守 + 校验宽松）
// ---------------------------------------------------------------------------

func fakeCatalog() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case EpRecommends:
			_, _ = io.WriteString(w, `{"recommended":[{"id":"anthropic/claude-sonnet-5.5"}],
			  "free":[{"id":"cline-free/step-5-preview","name":"Step 5 Preview"},
			         {"id":"cline-free/newcomer-x","name":""}],
			  "clinePass":[{"id":"cline-pass/glm-5.3"}],
			  "clineCloud":[{"id":"cline-cloud/deepseek-v4.1-flash"}]}`)
		case EpModels:
			_, _ = io.WriteString(w, `{"object":"list","data":[
			  {"id":"cohere/north-mini-code:free","owned_by":"cohere"},
			  {"id":"openai/gpt-6.1-sol","owned_by":"openai"}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
}

func TestFetchModelsExposesOnlyClineFree(t *testing.T) {
	srv := httptest.NewServer(fakeCatalog())
	defer srv.Close()
	c := New()
	c.Base = srv.URL

	models, err := c.FetchModels(&auth.Auth{Kind: "cline", AccessToken: "sk_x"})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, m := range models {
		sb.WriteString(m.ID + " ")
	}
	got := sb.String()
	for _, want := range []string{"stepfun/step-5-preview", "xiaomi/mimo-v2.6-flash",
		"meta/muse-spark-1.3-contributor", "cline-free/newcomer-x"} {
		if !strings.Contains(got, want) {
			t.Errorf("应暴露 %s，实得：%s", want, got)
		}
	}
	// 别名与真实名同基名 ⇒ 只保留真实名一次
	if n := strings.Count(got, "step-5-preview"); n != 1 {
		t.Errorf("step-5-preview 应按基名去重，实际出现 %d 次：%s", n, got)
	}
	if strings.Contains(got, "cline-free/step-5-preview") {
		t.Errorf("去重后必须保留真实名（走 Cline 网关、不依赖 X-CLIENT-TYPE），实得：%s", got)
	}
	for _, ban := range []string{"glm-5.3", "deepseek-v4.1-flash", "claude-sonnet-5.5", "north-mini-code"} {
		if strings.Contains(got, ban) {
			t.Errorf("%s 不该出现在暴露集（订阅池/云池/付费/OpenRouter 长尾）：%s", ban, got)
		}
	}
}

// TestKnownModelAcceptsLiveCatalog 是 R42 的回归：动态目录里的模型不得被本地校验误拒。
func TestKnownModelAcceptsLiveCatalog(t *testing.T) {
	srv := httptest.NewServer(fakeCatalog())
	defer srv.Close()
	c := New()
	c.Base = srv.URL
	if _, err := c.FetchModels(&auth.Auth{Kind: "cline", AccessToken: "sk_x"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cline-free/newcomer-x", "cohere/north-mini-code:free",
		"stepfun/step-5-preview", "openai/gpt-6.1-sol"} {
		if !c.knownModel(id) {
			t.Errorf("%s 应被接受（静态 ∪ 动态目录）", id)
		}
	}
	if c.knownModel("vendor/not-in-any-catalog") {
		t.Error("目录外的名字仍应被拒")
	}
}

// TestCappedModelAutoHidesAndReturns：标记后摘除，解冻后自动回归。
func TestCappedModelAutoHidesAndReturns(t *testing.T) {
	srv := httptest.NewServer(fakeCatalog())
	defer srv.Close()
	c := New()
	c.Base = srv.URL
	a := &auth.Auth{Kind: "cline", AccessToken: "sk_x"}

	c.markCapped("stepfun/step-5-preview", time.Hour)
	for _, m := range mustModels(t, c, a) {
		if strings.Contains(m.ID, "step-5-preview") {
			t.Fatalf("冷却中的模型不得出现在 /v1/models：%v", m.ID)
		}
	}
	c.markCapped("stepfun/step-5-preview", -time.Second) // 立即过期
	found := false
	for _, m := range mustModels(t, c, a) {
		if strings.Contains(m.ID, "step-5-preview") {
			found = true
		}
	}
	if !found {
		t.Error("解冻后应自动回归列表")
	}
}

func mustModels(t *testing.T, c *Client, a *auth.Auth) []provider.ModelInfo {
	t.Helper()
	m, err := c.FetchModels(a)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------------------------------------------------------------------------
// 接口完整性与默认值
// ---------------------------------------------------------------------------

// TestUpstreamContract 编译期锁死：Client 必须满足 provider.Upstream。
func TestUpstreamContract(t *testing.T) {
	var u provider.Upstream = New()
	if u == nil {
		t.Fatal("nil")
	}
}

// TestNoAccountEndpointsMustNotError：上游确无额度端点 ⇒ 必须返回 0 而不是 error。
func TestNoAccountEndpointsMustNotError(t *testing.T) {
	c := New()
	a := &auth.Auth{Kind: "cline", AccessToken: "sk_x"}
	if n, err := c.UserResource(a); n != 0 || err != nil {
		t.Errorf("UserResource 应为 (0,nil)，实得 (%d,%v)", n, err)
	}
	if n, items, err := c.UserResourceDetail(a); n != 0 || items != nil || err != nil {
		t.Errorf("UserResourceDetail 应为 (0,nil,nil)，实得 (%d,%v,%v)", n, items, err)
	}
	if p, err := c.FetchModelPricing(a); p != nil || err != nil {
		t.Errorf("FetchModelPricing 应为 (nil,nil)，实得 (%v,%v)", p, err)
	}
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("Key 不轮换 ⇒ RefreshToken 应为空实现：%v", err)
	}
}

// TestExpiresAtFarFuture：0 会让 NeedsRefresh 恒真 → 每次请求刷 token（不变量 21 同款陷阱）。
func TestExpiresAtFarFuture(t *testing.T) {
	a := &auth.Auth{Kind: "cline", AccessToken: "sk_x", ExpiresAt: FarFutureExpiry()}
	if a.NeedsRefresh(10 * time.Minute) {
		t.Fatal("远期 ExpiresAt 下不应需要刷新")
	}
	zero := auth.Auth{AccessToken: "sk_x"}
	if !zero.NeedsRefresh(10 * time.Minute) {
		t.Fatal("对照：ExpiresAt=0 应当恒为需要刷新（这正是必须避免 0 值的原因）")
	}
}

// TestStreamNotBoundedByClientTimeout 复现 R35 的回归口径：
// 流式必须走 StreamHTTP（无总超时），慢回答不会被 Client.Timeout 从中间掐断。
func TestStreamNotBoundedByClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(250 * time.Millisecond) // 总耗时 ~750ms，远超非流式 client 的超时
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	c.HTTP = &http.Client{Timeout: 150 * time.Millisecond, Transport: newTransport()} // 故意很短
	a := &auth.Auth{Kind: "cline", AccessToken: "sk_x", ExpiresAt: FarFutureExpiry()}

	rc, status, _, err := c.ChatStream(a,
		[]byte(`{"model":"stepfun/step-5-preview","stream":true}`))
	if err != nil || status != 200 {
		t.Fatalf("流式应走无总超时的 StreamHTTP：status=%d err=%v", status, err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if strings.Count(string(body), "content") != 3 {
		t.Fatalf("慢流被掐断，只拿到 %d 帧", strings.Count(string(body), "content"))
	}

	// 对照：同一上游经带总超时的 HTTP client 必然在读阶段报错 ⇒ 两个 client 确实分离
	resp, herr := c.HTTP.Get(srv.URL)
	if herr == nil {
		_, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr == nil {
			t.Error("带 150ms 总超时的 HTTP client 竟读完了 750ms 的慢流 —— StreamHTTP 与 HTTP 未分离")
		}
	}
}

// TestValidateKeyShape 校验 uid 取自 apk- id、且能列出同账号兄弟 Key（同账号告警的依据）。
func TestValidateKeyShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpAPIKeys {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"keys":[
		  {"id":"apk-AAA","name":"main","key_prefix":"sk_aaaaaaa"},
		  {"id":"apk-BBB","name":"second","key_prefix":"sk_bbbbbbb"}]},"success":true}`)
	}))
	defer srv.Close()
	orig := defaultBaseURL
	_ = orig // ValidateKey 用默认基址，这里改测内部匹配逻辑
	key := "sk_aaaaaaa" + strings.Repeat("0", 57)
	// 直接走 client 侧的匹配（ValidateKey 需要真网络，单测里用 Base 注入等价路径）
	c := New()
	c.Base = srv.URL
	res, err := c.probeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if res.UID != "apk-AAA" {
		t.Errorf("uid 应取 key_prefix 相符那条的 id，实得 %q", res.UID)
	}
	if len(res.SiblingKeys) != 1 || res.SiblingKeys[0] != "sk_bbbbbbb" {
		t.Errorf("应列出同账号兄弟 Key（用于「同账号不扩额度」告警），实得 %v", res.SiblingKeys)
	}
	if res.KeyPrefix != "sk_aaaaaaa" {
		t.Errorf("KeyPrefix 应回显前 %d 字符，实得 %q", KeyPrefixLen, res.KeyPrefix)
	}
}

// TestValidateKeyRejectsNonSK：形态不对的输入在发请求前就拒，避免无谓 401。
func TestValidateKeyRejectsNonSK(t *testing.T) {
	if _, err := ValidateKey("workos:abc"); err == nil || !strings.Contains(err.Error(), "sk_") {
		t.Errorf("非 sk_ 前缀应直接报错并给出申请指引，实得 %v", err)
	}
}

// TestBaseName 锁基名归一（R33 同款：查表/去重前必须剥路由前后缀）。
func TestBaseName(t *testing.T) {
	for id, want := range map[string]string{
		"cline-free/step-5-preview":        "step-5-preview",
		"cline-pass/glm-5.3-flash":         "glm-5.3-flash",
		"cline-cloud/deepseek-v4.1-flash":  "deepseek-v4.1-flash",
		"stepfun/step-5-preview":           "step-5-preview",
		"cohere/north-mini-code:free":      "north-mini-code",
		`  xiaomi/mimo-v2.6-flash  `:       "mimo-v2.6-flash",
		"cline-free/step-5-preview:unused": "step-5-preview:unused",
	} {
		if got := baseName(id); got != want {
			t.Errorf("baseName(%q)=%q 期望 %q", id, got, want)
		}
	}
}

var _ = upstream.Aggregate // 显式保留对通用聚合的依赖，便于将来行为对照
