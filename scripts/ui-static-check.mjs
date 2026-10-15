// ui-static-check.mjs —— 前端静态验证：把假 state 喂给 app.js 的渲染函数，断言 HTML 输出。
//
// 为什么这样做（而不是起真实例）：
//   - 仓库无 JS 测试设施、也无浏览器环境；起真实例会连上游（签到/保活有副作用）；
//   - 我们只关心「函数拿到 state 会不会渲染出该有的 DOM」，纯函数级验证足够；
//   - app.js 顶层有 `let state = null`，故用「源码改写注入」把假 state 塞进去。
//
// 用法：node ui-static-check.mjs
import { readFileSync } from "node:fs";

const raw = readFileSync(new URL("../cmd/wild-work/web/app.js", import.meta.url), "utf8");

// 把 `let state = null;` 改成假数据，并暴露待测函数（app.js 是脚本，函数是顶层声明）。
const FAKE_STATE = {
  version: "2.5.5",
  accounts: [
    {
      uid: "1606848467182378",
      group: "traework",
      nickname: "中招号",
      credits: 2050,
      expiring_credits: 0,
      unusable_credits: 0,
      cooling: true,
      until: "10-03 09:12",
      reason: "stream business error: solo error code=3004 …",
      disabled: false,
      last_checkin_at: "2026-10-02 09:00",
      last_checkin_ok: true,
    },
    {
      uid: "4298452267967801",
      group: "traework",
      nickname: "健康号",
      credits: 2686,
      expiring_credits: 100,
      unusable_credits: 0,
      cooling: false,
      until: "",
      reason: "",
      disabled: false,
      last_checkin_at: "2026-10-02 09:00",
      last_checkin_ok: true,
    },
  ],
  checkin_times: [],
  next_checkin: "-",
};

// ---- 最小 DOM stub ----
const nodes = {};
function mkNode(id) {
  const cls = new Set();
  return {
    id,
    innerHTML: "",
    textContent: "",
    style: {},
    classList: {
      add: (c) => cls.add(c),
      remove: (c) => cls.delete(c),
      contains: (c) => cls.has(c),
      _set: cls,
    },
    // 运行日志容器（renderAppLog 吸底判定需要；stub 下 pinned 恒 true，只验证 HTML 生成）
    scrollTop: 0,
    scrollHeight: 100,
    clientHeight: 50,
    onmouseenter: null,
    onmouseleave: null,
    // querySelector 供 renderModelTable 取 tbody；返回独立 stub（其 innerHTML
    // 会被断言读取，故必须与父节点区分开）。
    querySelector: (sel) => (nodes[id + sel] ||= mkNode(id + sel)),
  };
}
const document = {
  getElementById: (id) => (nodes[id] ||= mkNode(id)),
  querySelectorAll: () => [],
  addEventListener: () => {},
  createElement: () => mkNode("stub-" + Math.random()),
  body: { appendChild: () => {} },
};
const window = { addEventListener: () => {}, location: { search: "" } };

// 源码改写：注入 state + 导出函数
const patched =
  raw.replace(/let state = null;/, "let state = " + JSON.stringify(FAKE_STATE) + ";")
  // 模型查找面板：预置一份费率缓存（正常由 loadFees 填充），供 renderModelSearch 过滤。
  + "\n; lastFees = " + JSON.stringify({ channels: [
    { channel: "qodercn", models: [
      { model: "qwen3.8-flash", priced: true, rate: 0.3, free: false, has_context: true, context_window: 180000, supports_images: false, supports_reasoning: false },
      { model: "glm-5.3", priced: true, rate: 0.5, free: false, has_context: true, context_window: 200000, supports_images: true, supports_reasoning: true,
        reasoning_efforts: ["low", "high", "max"], default_reasoning_effort: "high" },
    ] },
    { channel: "workbuddy", models: [
      { model: "deepseek-v4", priced: false, rate: 0, free: false, has_context: false },
      { model: "muse-free", priced: true, rate: 0, free: true, has_context: true, context_window: 128000 },
    ] },
    // 溢出场景：另 12 个 qwen* 模型，与 qodercn/qwen3.8-flash 合并共 13 条命中 > 上限 12
    { channel: "loomy", models: Array.from({ length: 12 }, (_, i) =>
      ({ model: `qwen-bulk-${i}`, priced: true, rate: 0.1, free: false, has_context: false })) },
    // 上游 ID 自带命名空间的渠道（cline 免费档）：完整 ID 取服务端给的 id
    // （= 上游 ID 原样 `cline-free/*`，不再拼 `cline/`；决议 R59）
    { channel: "cline", models: [
      { model: "cline-free/mimo-v2.6-flash", id: "cline-free/mimo-v2.6-flash", priced: true, rate: 0, free: true, has_context: true, context_window: 1000000 },
    ] },
  ] }) + ";"
  + "\n; globalThis.__T = { renderAccounts, renderSummary, loadStats, renderStats, renderPlatformTable, renderRotation, renderExpiry, renderModels, renderLogs, renderAbnormal, renderFallbacks, renderEffList, renderAppLog, handleToasts, renderModelSearch, collectAllModels, cacheCell, renderUsageCacheCard, renderModelTable, renderUsage, renderFees };";

new Function("document", "window", "localStorage", "fetch", "location", "navigator", patched)(
  document,
  window,
  { getItem: () => null, setItem: () => {} },
  async () => ({ ok: true, json: async () => ({}) }),
  window.location,
  { userAgent: "node" }
);

const T = globalThis.__T;
const results = [];
const check = (name, pass, extra = "") => results.push([name, pass, extra]);

