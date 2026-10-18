package pool

import (
	"testing"

	"wild-work/internal/auth"
)

// TestRecordCheckinFailureMarksSessionDead 回归测试（issue #157）：
// 签到失败且消息是「登录已失效」形态时，必须
//   ① last_checkin_ok = false（面板不能显示绿色「签到成功」）；
//   ② 打上 session_dead 标记（面板「需重新登录」徽标 + 挑号跳过）。
//
// 修复前此用例必失败：RecordCheckin 里 `if e, ok := p.byUID[uid]; ok` 的 ok
// 遮蔽了函数参数 ok bool ⇒ 恒为 true ⇒ lastCheckinOK 恒真、sessionDead 被清、
// `else if sessionDeadLike(msg)` 成死代码。
func TestRecordCheckinFailureMarksSessionDead(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.RecordCheckin("u1", false, "登录已失效，请重新登录（refresh failed: xxx）；余额查询失败")

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 应存在")
	}
	if st.LastCheckinOK {
		t.Errorf("last_checkin_ok 应为 false，实为 true（面板会显示绿色「签到成功」）")
	}
	if !st.SessionDead {
		t.Errorf("失败且消息含「登录已失效」时应标记 session_dead，实为未标记")
	}
	if st.SessionDeadReason == "" {
		t.Error("session_dead_reason 应保留失败原因")
	}

	// 反过来：一次失败的「非登录失效」消息（如网络抖动）不应误标 session_dead，
	// 但 last_checkin_ok 仍必须是 false —— 防止修遮蔽时把两条语义一起改坏。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u2"})
	p2.RecordCheckin("u2", false, "网络超时")
	st2, _ := p2.Status("u2")
	if st2.LastCheckinOK {
		t.Error("网络超时也应记为 last_checkin_ok=false")
	}
	if st2.SessionDead {
		t.Error("普通失败不应标记 session_dead")
	}
}

// TestRecordCheckinSuccessClearsSessionDead 成功签到要能清掉标记（#134 的另一半语义）。
func TestRecordCheckinSuccessClearsSessionDead(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u3"})
	p.MarkSessionDead("u3", "401 upstream")

	p.RecordCheckin("u3", true, "签到成功")

	st, _ := p.Status("u3")
	if !st.LastCheckinOK {
		t.Error("成功签到应记 last_checkin_ok=true")
	}
	if st.SessionDead {
		t.Error("成功签到应清除 session_dead")
	}
}
