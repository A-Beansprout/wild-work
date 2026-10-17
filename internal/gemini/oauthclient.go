// oauthclient.go 运行时获取 Google OAuth 客户端的 client_id / client_secret。
//
// 为什么不在源码里内嵌（2026-10-17 变更）：
// 这两枚值原本内嵌在 constants.go —— 它们是 Google 官方 Gemini CLI 内嵌的**公开**凭据
// （开源客户端里就有，任何人可提取），但 GitHub 的 Secret Scanning Push Protection
// **只看格式、不看是否公开**，会把它们判定为 secret 并**拒绝推送整个分支**（GH013）。
// 改为运行时获取，顺带与 glm / loomy / monkeycode 的「导入型」渠道保持同一思路。
//
// 获取顺序：
//  1. 环境变量 `CMDC_PAK_GOOGLE_CLIENT_ID` / `CMDC_PAK_GOOGLE_CLIENT_SECRET`（自建 OAuth 客户端或手动指定）；
//  2. 本机安装的官方 Gemini CLI 包（npm 全局 / npx 缓存 / 项目内 node_modules）；
//  3. 都拿不到 → 空串，调用方给出可操作错误（login_gemini 会提示两条解决路径）。
package gemini

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// 两枚公开值的**形态**。正则刻意拆成「字符类 + 片段」，不含任何完整可识别的 secret 值 ——
// 否则本文件自己又会被 push protection 拦下（那正是这次要修的问题）。
var (
	reOAuthClientID     = regexp.MustCompile(`[0-9]{10,}-[a-z0-9]{20,}\.apps\.googleusercontent\.com`)
	reOAuthClientSecret = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_\-]{20,}`)
)

// localOAuthClient 解析结果。
type localOAuthClient struct {
	ID     string
	Secret string
}

var (
	oauthClientOnce sync.Once
	oauthClientVal  localOAuthClient
)

// oauthClient 解析一次并缓存（提取要扫目录，不能每个请求都做）。
//
// env 只给了一半时，用本机 CLI 补另一半：自建客户端理应两枚都给，但宽松处理比直接报错友好。
func oauthClient() localOAuthClient {
	oauthClientOnce.Do(func() {
		oauthClientVal = localOAuthClient{
			ID:     strings.TrimSpace(os.Getenv(EnvClientID)),
			Secret: strings.TrimSpace(os.Getenv(EnvClientSecret)),
		}
		if oauthClientVal.ID == "" || oauthClientVal.Secret == "" {
			id, secret := extractOAuthClientFromLocalCLI()
			if oauthClientVal.ID == "" {
				oauthClientVal.ID = id
			}
			if oauthClientVal.Secret == "" {
				oauthClientVal.Secret = secret
			}
		}
	})
	return oauthClientVal
}

// cliRoots 官方 Gemini CLI 的常见安装位置。
//
// 只列**明确属于该 CLI**的目录：不扫整个 node_modules（动辄几十万文件，代价不可接受）。
func cliRoots() []string {
	var roots []string
	if v := strings.TrimSpace(os.Getenv("GEMINI_CLI_HOME")); v != "" {
		roots = append(roots, v)
	}
	if appdata := strings.TrimSpace(os.Getenv("APPDATA")); appdata != "" {
		roots = append(roots, filepath.Join(appdata, "npm", "node_modules", "@google", "gemini-cli"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots,
			filepath.Join(home, ".npm-global", "lib", "node_modules", "@google", "gemini-cli"),
			filepath.Join(home, "node_modules", "@google", "gemini-cli"),
			// npx 缓存：里面是 `_npx/<hash>/node_modules/@google/gemini-cli`，多版本并存，整体下探。
			filepath.Join(home, ".npm", "_npx"),
		)
	}
	return roots
}

// extractOAuthClientFromLocalCLI 在本机 CLI 包里找这两枚值；找不到（或目录不存在）返回空串。
//
// 扫描有硬上限：文件数、单文件字节数、扩展名白名单 —— 避免在意外巨大的目录上卡住主流程。
func extractOAuthClientFromLocalCLI() (id, secret string) {
	const (
		maxFiles = 4000
		maxBytes = 8 << 20
	)
	files := 0
	for _, root := range cliRoots() {
		if id != "" && secret != "" {
			break
		}
		if _, err := os.Stat(root); err != nil {
			continue // 未安装 / 无权限：跳过，不当错误
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "test", "tests", "docs", "coverage", "__tests__":
					return fs.SkipDir
				}
				return nil
			}
			if files >= maxFiles {
				return fs.SkipAll
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".js", ".mjs", ".cjs", ".json", ".ts":
			default:
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() > maxBytes {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			files++
			s := string(b)
			if id == "" {
				id = reOAuthClientID.FindString(s)
			}
			if secret == "" {
				secret = reOAuthClientSecret.FindString(s)
			}
			if id != "" && secret != "" {
				return fs.SkipAll
			}
			return nil
		})
	}
	return id, secret
}
