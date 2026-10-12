package cline

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 硬编码免费池（决议 R46：先上线公测，后续按反馈调整这张表）
// ---------------------------------------------------------------------------

// staticFreeModels 是本渠道默认暴露的免费模型。一律用**厂商真实名**而非 `cline-free/` 别名，
// 四条实测依据（见包注释第 4 点与 docs/cline渠道接入计划.md §3.2）：
//  1. 真实名不依赖 X-CLIENT-TYPE 头（别名缺该头即全量 403）；
//  2. `stepfun/step-5-preview`、`meta/muse-spark-1.3-contributor` 走 **Cline 自有网关**（gen_ 前缀），
//     配额键名是 `…UsageCostUSDPerUser` ⇒ 多账号轮换才可能有意义；别名一律被甩给 OpenRouter；
//  3. 网关路径带 provider_metadata（marketCost/routing/fallbacksAvailable）⇒ ledger 拿得到单价；
//  4. 真实名本身就在公开目录里，符合 R42「静态 ∪ 动态」。
//
// 注意 `meta/muse-spark-1.3-contributor` 实测**会扣费**（7.12e-05~8.98e-05，且同 ID 曾出现 cost=0
// ⇒ 计价逐请求抽签）。保留是用户的明确决定（公测期自行验证），兜底靠模型级冷却与运行期 cost 观测。
var staticFreeModels = []provider.ModelInfo{
	{ID: "stepfun/step-5-preview", Name: "Step 5 Preview (Free)"},
	{ID: "xiaomi/mimo-v2.6-flash", Name: "MiMo V2.6 Flash (Free)"},
	{ID: "meta/muse-spark-1.3-contributor", Name: "Muse Spark 1.3 Contributor (Free)"},
}

// StaticModels 实现 server.StaticModels 注入点。
// ContextWindow/MaxTokens **留 0**：上游 recommended-models 与 /models 都不返回元数据，
// 不得填猜测值 —— 面板按 0 隐藏该列（与不变量 18「不得用零值时间冒充永不过期」同一纪律）。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, staticFreeModels...)
}

// KnownModel 静态表成员判定。
func KnownModel(id string) bool {
	id = strings.TrimSpace(id)
	for _, m := range staticFreeModels {
		if m.ID == id {
			return true
		}
	}
	return false
}

// freePrefixes 是 Cline 自有路由前缀（参照 cline2api 的 clineModelPrefixes 思路）：
// 查表/去重前必须先剥掉，否则 `cline-free/step-5-preview` 与 `stepfun/step-5-preview`
// 会被当成两个不同模型同时出现在列表里。与 R33（glm 的 `:moe_53f` 后缀剥离）同族问题。
var freePrefixes = []string{freePrefix, passPrefix, cloudPrefix}

