package login_gemini

import (
	"os"
	"testing"

	"wild-work/internal/gemini"
)

// TestMain 给测试进程注入**假**的 OAuth 客户端凭据。
//
// 原因同 internal/gemini：源码已不再内嵌那两枚公开凭据（GitHub push protection 会拒绝推送
// 整个分支），干净环境里 gemini.ClientID()/ClientSecret() 返回空串，会让 token 表单相关断言失败。
// 假值刻意不匹配真实形态，避免测试文件自身被 push protection 拦下。
func TestMain(m *testing.M) {
	os.Setenv(gemini.EnvClientID, "0000000000-testclient.apps.googleusercontent.com")
	os.Setenv(gemini.EnvClientSecret, "test-placeholder-secret-not-real")
	os.Exit(m.Run())
}
