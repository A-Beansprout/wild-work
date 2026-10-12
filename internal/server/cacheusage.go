package server

// cacheusage.go 从上游 usage 帧提取「缓存命中 token」——面板「缓存利用率」的唯一取数点。
//
// 为什么单独成文件：这是**唯一**需要按渠道族区分口径的地方，判定规则多且每条都有实测
// 依据（见下）。把它集中在这里，其余调用方（ledger / stats / handler）只拿到
// 「cached / reported」两个数，不必各自理解渠道差异。
//
// 实测依据（2026-10-12，24 份真实上游快照，见 AGENTS.md §8.5 devdump）：
//
//	渠道族                     字段                                          命中示例
//	workbuddy / workbuddyai    prompt_cache_hit_tokens（也回 cached_tokens）  97/103
//	traework / traecode        cache_read_input_tokens                        40960/41610
//	qodercn / qodercom /       prompt_tokens_details.cached_tokens            本轮 0
//	  qwenwork / cline / oczen
//	monkeycode                 input_tokens_details.cached_tokens             本轮 0
//	raccoon                    prompt_tokens_details 为**空对象** {}          → 未上报
//	glm / loomy                整条流无 usage 字段                            → 未上报
//
// 三条必须遵守的语义（否则面板会显示错数）：
//
//  1. **命中数是 prompt_tokens 的子集**，不是并列项。workbuddy 系实测
//     `hit + miss == prompt_tokens`；traework 实测 `total = prompt + completion`
//     且 cache_read(40960) < prompt(41610) ⇒ prompt_tokens 已是**总输入**。
//     故分母直接用 PT，**不可**做 `PT + cacheRead`（那是 Anthropic **官方**口径，
//     与本仓实测的 trae 口径相反）。副产品：利用率恒 ∈ [0,1]，坏数据天然被钳住。
//
//  2. **「字段存在但为 0」与「字段不存在」必须区分**。前者是有意上报的「未命中」
//     （应当显示 0%），后者是上游不提供该维度（应当显示「—」）。raccoon 下发空对象
//     `{}`，即属后者——若只判「有 details 对象就算上报」，面板会给它恒显 0%，
//     把「不知道」伪装成「命中率为零」。
//
//  3. **绝不用 usage 里那些跨渠道不同的重复字段**（workbuddy 系同一帧里同时出现
//     `cached_tokens:0` 与 `prompt_cache_hit_tokens:97`，二者矛盾且前者恒为 0）。
//     只认各自渠道族的主字段，保证取值唯一确定。

// cacheUsage 提取结果。
//
//	reported=false → 上游不提供该维度，面板显示「—」（优雅降级；不得显示 0%）
//	reported=true  → cached 可信（可能为 0，即本轮无命中），面板显示百分比
type cacheUsage struct {
	cached   int64
	reported bool
}

// cacheUsageOf 从 usage 帧提取缓存命中数。
//
// 传入的 u 是**上游原始 usage 对象**（OpenAI 形状；traework 的 SOLO token_usage 亦兼容）。
// u 为 nil 或非 map 时返回「未上报」。
func cacheUsageOf(u map[string]any) cacheUsage {
	if u == nil {
		return cacheUsage{}
	}
	// ① workbuddy / workbuddyai 主字段：prompt_cache_hit_tokens。
	//    实测与 prompt_cache_miss_tokens 互补且和恒等于 prompt_tokens（子集口径）。
	if v, ok := presentNum(u, "prompt_cache_hit_tokens"); ok {
		return cacheUsage{cached: v, reported: true}
	}
	// ② traework / traecode：Anthropic 命名的 cache_read_input_tokens。
	//    注意**只取 read（命中）**，不把 cache_creation（写入）算进命中——
	//    写入是「为下次缓存付的费」，计入命中率会虚高。
	if v, ok := presentNum(u, "cache_read_input_tokens"); ok {
		return cacheUsage{cached: v, reported: true}
	}
	// ③ 其余渠道（qodercn/qodercom/qwenwork/cline/oczen/monkeycode）：
	//    details 子对象里的 cached_tokens。**子对象必须非空且确有该键**——
	//    raccoon 下发空 {} 即在此被正确判为「未上报」（见文件头语义 2）。
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		d, ok := u[key].(map[string]any)
		if !ok || len(d) == 0 {
			continue
		}
		if v, ok := presentNum(d, "cached_tokens"); ok {
			return cacheUsage{cached: v, reported: true}
		}
	}
	return cacheUsage{}
}

// presentNum 取字段值：**键存在且可转数值**才算命中（区别于 numField 的「缺失即 0」）。
// 这是语义 2 的实现基础——必须能分辨「0」与「没有」。
// 上游既有 float64（标准 json 解码）也有 json.Number（网关层解码）两种形态。
func presentNum(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, true // 负数钳 0（坏数据），但仍算「已上报」
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, true
		}
		return n, true
	case int:
		if n < 0 {
			return 0, true
		}
		return int64(n), true
	}
	return 0, false
}
