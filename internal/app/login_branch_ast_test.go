package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestStartLoginForPerChannelBranch 按 **AST 结构**断言 StartLoginFor 里
// 渠道 → 登录调用 的对应关系，而不是「文件里是否出现过某字符串」。
//
// 背景（2026-10-18 深档排障，H1/H2/H5）：
// 旧的 TestRaccoonLoginUsesWebFlow 用整文件 strings.Contains 断言，导致
//
//	app.go:737  authURL, err = loginloomy.Start(...)      ← Loomy 的结果
//	app.go:738  authURL, err = loginraccoon.Start()       ← 被下一行覆盖
//
// 这种「两行挤进同一个 case」的缺陷**结构上不可能被发现**：L24 的
// strings.Contains(code, "loginraccoon.Start()") 恰被缺陷本体满足。
//
// 本测试改用 AST：
//  1. 找到 StartLoginFor 函数体里的第 3 个 switch（渠道 → Start 调用）；
//  2. 对每个 case，收集该分支体内**实际被赋给 authURL 的最后一个调用**；
//  3. 断言「每个渠道的 case 只产生一个 authURL 赋值」——
//     多于一个 = 串写（H2 形态）；等于零 = 空壳（H1 形态）。
func TestStartLoginForPerChannelBranch(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "app.go", src, 0)
	if err != nil {
		t.Fatalf("解析 app.go 失败: %v", err)
	}

	fn := findFunc(file, "StartLoginFor")
	if fn == nil {
		t.Fatal("未找到 StartLoginFor 函数")
	}

	// 收集函数体内所有 switch，按出现顺序；第 3 个是「渠道 → Start() 调用」那张。
	var switches []*ast.SwitchStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		if sw, ok := n.(*ast.SwitchStmt); ok {
			switches = append(switches, sw)
		}
		return true
	})
	if len(switches) < 3 {
		t.Fatalf("StartLoginFor 里 switch 数量 = %d，期望至少 3 个（A 准入 / B client 工厂 / C Start 调用）；"+
			"若结构已重构请同步更新本测试", len(switches))
	}
	startSwitch := switches[2]

	// 逐 case 分析：该分支体对 authURL 赋值了几次、最后一次调的是什么。
	type branch struct {
		channel      string
		assigns      int
		lastCall     string
		line         int
		returnsError bool // 分支体内是否有「赋值前就 return 错误」的防御分支
	}
	var branches []branch
	for _, stmt := range startSwitch.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range cc.List {
			se, ok := expr.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			// provider.Raccoon → "Raccoon"
			assigns, lastCall := analyzeAuthURLAssign(cc.Body)
			branches = append(branches, branch{
				channel:      se.Sel.Name,
				assigns:      assigns,
				lastCall:     lastCall,
				line:         fset.Position(cc.Pos()).Line,
				returnsError: hasErrorReturn(cc.Body),
			})
		}
		// default 分支（cc.List == nil）不参与逐渠道断言。
	}

	if len(branches) == 0 {
		t.Fatal("第 3 个 switch 里没有解析到任何 provider.X case，AST 结构与预期不符")
	}

	for _, b := range branches {
		switch {
		case b.assigns == 0 && !b.returnsError:
			t.Errorf("❌ H1 形态：case provider.%s（app.go:%d）既不赋值 authURL、也不返回错误 —— "+
				"这是空壳 case：能进来、什么也不做，前端会拿到空 URL 并提示「无法获取登录链接」",
				b.channel, b.line)
		case b.assigns > 1:
			t.Errorf("❌ H2 形态：case provider.%s（app.go:%d）对 authURL 赋值了 %d 次 —— "+
				"多次赋值意味着**前一次的结果被后一次覆盖**（复制粘贴串写）。"+
				"最后一个生效的调用是 %s",
				b.channel, b.line, b.assigns, b.lastCall)
		}
	}
}

// analyzeAuthURLAssign 统计分支体内 `authURL, err = ...` 赋值的次数，
// 并返回最后一次右侧调用的「包.函数」描述。
func analyzeAuthURLAssign(body []ast.Stmt) (count int, lastCall string) {
	for _, stmt := range body {
		as, ok := stmt.(*ast.AssignStmt)
		if !ok || len(as.Lhs) == 0 {
			continue
		}
		// 只看左值里含 authURL 的赋值。
		hitsAuthURL := false
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == "authURL" {
				hitsAuthURL = true
				break
			}
		}
		if !hitsAuthURL || len(as.Rhs) == 0 {
			continue
		}
		count++
		// 右侧可能是 call(...)，也可能是逗号表达式里的第 1 个。
		if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok {
					lastCall = pkg.Name + "." + sel.Sel.Name
				}
			}
		}
	}
	return count, lastCall
}

// findFunc 在文件顶层声明里按名字找 *ast.FuncDecl。
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Body != nil {
			return fd
		}
	}
	return nil
}

// hasErrorReturn 判断分支体内是否有「return <非nil 第一值>, <错误>」形式的防御性返回。
//
// 为什么需要它：case provider.Oczen, provider.Kilo（app.go:759-764）**刻意**不赋值
// authURL，而是直接 return 一个错误（拒绝免密渠道）。那是正确的防御，不是空壳缺陷。
// 判据取「return 语句携带 ≥2 个返回值」——本函数所有合法路径都返回 (authURL, err)。
func hasErrorReturn(body []ast.Stmt) bool {
	found := false
	ast.Inspect(&ast.BlockStmt{List: body}, func(n ast.Node) bool {
		if found {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) >= 2 {
			found = true
			return false
		}
		return true
	})
	return found
}