// baseName 去掉路由前缀与 OpenRouter 的 `:free` 后缀，得到可比较的基名。
// `cline-free/step-5-preview` / `stepfun/step-5-preview` → `step-5-preview`
// `cohere/north-mini-code:free` → `north-mini-code`
func baseName(id string) string {
	s := strings.TrimSpace(id)
	for _, p := range freePrefixes {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	s = strings.TrimSuffix(s, openaiFreeSuffix)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

// isClineFree 报告 ID 是否属于 Cline 自有免费池（别名空间）。
// 公开 458 目录里的 `x/y:free` 是 OpenRouter 长尾免费档，**按裁定不暴露**（A 组小众）。
func isClineFree(id string) bool { return strings.HasPrefix(strings.TrimSpace(id), freePrefix) }

// ---------------------------------------------------------------------------
// 动态目录
// ---------------------------------------------------------------------------

type remoteModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

type recommendResp struct {
	Recommended []remoteModel `json:"recommended"`
	Free        []remoteModel `json:"free"`
	ClinePass   []remoteModel `json:"clinePass"`
	ClineCloud  []remoteModel `json:"clineCloud"`
}

// modelsEnvelope 是 /api/v1/models 的信封。实测它是 **OpenRouter 目录的镜像**
// （里面就有 openrouter/auto、openrouter/free 这些 OpenRouter 元模型），
// 且**无需鉴权**。本渠道只用它来**放宽校验**，不用它决定暴露清单。
type modelsEnvelope struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// FetchModels 返回「暴露集」= 静态表 ∪ recommended-models 的 free 数组（按基名去重）。
//
// 同时把两个目录里见到的全部 ID 记进 liveIDs（**只增不减**），供本地校验放宽用 ——
// R42 的教训：只查静态表会把上游新模型误拒成 model_not_found，而它正被 /v1/models 正常列出。
// 撞过共享上限的模型从暴露集里摘除（见 markCapped），解冻后自动回归。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	seen := map[string]bool{}
	out := []provider.ModelInfo{}
	add := func(mi provider.ModelInfo) {
		b := baseName(mi.ID)
		if b == "" || seen[b] || c.isCapped(mi.ID) || isClinePassLike(mi.ID) {
			return
		}
		seen[b] = true
		out = append(out, mi)
	}

	// 静态表**先入**：真实名与 `cline-free/` 别名同基名时必须保留真实名
	//（前者走 Cline 自有网关、配额按账号计、不依赖 X-CLIENT-TYPE，后者走 OpenRouter 共享出口）。
	// 此前先入动态目录会让别名抢占基名，导致硬编码的真实名被去重掉。
	for _, s := range staticFreeModels {
		add(s)
	}

	var live []string

	// ① Cline 官方免费池（权威来源，会随官方调整 ⇒ 硬编码表不必频繁改代码）。
	var rec recommendResp
	if raw, err := c.getJSON(a, EpRecommends); err == nil && json.Unmarshal(raw, &rec) == nil {
		for _, m := range rec.Free {
			if strings.TrimSpace(m.ID) == "" {
				continue
			}
			live = append(live, m.ID)
			if isClineFree(m.ID) {
				name := m.Name
				if name == "" || name == m.ID {
					name = prettifyID(m.ID)
				}
				add(provider.ModelInfo{ID: m.ID, Name: name})
			}
		}
	} else if err != nil {
		// 拉不到不致命：静态表兜底（同 R42 的「无静态兜底」反面教训 —— 这里两者都有）。
		c.rememberUnreachable(err)
	}

	// ② 公开目录：只用于放宽校验，不进入暴露集。
	if raw, err := c.getJSON(a, EpModels); err == nil {
		var me modelsEnvelope
		if json.Unmarshal(raw, &me) == nil {
			for _, m := range me.Data {
				live = append(live, m.ID)
			}
		}
	}
	c.rememberLiveModels(live...)

	// 静态表已在最前，动态部分按 ID 排序保证面板顺序稳定。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	res := append(StaticModels()[:0:0], out...)
	// 重算一次：静态表中被冷却/与动态重基名的已在 add 里处理，这里只保证顺序。
	seen2 := map[string]bool{}
	final := make([]provider.ModelInfo, 0, len(res))
	for _, m := range res {
		b := baseName(m.ID)
		if seen2[b] {
			continue
		}
		seen2[b] = true
		final = append(final, m)
	}
	if len(final) == 0 {
		return nil, fmt.Errorf("cline: 免费模型目录为空（静态表与动态目录都拿不到）")
	}
	return final, nil
}

// RouteOf 按上游响应 id 的形态判断该请求实际走了哪条链。
//
// 实测判据（2026-10-10，9 个 ID 零反例）：
//
//	gen-<unix秒>-<20位Base62> → OpenRouter（Cline 转发给聚合器，响应无 provider_metadata）
//	gen_<ULID 26 位>          → Cline 自有网关（带 marketCost/routing/fallbacksAvailable）
//
// 为什么需要它：配额键名是 `…UsageCostUSDPerUser`（按 Cline 账号计），
// 而 `limit_rpd` 日限落在 **Cline 自己的 OpenRouter key** 上 ⇒ 换 Cline 账号必然复现。
// 二者必须区分处置（见 Classify 的 ErrPassthrough vs ErrSoftRate），而成功响应里
// **没有任何字段直接写着聚合器名字**（is_byok / provider 实测都不等价，不可用作判据）。
func RouteOf(responseID string) string {
	switch {
	case strings.HasPrefix(responseID, routeClineGW):
		return "cline_gateway"
	case strings.HasPrefix(responseID, routeOpenRouter):
		return "openrouter"
	default:
		return "unknown"
	}
}

// isClinePassLike 拦掉订阅池/云池 ID：它们不是本渠道场景，实测也是硬 403
// （ENTITLEMENT_ERROR / "This request is not supported"），列出来只会制造无效请求。
func isClinePassLike(id string) bool {
	return strings.HasPrefix(id, passPrefix) || strings.HasPrefix(id, cloudPrefix)
}

// prettifyID 把 `cline-free/step-5-preview` 这类裸 ID 变成可读展示名。
func prettifyID(id string) string {
	s := id
	for _, p := range freePrefixes {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.ReplaceAll(s, "-", " ")
	if r := []rune(s); len(r) > 0 {
		return strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return s
}

// ---------------------------------------------------------------------------
// 校验集与模型级冷却
// ---------------------------------------------------------------------------

// knownModel 本地校验集合 = 静态表 ∪ 动态目录见过的全部 ID（含公开 :free 长尾）。
// **故意比暴露集宽**：列表保守（不淹没用户）、校验宽松（不误拒手工填的模型）。
func (c *Client) knownModel(id string) bool {
	if KnownModel(id) {
		return true
	}
	c.liveMu.RLock()
	ok := c.liveIDs[id]
	c.liveMu.RUnlock()
	return ok
}

func (c *Client) rememberLiveModels(ids ...string) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.liveIDs == nil {
		c.liveIDs = map[string]bool{}
	}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			c.liveIDs[id] = true
		}
	}
}

// capCooldown 撞共享上限后的默认摘除时长：`limit_rpd` 是**日**上限，给 1 小时足够
// 让面板不至于长期空列表，且每日重置后自动回归；`temporarily rate-limited` 更短就够，
// 统一取一个中等值避免为两种文案各写一条分支（工程实用主义）。
const capCooldown = time.Hour

// markCapped 记录「该模型此刻拿不到」，FetchModels 期间从暴露集摘除。
func (c *Client) markCapped(model string, d time.Duration) {
	if strings.TrimSpace(model) == "" {
		return
	}
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.capped == nil {
		c.capped = map[string]time.Time{}
	}
	// 同基名一并标记：别名与真实名是同一个底层模型，只标一个会漏。
	b := baseName(model)
	for _, s := range []string{model, b} {
		c.capped[s] = time.Now().Add(d)
	}
	for k, t := range c.capped {
		if time.Now().After(t) {
			delete(c.capped, k)
		}
	}
}

// isCapped 按 ID 或基名判定。
func (c *Client) isCapped(model string) bool {
	b := baseName(model)
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	now := time.Now()
	for _, s := range []string{model, b} {
		if t, ok := c.capped[s]; ok && now.Before(t) {
			return true
		}
	}
	return false
}

func (c *Client) rememberUnreachable(err error) { c.lastErr = err }

// liveBaseRe 兜底：从任意形态 ID 提基名做比较（已被 baseName 覆盖，保留给未来形态）。