// ---- 场景 1：账号卡片的冷却可见性 ----
T.renderAccounts();
const cards = document.getElementById("acctList").innerHTML;
check("冷却标签出现（冷却至 …）", /冷却至/.test(cards));
check("冷却时间正确显示", cards.includes("10-03 09:12"));
check("冷却原因进 title（含 3004）", cards.includes("3004"));
check("冷却卡片带 cooling 类（左侧琥珀边）", /acct-card cooling/.test(cards));
check("冷却标签用 warn 样式", /tag warn/.test(cards));
check(
  "只有冷却账号带冷却标签（健康号不带）",
  (cards.match(/冷却至/g) || []).length === 1,
  `实际 ${(cards.match(/冷却至/g) || []).length} 个`
);
check("健康号仍在（未被过滤）", cards.includes("健康号"));

// ---- 场景 2：渠道汇总条 ----
T.renderSummary();
const sum = document.getElementById("creditSummary").innerHTML;
check("汇总条渲染了 traework", sum.includes("TraeWork"));
check("汇总可用 = 2050+2686 = 4736", sum.includes("4,736"), sum.slice(0, 160));
check("汇总临期 = 100", sum.includes("临期100"));
check("汇总含合计", sum.includes("合计"));

// ---- 场景 2.5：模型查找面板（纯前端过滤，固定高度/限量/超出提示） ----
// 假缓存共 17 个模型：qodercn 2 + workbuddy 2 + loomy 12 + cline 1（含 13 个 qwen* 用于溢出断言）
check("collectAllModels 展平全渠道 17 个模型", T.collectAllModels().length === 17, `实际 ${T.collectAllModels().length}`);
check("collectAllModels 完整 ID = 渠道/裸名", T.collectAllModels()[0].full === "qodercn/qwen3.8-flash");
// 上游 ID 自带命名空间（cline-free/*）时完整 ID = 上游 ID 原样（服务端 m.id 同源），
// 不再拼 `cline/`（R59）；旧的 cline/cline-free/* 仍可路由（兼容断言在 Go 侧）
check("上游命名空间 ID 直通上游前缀（cline-free/*）",
  T.collectAllModels().some((x) => x.full === "cline-free/mimo-v2.6-flash"),
  JSON.stringify(T.collectAllModels().filter((x) => x.ch === "cline")));
T.renderModelSearch("glm");
let msHtml = document.getElementById("modelSearchList").innerHTML;
check("搜索 glm 命中 1 条", (msHtml.match(/class="ms-row"/g) || []).length === 1, msHtml.slice(0, 120));
check("命中项 data-mid 为完整 ID", msHtml.includes('data-mid="qodercn/glm-5.3"'));
check("命中项显示渠道徽章与费率", msHtml.includes("QoderCN") && msHtml.includes("x0.50"));
check("命中项显示思考档位（issue #96）", msHtml.includes('class="ms-eff"') && msHtml.includes("low·high·max"), msHtml.slice(0, 300));
check("档位 title 含默认档", msHtml.includes("默认 high"));
check("底部提示共 N 个匹配", /共 1 个匹配/.test(document.getElementById("modelSearchFoot").textContent));
T.renderModelSearch("");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("空关键字受上限约束只列 12 条", (msHtml.match(/class="ms-row"/g) || []).length === 12, `实际 ${(msHtml.match(/class="ms-row"/g) || []).length}`);
check("空关键字提示可输关键字缩小范围", /输入关键字可缩小范围/.test(document.getElementById("modelSearchFoot").textContent), document.getElementById("modelSearchFoot").textContent);
check("免费模型显示 Free", msHtml.includes("Free"));
check("未定价模型显示 unknown", msHtml.includes("unknown"));
T.renderModelSearch("zzz-none");
check("无匹配时提示无匹配", /无匹配/.test(document.getElementById("modelSearchList").innerHTML));
// 限量：13 个 qwen* 命中 > 上限 12 → 只渲染 12 条，底部提示还有未显示
T.renderModelSearch("qwen");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("超上限时只渲染 12 条", (msHtml.match(/class="ms-row"/g) || []).length === 12, `实际 ${(msHtml.match(/class="ms-row"/g) || []).length}`);
check("超上限时底部提示未显示数量", /还有 1 个未显示/.test(document.getElementById("modelSearchFoot").textContent), document.getElementById("modelSearchFoot").textContent);

// ---- 场景 2.7：费率表思考档位标记（issue #96，与 /v1/models + /api/fees 同源） ----
T.renderFees({ channels: [
  { channel: "qodercn", models: [
    { model: "glm-5.3", priced: true, rate: 0.5, free: false, has_context: true, context_window: 200000,
      supports_reasoning: true, reasoning_efforts: ["low", "high", "max"], default_reasoning_effort: "high" },
    { model: "qwen3.8-flash", priced: true, rate: 0.3, free: false, has_context: true, context_window: 180000, supports_reasoning: false },
  ] },
  // 上游 ID 自带命名空间（cline 免费档）：完整 ID = 上游 ID 原样
  { channel: "cline", models: [
    { model: "cline-free/mimo-v2.6-flash", id: "cline-free/mimo-v2.6-flash", priced: true, rate: 0, free: true, has_context: true, context_window: 1000000 },
  ] },
] });
const feesHtml = document.getElementById("feesBox").innerHTML;
check("费率表档位小字标出现（[low·high·max]）", feesHtml.includes("effort-tag") && feesHtml.includes("[low·high·max]"), feesHtml.slice(0, 200));
check("档位 title 含默认档与调档提示", feesHtml.includes("默认 high") && feesHtml.includes("reasoning_effort"));
check("无档位声明的模型不显示档位标", (feesHtml.match(/effort-tag/g) || []).length === 1);
// 完整 ID（R59）：上游命名空间 ID 直接透传（服务端 m.id 同源），复制出去即可用
// （旧的 cline/cline-free/* 仍被服务端路由兼容，但对外只广告新形态）
check("费率表上游命名空间 ID 直通（cline-free/*）",
  feesHtml.includes('data-mid="cline-free/mimo-v2.6-flash"'), feesHtml.slice(0, 300));

// ---- 场景 2.75：模型查找面板无档位声明时不显示档位 ----
T.renderModelSearch("deepseek");
msHtml = document.getElementById("modelSearchList").innerHTML;
check("未声明档位的模型行无 ms-eff", !msHtml.includes("ms-eff"), msHtml.slice(0, 200));

