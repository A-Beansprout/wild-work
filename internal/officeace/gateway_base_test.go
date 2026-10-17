package officeace

import "testing"

// 本文件锁死 OfficeAce 网关 base 的**两种命名空间**（R70 附件，2026-10-17 离线取证定位）：
//
//	数据面（对话/模型目录） = 网关根 + "/v2" + path
//	控制面（/v1/subscription、/v1/claw/*） = 网关根 + path（**不得**带 /v2）
//
// 历史缺陷：两个 base 来源形态不同（导入的 api_base 自带 `/v2`；自助登录的
// model_api_url_base 不带），而旧实现把同一个 BaseURL 直接拼到两种端点上 ⇒
// OAuth 产物的 `/models` 落在 `{host}/models` 被上游 404 APIG.0101；导入产物的
// 控制面落在 `{host}/v2/v1/subscription` 同样 404。修法是读时归一（本文件的判据）。

const (
	testHostNoV2 = "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com"
	testHostV2   = testHostNoV2 + "/v2"
)

// TestGatewayRootOfStripsDataNamespace 归一规则：只剥**尾部整段** `/v2`，其余原样。
func TestGatewayRootOfStripsDataNamespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{testHostV2, testHostNoV2},                     // 导入形态
		{testHostV2 + "/", testHostNoV2},               // 尾部斜杠
		{testHostNoV2, testHostNoV2},                   // OAuth 形态（幂等）
		{testHostNoV2 + "/", testHostNoV2},             //
		{testHostNoV2 + "/V2", testHostNoV2},           // 大小写不敏感
		{testHostNoV2 + "/v2/v1", testHostNoV2 + "/v2/v1"}, // 只剥尾部，不递归
		{testHostNoV2 + "/api/v2", testHostNoV2 + "/api"},
		{testHostNoV2 + "/v1", testHostNoV2 + "/v1"},   // /v1 不是数据面命名空间
		{"", ""},
		{"  " + testHostV2 + "  ", testHostNoV2}, // 前后空白
	}
	for _, c := range cases {
		if got := gatewayRootOf(c.in); got != c.want {
			t.Errorf("gatewayRootOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDataPlaneURLsAlwaysUseV2 两种来源形态都必须落到 `/v2/...`。
// 反向验证（已做）：把 chatURL/modelsURL 改回 `trimSlash(ac.BaseURL)+path`，
// 本用例在 OAuth 形态上立即变红（产出 `{host}/models`）。
func TestDataPlaneURLsAlwaysUseV2(t *testing.T) {
	for _, base := range []string{testHostV2, testHostNoV2, testHostV2 + "/"} {
		ac := &Account{BaseURL: base}
		if got, want := ac.chatURL(), testHostV2+"/chat/completions"; got != want {
			t.Errorf("chatURL(base=%q) = %q, want %q", base, got, want)
		}
		if got, want := ac.modelsURL(), testHostV2+"/models"; got != want {
			t.Errorf("modelsURL(base=%q) = %q, want %q", base, got, want)
		}
	}
}

// TestControlPlaneBaseHasNoV2 控制面基址必须是网关根（且拼接后不含 /v2）。
// 反向验证（已做）：去掉 gatewayBase 里的 gatewayRootOf，本用例对导入形态变红。
func TestControlPlaneBaseHasNoV2(t *testing.T) {
	c := New() // 仅构造 Transport，不发任何请求（BaseURL 非空时 gatewayBase 直接返回）
	for _, base := range []string{testHostV2, testHostNoV2} {
		got, err := c.gatewayBase(&Account{BaseURL: base})
		if err != nil {
			t.Fatalf("gatewayBase(base=%q) 出错：%v", base, err)
		}
		if got != testHostNoV2 {
			t.Errorf("gatewayBase(base=%q) = %q, want %q", base, got, testHostNoV2)
		}
		// 真实控制面拼接点（与 quota.go fetchSubscription/DailyCheckinReport 同构）
		for _, suffix := range []string{"/v1/subscription", "/v1/subscription/bonus/claim"} {
			if u := trimSlash(got) + suffix; u != testHostNoV2+suffix {
				t.Errorf("控制面 URL(base=%q, suffix=%q) = %q, want %q", base, suffix, u, testHostNoV2+suffix)
			}
		}
	}
}

// TestDefaultGatewayIsRoot 兜底常量本身就应是根（无 /v2），否则控制面会多一层前缀。
func TestDefaultGatewayIsRoot(t *testing.T) {
	if gatewayRootOf(defaultGateway) != defaultGateway {
		t.Errorf("defaultGateway 不是网关根：%q ⇒ %q", defaultGateway, gatewayRootOf(defaultGateway))
	}
}
