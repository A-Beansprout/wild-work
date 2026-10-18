package provider

import (
	"testing"
	"time"
)

// timeNow 返回 UTC+8 墙钟当前时刻（与 expireLoc 口径一致，测试用）。
func timeNow() time.Time { return time.Now().In(expireLoc) }

func TestExpiringWithin(t *testing.T) {
	today := timeNow().Format("2006-01-02")
	tomorrow := timeNow().AddDate(0, 0, 1).Format("2006-01-02")
	after := timeNow().AddDate(0, 0, 3).Format("2006-01-02")

	items := []ResourceItem{
		{Name: "今日到期", Remain: 100, Usable: true, ExpireAt: today},
		{Name: "明天到期", Remain: 50, Usable: true, ExpireAt: tomorrow},
		{Name: "三天后到期", Remain: 999, Usable: true, ExpireAt: after},
		{Name: "无到期时间", Remain: 70, Usable: true},
		{Name: "不可用池临期", Remain: 300, Usable: false, ExpireAt: today},
	}
	if got := ExpiringWithin(items, 24*time.Hour); got != 150 {
		t.Fatalf("expiring=%d want 150 (今天100+明天50，三天后/无到期/不可用不计)", got)
	}
}

func TestSummarizeExpiring(t *testing.T) {
	items := []ResourceItem{
		{Remain: 100, Usable: true, ExpireAt: timeNow().Format("2006-01-02")},
		{Remain: 200, Usable: false},
	}
	u, un := Summarize(items)
	e := ExpiringWithin(items, 24*time.Hour)
	if u != 100 || un != 200 || e != 100 {
		t.Fatalf("usable=%d unusable=%d expiring=%d want 100/200/100", u, un, e)
	}
}

// TestExpireInstant 失效时刻口径：日期粒度是「次日 00:00」，RFC3339 是精确时刻。
// 这是 ledger「是否已过期」与面板「临期」共用的唯一解析入口，两种写法都要覆盖。
func TestExpireInstant(t *testing.T) {
	// 日期粒度：当天仍可用 ⇒ 失效时刻落在次日零点
	got, ok := ExpireInstant("2026-10-18")
	if !ok {
		t.Fatal("日期粒度应可解析")
	}
	want := time.Date(2026, 10, 19, 0, 0, 0, 0, expireLoc)
	if !got.Equal(want) {
		t.Fatalf("日期粒度失效时刻 = %v，want %v（次日 00:00）", got, want)
	}

	// RFC3339：精确时刻，不做「次日」平移
	got, ok = ExpireInstant("2026-10-18T15:00:00+08:00")
	if !ok {
		t.Fatal("RFC3339 应可解析")
	}
	want = time.Date(2026, 10, 18, 15, 0, 0, 0, expireLoc)
	if !got.Equal(want) {
		t.Fatalf("RFC3339 失效时刻 = %v，want %v（原样）", got, want)
	}

	// 空串 / 不可解析：ok=false，调用方按「无到期信息」处理
	for _, s := range []string{"", "  ", "not-a-date", "2026-13-45"} {
		if _, ok := ExpireInstant(s); ok {
			t.Errorf("ExpireInstant(%q) 不应可解析", s)
		}
	}
}

// TestExpiringWithinRFC3339 临期统计要能吃 RFC3339（gemini 的窗口重置时间）。
// 日期粒度的既有口径（今天/明天都算临期）由 TestExpiringWithin 锁住，这里只补新格式。
func TestExpiringWithinRFC3339(t *testing.T) {
	now := timeNow()
	items := []ResourceItem{
		{Name: "两小时后重置", Remain: 40, Usable: true, ExpireAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
		{Name: "两天后重置", Remain: 900, Usable: true, ExpireAt: now.Add(48 * time.Hour).Format(time.RFC3339)},
		{Name: "不可解析", Remain: 500, Usable: true, ExpireAt: "bogus"},
	}
	if got := ExpiringWithin(items, 24*time.Hour); got != 40 {
		t.Fatalf("expiring=%d want 40（只算 24h 内的 RFC3339 条目）", got)
	}
}
