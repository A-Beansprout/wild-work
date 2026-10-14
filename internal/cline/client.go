package cline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/backuplink"
	"wild-work/internal/provider"
)

// Client 实现 provider.Upstream。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流：**不设 Client.Timeout**（R35）。
	// Go 的 Client.Timeout 覆盖整个请求生命周期（含读 body），对 SSE 意味着
	// 「长回答必被从中间掐断」。空闲兜底改由 IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲上限，0 = DefaultIdleTimeout；由 main 注入
	// config.upstream.stream_idle_seconds。
	IdleTimeout time.Duration
	// Base 网关基址（默认 APIBase）；测试注入 httptest 假上游用，生产不设。
	Base string

	knownMu sync.RWMutex
	// knownIDs 是已知模型集（ID → 展示名，来自 free[]），也是 ChatStream 的本地校验集。
	// 冷启动（未拉到目录）时校验放行 —— 见 models.go 的 knownModel 注释。
	knownIDs map[string]string
	capMu    sync.RWMutex
	capped   map[string]time.Time
	regMu    sync.RWMutex
	// regModels 是「地区限制→改走备用上游通道」的学习路由：模型 ID → 过期时刻。
	// 命中一次后 TTL 内不再白撞直连；备用地址一变（Generation 变）就整体作废。
	regModels map[string]time.Time
	regGen    uint64 // 写入时的 backuplink.Generation()
}

// New 构造客户端（非流式 120s 总超时；一问一答需要兜底，故保留 Timeout）。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		knownIDs:   map[string]string{},
		capped:     map[string]time.Time{},
	}
}

// newTransport 出厂 Transport：禁 h2 + ResponseHeaderTimeout 兜底。
// StreamHTTP 无总超时后**必须**有 ResponseHeaderTimeout，否则连响应头都等不到会无限挂。
func newTransport() *http.Transport {
	return &http.Transport{
		DisableKeepAlives:     false,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		DisableCompression:    false,
	}
}

func (c *Client) base() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	return defaultBaseURL
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP == nil {
		c.StreamHTTP = &http.Client{Transport: newTransport()}
	}
	return c.StreamHTTP
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 120 * time.Second, Transport: newTransport()}
	}
	return c.HTTP
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout <= 0 {
		return DefaultIdleTimeout
	}
	return c.IdleTimeout
}

// applyHeaders 统一出站头。
//
// ★ X-CLIENT-TYPE 必须带且必须非空：缺这个头，所有 `cline-free/*` 一律 403
//
//	API_REQUEST_ERROR_CODE，且**症状是「免费模型全部消失」而不报任何配置错误**。
//	值本身不参与判定（实测 bogus-value 同样放行），故固定写死，勿当优化点删除。
func applyHeaders(req *http.Request, key string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderClientType, HeaderClientTypeValue)
	if key != "" {
		req.Header.Set(HeaderAuth, "Bearer "+key)
	}
}

func keyOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessTokenValue())
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// ChatStream 实现 provider.Upstream：返回上游流（200）或已读尽的错误响应。
//
// 状态码会经 effectiveStatus **还原被网关压平的内层码**（实测外层恒 500），
// 让客户端与 handler 看到真正的 429/401/404；响应体保持原文（不变量 8 的偏离已在注释说明）。
//
// 地区限制自动改道（决议 R57）：直连命中 IsRegionBlocked ⇒ 同一次请求内经**备用上游通道**
// 重放一次；未配置备用通道则原样透传失败。重试全部在本方法内完成，handler/账号惩罚/
// 粘性路由对该机制无感（备用链路只是换出口 IP，不是换账号）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	m := modelOf(body)
	if m == "" {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"model is required"}}`), nil
	}
	// 本地校验：集合 == 暴露集 == free[]（构造即一致）；目录未拉到过时放行（fail-open）。
	// R42 的教训：校验过严会误拒上游新模型，故冷启动不拦、集合跟随上游同步。
	if !c.knownModel(m) {
		return nil, http.StatusNotFound,
			[]byte(fmt.Sprintf(`{"error":{"message":"model not found in cline free catalog: %s",
			"code":"model_not_found"}}`, jsonEscape(m))), nil
	}

	out := clampMaxTokens(body)
	out = forceStream(out)
	key := keyOf(a)

	// 首选链路：学到「直连被地区拒」的模型直接走备用，省一次注定失败的白撞
	// （多发 0.3~2s，且在上游留下重复失败特征）。
	useBackup := c.regionLearned(m)
	resp, err := c.attempt(out, key, useBackup)
	if err != nil {
		// 备用通道拨不动 / 与配置变更的竞态 ⇒ 作废学习结果并回落直连一次，
		// 否则代理一故障就把「本来能直连」的模型钉死不可用。
		link := "直连"
		if useBackup {
			link = "备用"
		}
		log.Printf("cline: 首选链路(%s)不可用 uid=%s model=%s err=%v（作废学习结果，回落直连）", link, uidOf(a), m, err)
		c.forgetRegion(m)
		if resp, err = c.attempt(out, key, false); err != nil {
			return nil, 0, nil, err
		}
		useBackup = false
	}
	if resp.StatusCode == http.StatusOK {
		if useBackup {
			c.markRegion(m) // 备用链路确认可用，续期学习结果
		}
		return c.openStream(resp), resp.StatusCode, nil, nil
	}
	status, raw := drainBody(resp)

	// (A) 走备用仍被地区拒 ⇒ 上游策略或代理出口已变 ⇒ 作废学习结果并回落直连。
	// 不做这层的话，最长要等 TTL（6h）才回到直连，期间该模型一直报错。
	if useBackup && IsRegionBlocked(status, string(raw)) {
		c.forgetRegion(m)
		r2, e2 := c.attempt(out, key, false)
		if e2 != nil {
			log.Printf("cline: 备用被地区拒且直连拨不动 uid=%s model=%s backup=%d err=%v（交出备用那份）", uidOf(a), m, status, e2)
		} else if r2.StatusCode == http.StatusOK {
			log.Printf("cline: 直连已恢复可用 uid=%s model=%s（学习结果已作废）", uidOf(a), m)
			return c.openStream(r2), r2.StatusCode, nil, nil
		} else {
			status, raw = drainBody(r2) // 交出直连那份（主链路口径），不再乒乓重试
		}
		if IsCapBody(string(raw)) {
			c.markCapped(m, capCooldown)
		}
		return nil, effectiveStatus(status, string(raw)), raw, nil
	}

	// (B) 直连被上游按地区拒 ⇒ 同请求内经备用通道重放一次（判据保守，见 regionRetryMarkers）。
	if IsRegionBlocked(status, string(raw)) {
		r2, e2 := c.attempt(out, key, true)
		switch {
		case errors.Is(e2, errNoBackup):
			// 未配置备用通道：保持旧行为（原文透传，不罚账号，见 Classify 的 regionBlockedMarkers）。
			log.Printf("cline: 上游按地区拒绝 uid=%s model=%s（未配置备用上游代理，原样透传）：%s",
				uidOf(a), m, truncate(string(raw), 120))
		case e2 != nil:
			// R37 同族：不得用第二次尝试的错误掩盖首因 ⇒ 仍透传直连那份 403 原文，代理故障只进日志。
			log.Printf("cline: 备用通道重放失败 uid=%s model=%s err=%v（透传直连原始响应）", uidOf(a), m, e2)
		case r2.StatusCode == http.StatusOK:
			c.markRegion(m)
			log.Printf("cline: 地区限制已改走备用通道 uid=%s model=%s（后续同模型直接走备用）", uidOf(a), m)
			return c.openStream(r2), r2.StatusCode, nil, nil
		default:
			s2, raw2 := drainBody(r2)
			if IsRegionBlocked(s2, string(raw2)) {
				// 备用出口同样受限 ⇒ **不**记学习结果（钉上去只会长期失败），仍透传直连首因。
				log.Printf("cline: 备用通道出口同样被地区拒绝 uid=%s model=%s（透传直连原始响应）", uidOf(a), m)
			} else {
				// 链路可达、上游给了**不同**的权威答复 ⇒ 以它为准，并记学习结果。
				c.markRegion(m)
				if IsCapBody(string(raw2)) {
					c.markCapped(m, capCooldown)
				}
				log.Printf("cline: 经备用通道拿到上游答复 uid=%s model=%s status=%d", uidOf(a), m, s2)
				return nil, effectiveStatus(s2, string(raw2)), raw2, nil
			}
		}
	}

	if IsCapBody(string(raw)) {
		c.markCapped(m, capCooldown)
		log.Printf("cline: 模型触上限 uid=%s model=%s（不罚账号，已标记冷却）", uidOf(a), m)
	}
	return nil, effectiveStatus(status, string(raw)), raw, nil
}

// errNoBackup 「要求走备用但未配置备用通道」，多出现于学习判定与配置变更的竞态。
var errNoBackup = errors.New("cline: 备用上游通道未配置")

// attempt 发一发出站对话请求：useBackup 决定出口（直连 or 备用上游通道）。
// 重放时重新构造请求（body 已在内存，无需 GetBody），两个出口共用同一构造器
// ⇒ 头完全一致：X-CLIENT-TYPE 缺失会恒回 403，重放少了它会把「代理也没用」误判成代理故障。
func (c *Client) attempt(out []byte, key string, useBackup bool) (*http.Response, error) {
	cli := c.streamClient()
	if useBackup {
		bc := c.backupClient()
		if bc == nil {
			return nil, errNoBackup
		}
		cli = bc
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+EpChat, bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, key)
	return cli.Do(req)
}

// drainBody 读尽并关闭**非 200** 的响应体（限 64KB，与旧行为一致），返回 (状态码, 原文)。
func drainBody(resp *http.Response) (int, []byte) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

// openStream 把 200 响应包成带空闲兜底的流（R35：不用 Client.Timeout，靠 IdleReader）。
func (c *Client) openStream(resp *http.Response) io.ReadCloser {
	return provider.NewIdleReader(resp.Body, c.idleTimeout())
}

// backupClient 返回绑定备用上游通道的 client（未配置返回 nil = 只能直连）。
// base 取流式 client 的 Transport：只改出口，其余超时/协议参数与出厂一致。
func (c *Client) backupClient() *http.Client {
	tr, _ := c.streamClient().Transport.(*http.Transport)
	return backuplink.Client(tr)
}

// regionRouteTTL 学习结果有效期：地区限制是稳定属性，取长一些（6h）以免每发都先白撞
// 一次直连；到期后自动重新试探直连（上游若解除限制即回归直连）。
const regionRouteTTL = 6 * time.Hour

// regionLearned 报告该模型是否已学到「直连被地区拒绝、只能走备用」，且备用配置仍生效。
func (c *Client) regionLearned(m string) bool {
	if !backuplink.Enabled() {
		return false
	}
	g := backuplink.Generation()
	now := time.Now()
	c.regMu.RLock()
	defer c.regMu.RUnlock()
	exp, ok := c.regModels[m]
	return ok && g == c.regGen && now.Before(exp)
}

// markRegion 记录/续期学习结果；备用配置一变（generation 变）就整体作废重建。
func (c *Client) markRegion(m string) {
	g := backuplink.Generation()
	if g == 0 {
		return
	}
	now := time.Now()
	c.regMu.Lock()
	defer c.regMu.Unlock()
	if c.regModels == nil {
		c.regModels = map[string]time.Time{}
	}
	if c.regGen != g {
		clear(c.regModels)
		c.regGen = g
	}
	c.regModels[m] = now.Add(regionRouteTTL)
	for k, t := range c.regModels { // 顺带清过期项，防长期碎片
		if now.After(t) {
			delete(c.regModels, k)
		}
	}
}

// forgetRegion 作废单个模型的学习结果（备用链路拨不动时回落直连用）。
func (c *Client) forgetRegion(m string) {
	c.regMu.Lock()
	defer c.regMu.Unlock()
	delete(c.regModels, m)
}

// forceStream 强制上游 stream:true（不变量 1）。
//
// **为什么必须有**（2026-10-12 实测踩坑）：本渠道把客户端 body 原样转发，而 handler 对上游
// 响应**一律按 SSE 解析** —— 流式走 Stream、非流式客户端走 Aggregate，二者都只认 `data:` 帧。
// 客户端发 `stream:false`（或干脆不带 stream）时，上游会回**非流式 JSON 信封**，解析器找不到
// 任何 data 帧 ⇒ `errEmptyStream` ⇒ 502 `upstream_parse`，且该错误被当作账号连续错误
// ⇒ 冷却整条渠道（实测：一次非流式请求后，四个免费模型全部 503 no_healthy_account 约 60s）。
//
// 讽刺的是 sse.go 的 Aggregate 注释早已假定「本渠道永远请求 stream=true（不变量 1）」，
// 但 ChatStream 此前只做了 clampMaxTokens，并未落实该假定 —— 这就是 bug 本身。
// 非流式客户端由 handler 本地聚合（Aggregate），与 oczen/monkeycode 同款做法。
func forceStream(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, _ := obj["stream"].(bool); v {
		return body // 已是 true，零改动原样返回（不重新 marshal，避免无谓的键序变化）
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// clampMaxTokens 治理输出上限（实测：8→上游 400；16→500 empty response content；
// 64~200000 全通；1e6→拒）。**客户端未给时一律不传** —— 不传最稳，实测可自由跑 3242 token
// 且正常出内容；强行补一个小值反而会制造「空回答」。
func clampMaxTokens(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, ok := obj["max_tokens"]; ok {
		n := toInt(v)
		switch {
		case n <= 0:
			delete(obj, "max_tokens") // 0/负数/非数值：视同未给
		case n < minUpstreamMaxTokens:
			obj["max_tokens"] = minUpstreamMaxTokens
		case n > maxUpstreamMaxTokens:
			obj["max_tokens"] = maxUpstreamMaxTokens
		}
	}
	// 同步治理 OpenAI 新字段名（上游只认 max_tokens；带 max_completion_tokens 时按同规则夹）
	if v, ok := obj["max_completion_tokens"]; ok {
		n := toInt(v)
		if n > 0 {
			if n < minUpstreamMaxTokens {
				n = minUpstreamMaxTokens
			}
			if n > maxUpstreamMaxTokens {
				n = maxUpstreamMaxTokens
			}
			obj["max_completion_tokens"] = n
		} else {
			delete(obj, "max_completion_tokens")
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------------------
// 凭据探针（面板添加 Key 用：零对话成本，只读自己的 Key 列表）
// ---------------------------------------------------------------------------

type apiKeyInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

type apiKeysResp struct {
	Data struct {
		Keys []apiKeyInfo `json:"keys"`
	} `json:"data"`
	Success bool `json:"success"`
}

// ProbeResult 是 ValidateKey 的产出。
type ProbeResult struct {
	UID         string   // 稳定的 apk- id；取不到时退化为 key 的 sha256 前缀
	KeyPrefix   string   // 本 Key 的官方回显前缀
	SiblingKeys []string // 同一账号下的其它 Key 前缀（用于「同账号不扩额度」告警）
	AccountName string   // Key 的 name 字段，仅展示
}

// ValidateKey 校验 API Key 并取回身份（面板添加账号入口，见 app.SubmitClineKey）。
//
// 只调 GET /api/v1/api-keys —— **零 token 消耗、零副作用**，避免「添加账号即烧额度」。
// uid 取该端点里 key_prefix 与本 Key 相符的那条 id（实测 key_prefix == key 前 10 字符）。
// 同账号判定：该端点返回**本账号全部 Key**，故能列出兄弟 Key 的前缀 ——
// 由于配额键名是 `…UsageCostUSDPerUser`（按账号计），同账号多 Key **不会**扩额度，调用方据此告警。
func ValidateKey(key string) (*ProbeResult, error) { return New().probeKey(key) }

// probeKey 是 ValidateKey 的可注入实现（Base 指向 httptest 假上游即可单测）。
func (c *Client) probeKey(key string) (*ProbeResult, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "sk_") {
		return nil, fmt.Errorf("Cline API Key 应以 sk_ 开头（在 app.cline.bot → Account / API Keys 创建）")
	}
	req, err := http.NewRequest(http.MethodGet, c.base()+EpAPIKeys, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, key)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Key 无效或被拒（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return nil, fmt.Errorf("校验 Key 失败 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var r apiKeysResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("api-keys 响应无法解析：%v", err)
	}
	prefix := key
	if len(prefix) > KeyPrefixLen {
		prefix = prefix[:KeyPrefixLen]
	}
	res := &ProbeResult{KeyPrefix: prefix}
	for _, k := range r.Data.Keys {
		kp := strings.TrimSpace(k.KeyPrefix)
		if kp == "" {
			continue
		}
		if kp == prefix {
			res.UID = strings.TrimSpace(k.ID)
			res.AccountName = strings.TrimSpace(k.Name)
			continue
		}
		res.SiblingKeys = append(res.SiblingKeys, kp)
	}
	if res.UID == "" {
		// 列表里没有自己（上游形态变化）⇒ 用 hash 兜底，仍保证稳定且不含明文。
		sum := sha256.Sum256([]byte(key))
		res.UID = "k" + hex.EncodeToString(sum[:])[:16]
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Upstream 其余方法
// ---------------------------------------------------------------------------

// RefreshToken 空实现：官方 API Key **不轮换**。
// 因此不变量 20（refresh 后必须 SaveAtomic）在本渠道不适用，
// 且 R26/R44 那种「同凭据多处使用互踢」的问题天然不存在。
func (c *Client) RefreshToken(a *auth.Auth) error { return nil }

// UserResource 恒 0 且**不得返回 error**：上游确无额度/余额端点
// （/credits /usage /billing /account /quota /me 等 12 个候选实测全 404）。
// 返回 error 会让面板与池逻辑报错，而"0"才是这里的正确事实。
func (c *Client) UserResource(a *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上：可消耗余额 0、无明细。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 无签到活动。调度器侧 CheckinHours 传 []int{}（R39），
// 且 noExplicitCheckin 会挡掉面板手工签到，故本方法正常情况下不会被调用。
func (c *Client) DailyCheckin(a *auth.Auth) error { return fmt.Errorf("cline 无签到活动") }

// FetchModelPricing 上游无费率端点；单价只在逐请求 usage 里（cost/cost_details），
// 由 ledger 记账，不进费率表。返回空不报错（面板据此显示「不适用」）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

var modelRE = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

// modelOf 从请求体里取模型名（不做完整反序列化，省一次分配）。
func modelOf(body []byte) string {
	if m := modelRE.FindSubmatch(body); len(m) == 2 {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func uidOf(a *auth.Auth) string {
	if a == nil {
		return "-"
	}
	return a.UID
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		var i int
		_, _ = fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}

func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"?"`
	}
	return string(b)
}

// getJSON 发一个 GET 并回原始 body（带 Bearer，虽然两个目录端点实测无需鉴权）。
func (c *Client) getJSON(a *auth.Auth, path string) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, keyOf(a))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode, Msg: truncate(string(raw), 240)}
	}
	return json.RawMessage(raw), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