// ---- 场景 3：运行统计（/api/stats 载荷 → 各区块渲染） ----
// 载荷形状 = internal/stats 引擎 Snapshot 的 JSON（与 sidecar /capi/stats 对齐）
const FAKE_STATS = {
  version: 1, today: "2026-10-06",
  total_credits: 4736, total_accounts: 2,
  credit_in: 200, credit_out: 150, credit_out_exact: 150.5, credit_expired: 100, credit_used: 50,
  tokens: 12345, reqs: 10,
  token_usage: { today: 9000, days7: 50000, days30: 120000, all: 200000, req_today: 8, req_all: 90, days: 12 },
  expiring_7d: 300, expiring_7d_accts: 1, expire_today: 100, expire_tomorrow: 200,
  platforms: [{
    group: "traework", accounts: 2, credits: 4736, credit_in: 200, credit_out: 150,
    credit_expired: 100, credit_used: 50, tokens: 12345, expire_7d: 300, expire_today: 100, expire_tomorrow: 0,
  }],
  models: [{ model: "traework/DeepSeek-V4", reqs: 3, tokens: 5000, credit: 12.5, avg_ttfb_ms: 800, avg_tok_per_sec: 45.2, perf_samples: 3 }],
  expiry: [{ uid: "4298452267967801", nickname: "健康号", group: "traework", expire_at: "2026-10-08", days_left: 2, amount: 100, today_out: 30, burn_forecast: 45, risk: true }],
  rotation: [{
    group: "traework",
    current: { uid: "1606848467182378", nickname: "中招号", group: "traework", credits: 2050, expiring: 0, sticky: "12/50" },
    next: [{ uid: "4298452267967801", nickname: "健康号", group: "traework", credits: 2686, expiring: 100 }],
  }],
  abnormal: [
    { uid: "1606848467182378", nickname: "中招号", group: "traework", level: "cool", reason: "stream business error: code=3004", until: "10-08 09:12", err_count: 0, credits: 2050, updated_at: "2026-10-06 12:00:00" },
    { uid: "4298452267967801", nickname: "健康号", group: "traework", level: "err", reason: "", until: "", err_count: 3, credits: 2686, updated_at: "2026-10-06 12:00:00" },
  ],
  milestone_total: 100, milestone_count: 1,
  toasts: [], // 空：本场景不触 Notification/面板 toast 路径
};

T.renderStats(FAKE_STATS);
const stSum = document.getElementById("stSum").innerHTML;
check("统计汇总条渲染总积分（4,736）", stSum.includes("4,736"));
check("汇总条含账号数注解", stSum.includes("2 个账号"));
check("今日作废仅 >0 时出现", stSum.includes("今日作废"));
check("今日到期仅 >0 时出现", stSum.includes("今日到期"));
check("里程碑计数正确", document.getElementById("stMilestone").textContent === "里程碑 1×100分");
const stToken = document.getElementById("stToken").innerHTML;
check("Token 多天行含今日/7日/已记录", stToken.includes("Token 今日") && stToken.includes("7日") && stToken.includes("已记录") && stToken.includes("12 天"));

