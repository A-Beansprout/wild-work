// checkin.go QoderCN 每日签到：仅 campaigns 活动路径（对齐上游 qoder2api 99ab022）。
//   - GET  /sash/api/v1/me/campaigns                    → 找 CLAIMABLE 的 CLAIM_BENEFIT
//   - POST /sash/api/v1/me/campaigns/{campaignId}/claim → 领取（空 body）
//
// 2026-09-21 实测（本项目抓包 + 上游 qoder2api 同款结论）：legacy daily-check-in
// 系统已全局 DISABLED，但其 claim 端点对未领取日也恒返回 409，会被误判成
// 「今日已领取」而跳过真实领取 → 假成功、零积分。故不再走 legacy claim；
// status 端点仅作只读统计（上游 99ab022 亦如此，保留备用）。
// 签到认证：仅需 Bearer dt- + cosy-clienttype:10（桌面端标识），无需 COSY 签名。
// 幂等语义：已签到（409 / CLAIMED / replayed）与无活动均视为成功（返回 nil）。
//
// 本文件与 internal/qodercom/checkin.go **逻辑逐行对齐**（本仓对 Qoder 三渠道刻意
// 采用「代码级复制、不共享运行时」的既有约定）。此前两者解析层不一致，出现过
// 「COM 已改、CN 落后」的分叉——改这里时请同步核对 qodercom 同名文件。
// 2026-10-17 对齐内容：claimStatus 双形态容错（含未知形态告警）、已领金额上报、
// no_campaign 诊断文案（把「上游没发活动」与「本工具解析出错」分开）。
package qodercn

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// checkinHost 签到 API 域名（抓包确认 openapi.qoder.com.cn，与业务 API 同域）。
// var 而非 const：测试用 httptest 覆盖。
var checkinHost = OpenAPIBase

// checkinHeaders 构造桌面端签到请求头（抓包确认的必需头）。
// ★ cosy-clienttype 必须为 10（桌面端标识），推理链路用的是 5，两者不可混用。
func checkinHeaders(req *http.Request, dt string) {
	req.Header.Set("authorization", "Bearer "+dt)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Qoder")
	req.Header.Set("cosy-clienttype", "10")
}

// doCheckinRequest 发送签到请求，返回 (httpStatus, rawBody)。
// reqBody 为 nil 时发送空 body（抓包确认 campaigns/claim 即空 body）。
func doCheckinRequest(method, path, dt string, reqBody any) (int, []byte, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, nil, err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, checkinHost+path, body)
	if err != nil {
		return 0, nil, err
	}
	checkinHeaders(req, dt)
	if method == http.MethodPost {
		req.Header.Set("origin", checkinHost)
		if reqBody == nil {
			req.ContentLength = 0
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// checkin 执行签到：campaigns 活动路径（唯一领取路径）。
// 返回 (报告, error)：error 非 nil 时调度器可按 isSessionDead 自愈重试；
// 报告状态供调度器判定当日是否完成、是否需要在窗口内重试。
func checkin(a *auth.Auth) (provider.CheckinReport, error) {
	dt := a.JWT()
	if dt == "" {
		return provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "no dt- available"}, nil
	}
	rep, err := tryCampaigns(dt)
	log.Printf("qodercn checkin campaigns uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
		a.UID, rep.Status, rep.Status == provider.CheckinClaimed, rep.Amount, rep.Msg, err)
	return rep, err
}

// campaignNode 活动列表里的一条活动（只解本工具需要的字段）。
type campaignNode struct {
	CampaignID  string           `json:"campaignId"`
	CampaignKey string           `json:"campaignKey"`
	ActionType  string           `json:"actionType"`
	ClaimStatus claimStatusNode  `json:"claimStatus"`
	Benefit     *campaignBenefit `json:"benefit"`
}

// campaignsList 活动列表响应。showCampaign/claimable 只用于诊断文案。
type campaignsList struct {
	ShowCampaign bool           `json:"showCampaign"`
	Claimable    bool           `json:"claimable"`
	Campaigns    []campaignNode `json:"campaigns"`
}

// tryCampaigns 查活动列表 → 领取 CLAIMABLE 的 CLAIM_BENEFIT。
func tryCampaigns(dt string) (provider.CheckinReport, error) {
	status, raw, err := doCheckinRequest(http.MethodGet, EpCampaigns, dt, nil)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	// 401 必须按 session dead 类型透传，否则调度器无法自愈（AGENTS.md 不变式 19）。
	if status == http.StatusUnauthorized {
		return provider.CheckinReport{Status: provider.CheckinError}, sessionDead(status, raw)
	}
	if status != http.StatusOK {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("campaigns http %d: %s", status, truncate(string(raw), 200))}, nil
	}
	var list campaignsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("campaigns parse: %v", err)}, nil
	}
	warnUnrecognizedClaimStatus(list.Campaigns)
	var targetID string
	var alreadyAmount int64
	already := false
	for _, cp := range list.Campaigns {
		// 只有 CLAIM_BENEFIT 是签到领积分；VIEW_DETAILS 等其它动作的 CLAIMED 与「今日已领」无关，
		// 不能用来把 no_campaign 掩成已领取（否则当天漏领）。
		if cp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch cp.ClaimStatus.mode() {
		case "CLAIMABLE":
			targetID = cp.CampaignID
		case "CLAIMED":
			already = true
			if cp.Benefit != nil {
				// 仅供面板显示「今日已领取 +N」：provider.CheckinReport.Amount 的字段口径是
				// 「仅 Status==CheckinClaimed 时有意义」，而调度器不读 already 分支的 Amount
				// （已核对 scheduler 生产代码）。多条 CLAIMED 活动时取最后一条、不做求和，
				// 避免把不同活动重复计入展示金额。
				alreadyAmount = cp.Benefit.Amount
			}
		}
	}
	if targetID == "" {
		if already {
			// 已领取时活动里只有 CLAIMED 记录，本次不会再拿到 claim 回执，
			// 金额只能取活动自身的 benefit.amount（桌面端已领后仍会保留该字段）。
			return provider.CheckinReport{Status: provider.CheckinAlready, Amount: alreadyAmount, Msg: alreadyMsg(alreadyAmount)}, nil
		}
		// 活动尚未创建（10:00 整点延迟）或上游按客户端会话/设备下发 → 可重试状态，
		// 不可当作完成。Msg 带上游响应摘要，便于判断是谁的问题。
		return provider.CheckinReport{Status: provider.CheckinNoCampaign, Msg: noCampaignMsg(list)}, nil
	}

	// 领取（空 body，抓包确认）
	status, raw, err = doCheckinRequest(http.MethodPost, EpCampaigns+"/"+targetID+"/claim", dt, nil)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if status == http.StatusConflict { // 409 幂等：今日已领
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: alreadyMsg(0)}, nil
	}
	if status == http.StatusUnauthorized {
		return provider.CheckinReport{Status: provider.CheckinError}, sessionDead(status, raw)
	}
	if status != http.StatusOK {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("领取失败 http %d: %s", status, truncate(string(raw), 150))}, nil
	}
	var claim struct {
		Status   string           `json:"status"`
		Replayed bool             `json:"replayed"`
		Benefit  *campaignBenefit `json:"benefit"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("claim parse: %v", err)}, nil
	}
	if claim.Status != "CLAIMED" {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("未知状态 %s", claim.Status)}, nil
	}
	if claim.Replayed {
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: alreadyMsg(claim.Benefit.amount())}, nil
	}
	amount := claim.Benefit.amount()
	return provider.CheckinReport{Status: provider.CheckinClaimed, Amount: amount, Msg: fmt.Sprintf("签到成功 +%d", amount)}, nil
}

// noCampaignMsg 组装 no_campaign 的诊断文案（进面板 last_checkin_msg）。
// 目的是让用户一眼分清「上游本次没下发可领取活动」与「本工具解析/请求出错」。
// showCampaign=false 表示上游连活动体系都没开——这是与「活动尚未创建」不同的信号，故一并带上。
func noCampaignMsg(list campaignsList) string {
	if len(list.Campaigns) == 0 {
		return fmt.Sprintf("上游未下发任何活动（showCampaign=%t, claimable=%t）", list.ShowCampaign, list.Claimable)
	}
	return fmt.Sprintf("上游未下发可领取活动（showCampaign=%t, claimable=%t；%s）", list.ShowCampaign, list.Claimable, campaignsSummary(list.Campaigns))
}

// campaignsSummary 把活动列表压成一行「动作=状态」摘要，形如
// "2 条活动：CLAIM_BENEFIT=CLAIMED, VIEW_DETAILS=CLAIMED"。
func campaignsSummary(campaigns []campaignNode) string {
	parts := make([]string, 0, len(campaigns))
	for _, cp := range campaigns {
		state := cp.ClaimStatus.mode()
		if state == "" {
			// 未知形态原样带上（截断）：这是「上游换了形态、解析层需复核」的唯一线索。
			state = "未知" + truncate(cp.ClaimStatus.raw, 40)
		}
		parts = append(parts, cp.ActionType+"="+state)
	}
	return fmt.Sprintf("%d 条活动：%s", len(campaigns), strings.Join(parts, ", "))
}

// warnUnrecognizedClaimStatus 对「是 CLAIM_BENEFIT 但 claimStatus 不属于已知两值」的活动告警。
// 两种情形都要报，否则会被静默吞成一句笼统的 no_campaign：
//   - 取值是没见过的新枚举（如 "CLAIMABLE_V2"）→ 上游可能新增了可领状态；
//   - 形态本身不认识（非字符串、非 {"status":…}）→ 上游可能改了结构。
// 这正是本渠道历史上被误判为解析缺陷的盲区（形态/取值变了，面板却只显示「无可用签到活动」）。
func warnUnrecognizedClaimStatus(campaigns []campaignNode) {
	for _, cp := range campaigns {
		if cp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch cp.ClaimStatus.mode() {
		case "CLAIMABLE", "CLAIMED":
			continue
		}
		log.Printf("qodercn checkin: CLAIM_BENEFIT 活动 %s 的 claimStatus 未识别（raw=%s）——可能是上游新增枚举或新形态，需复核解析层",
			cp.CampaignID, truncate(cp.ClaimStatus.raw, 80))
	}
}

// campaignBenefit 活动/回执里的权益描述（签到只有 CREDITS 一种，故只关心金额）。
type campaignBenefit struct {
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

// amount 空指针安全地取金额：活动未配置 benefit 或上游未回填时为 0。
func (b *campaignBenefit) amount() int64 {
	if b == nil {
		return 0
	}
	return b.Amount
}

// claimStatusNode 兼容 claimStatus 的两种形态：
//
//	"CLAIMABLE"              扁平字符串 —— **线上实际形态**（本机 Qoder 桌面端 0.4.8
//	                         主进程日志三份会话逐字实证；本项目单发探测回包同形）
//	{"status":"CLAIMABLE"}   嵌套对象 —— 线上**未见证据**，仅作防御性兼容
//
// 之前按 string 反序列化：若上游真的换成对象形态，会让**整个列表**解析失败
// （campaigns parse: …），届时面板只会看到一句笼统错误、失去逐条线索。
// 这里自行解码，两种形态都接受，未知形态降级为空串（= 不可领取，不误领），
// 并保留原始取值供诊断文案与告警使用。
type claimStatusNode struct {
	state string // 归一化后的状态串
	raw   string // 原始取值（日志排查用）
}

func (n *claimStatusNode) UnmarshalJSON(b []byte) error {
	n.raw = string(b)
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		n.state = s
		return nil
	}
	var obj struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		n.state = obj.Status
		return nil
	}
	n.state = "" // 未知形态：当作不可领取，让上层按 no_campaign/已领判定
	return nil
}

// mode 返回归一化状态（大写、去空白）；未知形态返回空串，调用方按「不可领取」处理。
func (n claimStatusNode) mode() string { return strings.ToUpper(strings.TrimSpace(n.state)) }

// alreadyMsg 已领取的展示文案：有金额时带上，避免面板显示「今日已领取」却不知领了多少。
func alreadyMsg(amount int64) string {
	if amount > 0 {
		return fmt.Sprintf("今日已领取 +%d", amount)
	}
	return "今日已领取"
}

// sessionDead 构造类型化的 401 错误：调度器用 errors.As(*provider.Error) 判定自愈，
// 裸 fmt.Errorf 会让 isSessionDead 恒为 false（AGENTS.md 不变式 19）。
func sessionDead(status int, raw []byte) error {
	return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: truncate(string(raw), 200)}
}