const stPlat = document.getElementById("stPlat").innerHTML;
check("平台表渲染渠道徽章（traework）", /badge trae"/.test(stPlat));
check("平台表含合计行（全池口径 4,736）", stPlat.includes("合计") && stPlat.includes("4,736"));
check("平台表零值列置灰（st-plat-zero）", stPlat.includes("st-plat-zero"));

const stRotation = document.getElementById("stRotation").innerHTML;
check("接棒顺序渲染使用中账号", stRotation.includes("使用中 <b>中招号</b>"));
check("接棒顺序含 ① 候补", stRotation.includes("① 健康号"));

const stExpiry = document.getElementById("stExpiry").innerHTML;
check("临期风险行高亮（st-risk）", stExpiry.includes("st-ex-row st-risk"));
check("临期剩余天数文案", stExpiry.includes("2 天后到期"));
check("预计作废量 = 额度−外推消耗（55分）", stExpiry.includes("预计作废 55分"));

const stModels = document.getElementById("stModels").innerHTML;
check("Trae 系模型积分为估算（≈ 前缀）", stModels.includes("≈12.50"));
check("模型平均速度含 tok/s 与 TTFB", stModels.includes("45.2 tok/s") && stModels.includes("800ms"));

T.renderAbnormal(FAKE_STATS.abnormal);
const stAbnormal = document.getElementById("stAbnormal").innerHTML;
check("异常账号渲染整号冷却标签", stAbnormal.includes("整号冷却"));
check("错误累计显示连续次数", stAbnormal.includes("连续错误 3 次"));

// ---- 场景 4：请求日志（降序 / 状态标红 / tok/s 占位） ----
const FAKE_LOGS = [
  { seq: 1, time: "10-06 12:00:00", model: "traework/x", mode: "流", status: 200, ttfb_ms: 500, tok: 100, tok_per_sec: 20.5, total_sec: 3.2, credit: 1.5 },
  { seq: 2, time: "10-06 12:01:00", model: "qodercn/y", mode: "非流", status: 500, ttfb_ms: 800, tok: 0, tok_per_sec: 0, total_sec: 0, credit: 0 },
];
T.renderLogs(FAKE_LOGS);
const stLogs = document.getElementById("stLogs").innerHTML;
check("请求日志降序（最新 seq 在前）", stLogs.indexOf("<td>2</td>") < stLogs.indexOf("<td>1</td>"));
check("≥400 状态标红（st-bad）", stLogs.includes("st-bad"));
check("非流式 tok/s 显示「—」", stLogs.includes("—"));

// ---- 场景 5：运行日志（级别配色 / 时间前缀剥离） ----
const FAKE_APPLOG = [
  "2026/10/06 12:00:00.123 checkin platform=traework ok=false code=9074 msg=已签到",
  "2026/10/06 12:00:01.000 批量签到完成：total=20 ok=19 failed=1",
];
T.renderAppLog(FAKE_APPLOG);
const stApplog = document.getElementById("stApplog").innerHTML;
check("运行日志剥离年份前缀", stApplog.includes("10/06 12:00:00") && !stApplog.includes("2026/10/06"));
check("code=9074/ok=false 行标红", (stApplog.match(/st-log-err/g) || []).length === 1);
check("汇总成功行（含 failed=N）按 info 放行", (stApplog.match(/st-log-info/g) || []).length === 1);

// ---- 场景 6：里程碑 toast 去重（同 id 二次轮询不重复弹） ----
// 主 stub 的 localStorage 无状态，去重断言需带状态的存储：
// 用同一份 patched 源码再实例化一次（隔离作用域），以「已读标记写入次数」断言——
// 两次 handleToasts 同 id → 第一次写 1 次标记，第二次读到已读跳过 → 共 1 次写入。
let toastWrites = 0;
const lsStore = {};
const lsStub = {
  getItem: (k) => (k in lsStore ? lsStore[k] : null),
  setItem: (k, v) => { lsStore[k] = v; toastWrites++; },
};
new Function("document", "window", "localStorage", "fetch", "location", "navigator",
  patched + "\n; globalThis.__DedupT = handleToasts;")(
  document, window, lsStub, async () => ({ ok: true, json: async () => ({}) }), window.location, { userAgent: "node" });
globalThis.__DedupT([{ id: "2026-10-06-m1", title: "里程碑", body: "100分" }]);
globalThis.__DedupT([{ id: "2026-10-06-m1", title: "里程碑", body: "100分" }]);
check("里程碑 toast 同 id 去重（只写一次已读标记）", toastWrites === 1, `实际写入 ${toastWrites} 次`);

// ---- 静态结构断言：index.html / style.css 的 stats 骨架 ----
const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
check('第三个 tab 按钮存在（data-mtab="stats"）', /data-mtab="stats"/.test(html));
check("#mtabStats body 存在且默认隐藏", /class="mtab-body hidden" id="mtabStats"/.test(html));
check("运行统计区块骨架存在（stSum/stPlat/stApplog）", /id="stSum"/.test(html) && /id="stPlat"/.test(html) && /id="stApplog"/.test(html));
check("更新角标存在（id=stUpdated，拉取失败可感知）", /id="stUpdated"/.test(html));
// ---- 第四个 tab：模型列表和费率（原独立 section 移入主 tab 组）----
check('第四个 tab 按钮存在（data-mtab="fees"）', /data-mtab="fees"/.test(html));
check("#mtabFees body 存在且默认隐藏", /class="mtab-body hidden" id="mtabFees"/.test(html));
check("模型费率区块骨架存在（feesBox/btnRefreshFees）", /id="feesBox"/.test(html) && /id="btnRefreshFees"/.test(html));
check("模型费率区块在 section 内（不再是独立 section）", /<div class="mtab-body hidden" id="mtabFees">[\s\S]*?<\/section>/.test(html) && !/<!-- 模型列表和费率 -->\s*<section class="panel">/.test(html));
const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
check("style.css 含运行统计样式段（#mtabStats .st-sum 等）", /#mtabStats \.st-sum/.test(css) && /#mtabStats \.st-plat-row/.test(css));
check("更新角标失败警示样式存在（#stUpdated.st-stale）", /#stUpdated\.st-stale/.test(css));
// ---- 用量 tab 五卡同行（5 列 grid，不换行）----
check("用量卡片 5 列同行（.usage-cards repeat(5)）", /\.usage-cards \{[^}]*grid-template-columns: repeat\(5, 1fr\)/.test(css));

// ---- 场景 3.5：缓存利用率的**三态降级**（本列的核心就是不能把「不知道」显示成 0%） ----
// 语义务必与 internal/server/cacheusage.go + internal/ledger 的 CR/CRC 保持一致：
//   cr_cov > 0 → 百分比（允许 0%，那是真未命中）
//   cr_cov = 0 → 「—」（上游不提供该维度：glm / raccoon）
{
  const T2 = globalThis.__T;
  // ① 有上报样本：命中 97 / 分母 103 → 94.17%
  const withHit = T2.cacheCell({ cr: 97, cr_cov: 103, cr_reqs: 2 });
  check("有上报样本时显示百分比（97/103 → 94.2%）", /94\.2%/.test(withHit), withHit);
  check("百分比带样本量 title", /2 次上报请求/.test(withHit), withHit);
  // ② 上报了但命中为 0：显示 0.00%，**不是** 「—」（这两者必须区分）
  const zeroHit = T2.cacheCell({ cr: 0, cr_cov: 500, cr_reqs: 3 });
  check("上报但未命中显示 0%（不显示「—」）", /0\.00%/.test(zeroHit) && !/—/.test(zeroHit), zeroHit);
  // ③ 未上报（glm / raccoon）：显示「—」，且**绝不能**出现百分比
  const noReport = T2.cacheCell({ cr: 0, cr_cov: 0, cr_reqs: 0 });
  check("未上报时显示「—」而非 0%", noReport.includes("—") && !/%/.test(noReport), noReport);
  const missing = T2.cacheCell({});
  check("字段整体缺失（老流水）也显示「—」", missing.includes("—") && !/%/.test(missing), missing);
  // ④ 汇总卡同款三态
  T2.renderUsageCacheCard({ cr: 50, cr_cov: 200, cr_reqs: 4 });
  check("汇总卡有样本时显示 25.0%", document.getElementById("ucCache").textContent === "25.0%", document.getElementById("ucCache").textContent);
  T2.renderUsageCacheCard({});
  check("汇总卡无样本时显示「—」", document.getElementById("ucCache").textContent === "—", document.getElementById("ucCache").textContent);
  // ⑤ 模型表真渲染一列（列数从 5 → 6，占位行 colspan 同步）
  T2.renderModelTable([
    { model: "glm-5.3", channel: "workbuddy", requests: 2, pt: 200, ct: 100, cr: 150, cr_cov: 200, cr_reqs: 2 },
    { model: "chatglm", channel: "glm", requests: 1, pt: 50, ct: 10 },
  ], 1);
  const rowsHtml = document.getElementById("tblModelstbody").innerHTML;
  check("模型表渲染缓存列（命中行 75.0%）", /75\.0%/.test(rowsHtml), rowsHtml.slice(0, 200));
  check("模型表对不上报渠道显示「—」", rowsHtml.includes("—"));
  check("模型表渲染平均速度列（无速度样本显示「—」）", (rowsHtml.match(/—/g) || []).length >= 2, rowsHtml.slice(0, 300));
  T2.renderModelTable([], 1);
  check("模型表空态占位行跨 7 列", document.getElementById("tblModelstbody").innerHTML.includes('colspan="7"'));
}

// ---- 输出 ----
let failed = 0;
for (const [name, pass, extra] of results) {
  console.log(`  ${pass ? "✅" : "❌"} ${name}${extra && !pass ? "  → " + extra : ""}`);
  if (!pass) failed++;
}

// ---- 静态结构断言：同名顶层函数/常量不得重复定义 ----
// 起因（2026-10-03 复核发现）：renderSummary 曾被定义两次，前者成死代码；
// node --check 与功能断言都发现不了（后者只测生效的那版）。
// 一旦有人"修好"死的那版，面板数字会静默变化。
const uniq = [
  ["function renderSummary", /^\s*function renderSummary\s*\(/gm],
  ["const CH_ORDER", /^\s*const CH_ORDER\s*=/gm],
  ["const CH_SUMMARY_ORDER", /^\s*const CH_SUMMARY_ORDER\s*=/gm],
  ["function renderAccounts", /^\s*function renderAccounts\s*\(/gm],
  ["function render()", /^\s*function render\s*\(/gm],
  // 运行统计区块（防「同一提交里写两版」的非预期重复，同 renderSummary 教训）
  ["function loadStats", /^\s*async function loadStats\s*\(/gm],
  ["function renderStats", /^\s*function renderStats\s*\(/gm],
  ["function renderPlatformTable", /^\s*function renderPlatformTable\s*\(/gm],
  ["function renderRotation", /^\s*function renderRotation\s*\(/gm],
  ["function renderExpiry", /^\s*function renderExpiry\s*\(/gm],
  ["function renderModels", /^\s*function renderModels\s*\(/gm],
  ["function renderLogs", /^\s*function renderLogs\s*\(/gm],
  ["function renderAbnormal", /^\s*function renderAbnormal\s*\(/gm],
  ["function renderAppLog", /^\s*function renderAppLog\s*\(/gm],
  ["function handleToasts", /^\s*function handleToasts\s*\(/gm],
];
for (const [label, re] of uniq) {
  const n = (raw.match(re) || []).length;
  // 判定「不得 >1」而非「必须 ==1」：某些定义（如 CH_ORDER）本就不该存在，
  // 删掉后是 0 次也正常；这里防的是"同一提交里写了两版"这种非预期重复。
  const pass = n <= 1;
  console.log(`  ${pass ? "✅" : "❌"} 顶层定义不重复：${label}（${n} 次）`);
  if (!pass) failed++;
}

// ---- 渠道接线完整性（cline）----
// 面板新增一个渠道最容易漏的是「按钮 ↔ 弹层 ↔ 提交函数」这条链：
// 漏了不报错，只表现为「渠道能选到但没入口」或「弹层弹出来但点没反应」——
// 与 R34/R41 同族的「声明了却没接线」，故在此静态锁住。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    ["index.html 有 Cline 添加按钮", html.includes('id="btnAddCline"')],
    ["index.html 有 API Key 粘贴弹层", html.includes('id="clineOverlay"')],
    ["弹层含 Key 输入框", html.includes('id="clineKeyInput"')],
    ["弹层写明申请办法（app.cline.bot）", html.includes("app.cline.bot")],
    ["弹层警告「必须多个独立账号」", /多个独立账号|多个不同.*账号/.test(html)],
    ["app.js 接线了 btnAddCline", raw.includes('$("btnAddCline").onclick')],
    ["app.js 接线了提交/取消按钮", raw.includes('$("btnClineSubmit").onclick') && raw.includes('$("btnClineCancel").onclick')],
    ["app.js 调 /api/login/cline_key", raw.includes("/api/login/cline_key")],
    ["cline 在渠道名/样式表里登记", /cline: "Cline"/.test(raw) && /cline: "cline"/.test(raw)],
    ["cline 归入无签到渠道", /NO_EXPLICIT_CHECKIN[^)]*"cline"/.test(raw)],
    ["cline 无余额概念→积分显示不适用", /NO_CREDITS[^)]*"cline"/.test(raw)],
    ["备用上游通道入口在位（单一输入 + 生效位回显 + 保存走 /api/config/proxy）",
      html.includes('id="backupProxyInput"') && html.includes('id="backupProxyState"')
        && raw.includes('"/api/config/proxy"') && raw.includes("backup_proxy_active")],
    // issue #117：面板「测试」按钮原先直连上游（实际对话走备用通道），用户配了代理点测试
    // 仍看到 403 且无从判断代理是否生效。现已与真实对话同源，并把出口（直连/备用）如实展示。
    ["OC Zen 测试按钮展示出口（直连/备用）——后端同源编排 + 前端读 via_backup",
      raw.includes("via_backup") && raw.includes("经备用通道")],
    ["hint 写明测试与真实对话同源（否则用户仍会误读 403）",
      html.includes("与真实对话同源") && html.includes("自动改走备用通道")],
    ["旧「每渠道代理」输入表已清除（R57：不再有 PROXY_CHANNELS / proxyList）",
      !raw.includes("PROXY_CHANNELS") && !html.includes('id="proxyList"') && !raw.includes("/api/config/proxies")],
    ["style.css 有 cline 渠道色", css.includes(".badge.cline") && css.includes(".btn.primary.alt.cline")],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 小浣熊签到接线（R65）----
// 与 R34/R41 同族：签到接入横跨「渠道实现 + 调度器配置 + noExplicitCheckin + 前端集合」四处，
// 漏任意一处都**不报错**——只表现为「按钮是灰的」或「定时不跑」，极难发现，故在此静态锁住。
// （渠道实现与调度器配置由 Go 侧锁：internal/raccoon/checkin_test.go、internal/app 的 noExplicitCheckin 断言。）
{
  const need = [
    ["raccoon 不在无签到集合（否则手动按钮永久置灰、批量签到静默跳过）",
      !/NO_EXPLICIT_CHECKIN[^)]*"raccoon"/.test(raw)],
    ["raccoon 仍保留渠道名/样式登记（防止误删）",
      /raccoon: "小浣熊"/.test(raw) && /raccoon: "raccoon"/.test(raw)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 积分明细浮窗点击式 + 手机竖屏响应式 ----
// 与 R34/R41 同族：这两处都是「改了交互/样式但没接线/被后写规则覆盖」就静默失效，
// 且宽度断点一旦被人调大就会重新弄坏桌面端（本次就是靠像素级 diff 才发现过一次），故在此锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    // 积分明细：点击式。hover 版在「浮窗开着时后台 loadState() 重建卡片 DOM」会让
    // mouseleave 不再派发，浮窗永久残留（用户反馈的「经常不能自动关闭」），触屏更是关不掉。
    ["明细标签已改为点击触发（不再用 onmouseenter）", raw.includes('onclick="showCreditDetail(event,')],
    ["明细标签不再残留 hover 处理器", !raw.includes("onmouseenter=")],
    ["hover 关闭函数与定时器已删除（防回退；注释提及不算）", !/\bdetailTimer\b/.test(raw.replace(/^\/\/.*$/gm, "")) && !raw.includes("hideCreditDetail")],
    ["浮窗渲染右上角关闭按钮", raw.includes('data-close-credit-tip="1"')],
    ["存在统一关闭入口 closeCreditDetail", /function closeCreditDetail\(/.test(raw)],
    ["存在序号竞态守卫 detailOpenSeq（连点两账号只让最后一次上屏）", /let detailOpenSeq = 0;/.test(raw) && raw.includes("seq !== detailOpenSeq")],
    ["外部关闭路径已绑定一次", raw.includes("bindCreditDetailDismiss()")],
    ["关闭判定用 composedPath（syncCreditsFromDetail 会在派发途中重写积分格，e.target 会变 detached）",
      /bindCreditDetailDismiss[\s\S]{0,900}composedPath/.test(raw) && !/closest\("\.acct-credits"\)/.test(raw)],
    ["切主 tab 仍会收起浮窗", /切换主 tab 强制收起积分明细浮窗[\s\S]{0,80}closeCreditDetail\(\)/.test(raw)],
    [".credit-tip 不再重复定义（原两块，后者静默覆盖前者）", (css.match(/^\.credit-tip \{/gm) || []).length === 1],
    ["浮窗宽度自适应视口（手机不再溢出）", /width: min\(420px, calc\(100vw - 24px\)\)/.test(css)],
    // 响应式：断点是按实测溢出阈值卡位的，调宽会破坏桌面端逐像素一致性，调窄则手机仍溢出。
    ["渠道按钮基础为 6 列（PC 默认宽度 6+5 两行，消除第三行孤行）", css.includes("repeat(6, auto)")],
    ["渠道按钮摆位全归 CSS（index.html 不再内联 grid-column，否则窄屏无法重排）", !/btnAdd\w+" style="grid-column/.test(html)],
    ["渠道按钮 DOM 顺序 = 列配对（同列上下 = 同渠道国内/国际版；Cline 占第一行末位）",
      /btnAddWB\b[\s\S]*?btnAddQoderCN\b[\s\S]*?btnAddTrae\b[\s\S]*?btnAddMonkeyCode\b[\s\S]*?btnAddLoomy\b[\s\S]*?btnAddCline\b[\s\S]*?btnAddWBAI\b[\s\S]*?btnAddQoderCOM\b[\s\S]*?btnAddQwen\b[\s\S]*?btnAddRaccoon\b[\s\S]*?btnAddGLM\b/.test(html)],
    ["窄屏渠道按钮自适应列（auto-fit，118px 可在 320px 竖屏排出两列）", css.includes("repeat(auto-fit, minmax(118px, 1fr))")],
    ["按钮紧凑档字号（12px，PC 一行放下 6 个）", /\.panel-actions-wrap \.btn \{ font-size: 12px;/.test(css)],
    // Cline 实测元数据链路：Go 侧（measuredCaps 表 / ContextFromAPI=false / solar 不支持图像）
    // 由 internal/cline 的单测锁住（TestFetchModelsAppliesMeasuredCaps），此处只管前端文案。
    ["添加窗口含实测参数表且标注仅供参考",
      /免费模型参数（本工具实测验证值，仅供参考）/.test(html) && /仅作参考/.test(html)],
    ["添加窗口说明 muse 靠备用上游通道解地区限制",
      /地区限制/.test(html) && /备用上游通道/.test(html)],
    ["顶栏 1fr 下界解除（minmax(0,1fr)）", /\.top-info \{[^}]*grid-template-columns: minmax\(0, 1fr\)/.test(css)],
    ["账号卡片允许收缩（minmax(0,1fr)）", /\.acct-grid \{ grid-template-columns: minmax\(0, 1fr\); \}/.test(css)],
    ["费率表窄屏隐藏第 3/4 列（4 列布局手机必溢出）", /\.fees table th:nth-child\(n\+3\)[\s\S]*?display: none/.test(css)],
    ["运行统计宽表给横向滚动容器", /#mtabStats \.st-plat,\s*\n\s*#mtabStats \.st-ex-scroll,\s*\n\s*#mtabStats \.st-ab-scroll,\s*\n\s*#mtabStats \.st-scroll \{ max-width: 100%; overflow-x: auto; \}/.test(css)],
    ["触控目标仅在粗指针设备放大（桌面鼠标不吃）", /@media \(hover: none\) and \(pointer: coarse\)/.test(css)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 模型降级链（PR #106）：面板接线 + 降级记录渲染 ----
// 与 R34/R41 同族：「后端有端点、面板没接线」不报错不崩溃，只表现为
// 配不了降级链 / 配了不生效，故把接线与渲染一并锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  T.renderFallbacks([
    { from: "workbuddyai/deepseek-v4.1-flash", to: "workbuddy/glm-5.2", at: "09:12:03", at_unix: 1760000000, active: true },
    { from: "oczen/big-pickle", to: "workbuddy/glm-free", at: "08:01:00", at_unix: 1759990000, recovered: "08:05:12", dur_sec: 252, active: false },
  ]);
  const fbHtml = nodes["stFbs"].innerHTML;
  const need = [
    ["设置弹层有降级链编辑区", html.includes('id="fbList"') && html.includes('id="fbFromInput"') && html.includes('id="fbToInput"')],
    ["app.js 接线添加/保存按钮", raw.includes('$("btnFbAdd").onclick') && raw.includes('$("btnFbSave").onclick')],
    ["app.js 调 POST /api/config/model_fallback", raw.includes('"/api/config/model_fallback"')],
    ["openSettings 从 state.model_fallback 初始化编辑态", /fbRules = JSON\.parse\(JSON\.stringify\(state\.model_fallback \|\| \[\]\)\)/.test(raw)],
    ["运行统计有降级记录区块且 loadStats 渲染它", html.includes('id="stFbBlock"') && raw.includes("renderFallbacks(s.fallbacks")],
    ["进行中事件渲染「降级中」+ 实时计时锚点 data-fb-epoch", fbHtml.includes("降级中") && fbHtml.includes('data-fb-epoch="1760000000"')],
    ["已恢复事件渲染恢复时刻与持续时长", fbHtml.includes("08:05:12") && fbHtml.includes("4 分 12 秒")],
    ["原/备胎模型与箭头均在行内", fbHtml.includes("workbuddyai/deepseek-v4.1-flash") && fbHtml.includes("→")],
    ["空事件常显占位说明（不整块隐藏）", (T.renderFallbacks([]), nodes["stFbs"].innerHTML.includes("暂无降级记录"))],
    ["行内主模型 ID 是可编辑输入框（fb-from-edit + data-edit-idx）", raw.includes('fb-from-edit') && raw.includes('data-edit-idx')],
    ["行内主模型编辑有 change 提交接线（校验+回滚）", raw.includes('classList.contains("fb-from-edit")') && raw.includes("已有规则（每个主模型只允许一条）")],
    // PR #127（R54 触发判据扩展）：文案必须覆盖「整渠道全灭」而非只说「模型全冷」，
    // 否则用户在渠道全灭时看到 503 会以为降级链没配好。
    ["降级链 hint 写明两种触发形态（模型全冷 / 整渠道全灭）",
      /无账号可承载[\s\S]{0,120}整渠道/.test(html)],
    ["降级记录区块标题不再写死「主模型全冷」", !html.includes("主模型全冷切备胎")],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 思考强度注入（issue #96）：设置弹层接线与三态语义文案 ----
// 与降级链同族：「后端有端点、面板没接线」不报错不崩溃，只表现为配不了
// 档位 / 配了不生效，故把编辑区、建议集与接线一并锁死。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const need = [
    ["设置弹层有思考强度编辑区（全局默认行 + 映射列表 + 输入行）",
      html.includes('id="effGlobalInput"') && html.includes('id="effList"') && html.includes('id="effModelInput"') && html.includes('id="effEffortInput"') && html.includes('id="btnEffAdd"') && html.includes('id="btnEffSave"')],
    ["档位建议集 datalist = none/low/medium/high/max（哑管道：输入框仍可自由输入）",
      /<datalist id="effortDatalist">[\s\S]*?value="none"[\s\S]*?value="low"[\s\S]*?value="medium"[\s\S]*?value="high"[\s\S]*?value="max"/.test(html)
      && html.includes('list="effortDatalist"')],
    ["hint 含三态语义（未带才注入 / 显式传不覆盖 / 回退全局默认）",
      html.includes("客户端请求<b>未带</b>") && html.includes("一律不覆盖") && html.includes("精确未命中回退全局默认")],
    ["hint 含 channel/model 格式与 TraeWork 无效说明",
      html.includes("channel/model") && html.includes("TraeWork 渠道协议无 reasoning 注入点")],
    ["app.js 接线添加/保存按钮", raw.includes('$("btnEffAdd").onclick') && raw.includes('$("btnEffSave").onclick')],
    ["app.js 调 POST /api/config/reasoning_efforts", raw.includes('"/api/config/reasoning_efforts"')],
    ["openSettings 从 state.reasoning_efforts 初始化且 `*` 分离到全局默认行",
      /effRules = Object\.entries\(effs\)\.filter\(\(\[k\]\) => k !== "\*"\)/.test(raw) && raw.includes('effs["*"] || ""')],
    ["重复模型拒绝（每个模型只允许一条）", raw.includes("已有档位（每个模型只允许一条）")],
    ["模型行拒绝 `*`（全局默认专用键另有入口）", raw.includes("* 是全局默认专用键")],
    ["renderEffList 空态占位常显（未配置 = 不注入，不整块隐藏）",
      (T.renderEffList(), nodes["effList"].innerHTML.includes("暂无模型档位"))],
    ["renderEffList 模型 ID 列显式 width:auto（.lbl.wide 的 90px 定宽会压掉 max-width）",
      /class="lbl wide" style="width:auto;max-width:340px/.test(raw)],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- 自定义渠道（R66）：设置面板接线与样式 ----
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const css = readFileSync(new URL("../cmd/wild-work/web/style.css", import.meta.url), "utf8");
  const need = [
    ["设置弹层有自定义渠道编辑区", html.includes('id="customChList"') && html.includes('id="inCustomName"') && html.includes('id="inCustomURL"')],
    ["app.js 接线自定义渠道测试/添加/保存按钮", raw.includes('$("btnCustomTest").onclick') && raw.includes('$("btnCustomAdd").onclick') && raw.includes('$("btnCustomSave").onclick')],
    ["app.js 调 POST /api/config/custom_channels", raw.includes('"/api/config/custom_channels"')],
    ["app.js 调 POST /api/config/custom_channel_test", raw.includes('"/api/config/custom_channel_test"')],
    ["openSettings 从 state.custom_channels 初始化编辑态", /editCustomChannels = JSON\.parse\(JSON\.stringify\(state\.custom_channels \|\| \[\]\)\)/.test(raw)],
    // 内建同名拒绝（R66）：前后端各自守一道——前端给即时反馈，后端 Validate 会拒。
    ["前端有内建渠道名保留集（防与内建同名）", raw.includes("RESERVED_CHANNEL_NAMES") && raw.includes('"workbuddy"') && raw.includes('"cline-free"')],
    ["后端拒绝内建同名的文案已在 hint 写清", html.includes("不得与内建渠道同名")],
    // 测试按钮必须带 name（否则脱敏回显态的 api_key 无法在后端找回真实密钥，恒 401）
    ["自定义渠道测试请求携带 name（后端子据以找回被脱敏的真实 api_key）",
      /custom_channel_test[\s\S]{0,90}name[:,]/.test(raw)],
    ["style.css 有自定义渠道徽章颜色", css.includes(".badge.custom")],
    // 悬空 DOM 引用检测（本轮实测踩坑）：app.js 里 $("x") 指向的 id 必须在 index.html 存在，
    // 否则 openSettings() 直接抛 TypeError: Cannot set properties of null 导致弹层打不开——
    // 不报错到日志、静态检查也全绿，属 R34/R41 家族的「声明了却没接线」。
    // 例外：creditTip 由 JS 动态创建（见 newCreditTip），故白名单放行。
    (() => {
      const dyn = new Set(["creditTip"]);
      const ids = new Set([...html.matchAll(/id="([A-Za-z0-9_-]+)"/g)].map((m) => m[1]));
      const missing = [...new Set([...raw.matchAll(/\$\("([A-Za-z0-9_-]+)"\)/g)].map((m) => m[1]))]
        .filter((id) => !ids.has(id) && !dyn.has(id));
      return ["app.js 的 $() DOM 引用无悬空（防 openSettings 抛 TypeError 打不开弹层）", missing.length === 0];
    })(),
    // ---- 跟进项（PR #121：自定义渠道 UI 打磨，R66 延续）----
    ["模型添加按钮为 ➕ emoji（紧凑单行）", raw.includes('data-add-model="${i}"') && raw.includes("➕")],
    ["上下文/输出输入支持 k/M 单位（parseTokens）", raw.includes("function parseTokens(") && raw.includes("function formatTokenCount(")],
    ["上下文/输出默认值使用 1M / 64k 单位串", raw.includes('value="1M"') && raw.includes('value="64k"')],
    ["模型行含工具能力勾选（supports_tools）", raw.includes('data-m-tools="${i}"') && raw.includes("supports_tools: hasTools")],
    ["模型表格有固定列宽 colgroup（对齐）", raw.includes('<colgroup>') && css.includes("table-layout: fixed")],
    ["openSettings 不再引用已移除的 inCustomModels", !raw.includes('$("inCustomModels")')],
    ["模型添加行回车快捷提交", raw.includes("模型添加输入框回车快捷添加")],
  ];
  for (const [label, pass] of need) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

// ---- overlay 嵌套完整性（issue #125）----
// 起因：v2.7.3 的「思考强度」cfg-section 少写一个 </div> ⇒ settingsOverlay 提前“吃掉”
// 其后的 rename/help/about/modelSearch/**adminLogin** 五个弹层。DOM 上 JS 已去掉 hidden 且
// display:flex，但父级 settingsOverlay.hidden 为 display:none，子树布局恒 0×0 ⇒ 用户开了
// admin_password 却看不到登录弹层、点任何按钮无反应，而 JS/后端全都正常（零报错零崩溃）。
// 与 R34/R41 同族「声明了却没接线」，只是这次是 HTML 结构层面的静默失效，故静态锁住。
{
  const html = readFileSync(new URL("../cmd/wild-work/web/index.html", import.meta.url), "utf8");
  const opens = (html.match(/<div\b/g) || []).length;
  const closes = (html.match(/<\/div>/g) || []).length;
  // 从任一 overlay 起点按花括号/标签配平找到它的匹配 </div>（返回闭合处的下标，找不到返回 -1）。
  const matchClose = (from) => {
    let depth = 0;
    const re = /<div\b|<\/div>/g;
    re.lastIndex = from;
    for (let m; (m = re.exec(html)); ) {
      if (m[0] === "</div>") {
        if (--depth === 0) return m.index;
      } else depth++;
    }
    return -1;
  };
  const start = html.indexOf('id="settingsOverlay"');
  const closeAt = start < 0 ? -1 : matchClose(html.lastIndexOf("<", start));
  const adminAt = html.indexOf('id="adminLoginOverlay"');
  const need2 = [
    ["index.html 的 <div> 开闭数量配平（少一个 </div> 会把后续弹层全卷进上一层）", opens === closes],
    ["settingsOverlay 有匹配闭合标签（未闭合即 issue #125 复现）", closeAt >= 0],
    ["adminLoginOverlay 位于 settingsOverlay **之外**（登录层必须在 body 直系，否则被 hidden 父级压成 0×0）",
      closeAt > 0 && adminAt > closeAt],
  ];
  for (const [label, pass] of need2) {
    console.log(`  ${pass ? "✅" : "❌"} ${label}`);
    if (!pass) failed++;
  }
}

console.log(failed === 0 ? "\n全部通过" : `\n${failed} 项失败`);
process.exit(failed === 0 ? 0 : 1);

