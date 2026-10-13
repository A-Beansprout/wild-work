package traework

import (
	"encoding/json"
	"strings"
	"testing"
)

// 用例镜像线上 4027 报错路径：properties.format.anyOf 内的 $ref 递归环。
func TestBreakToolSchemaCycles_AnyOfSelfRef(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"format": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"$ref": "#/$defs/node"},
				},
			},
		},
		"$defs": map[string]any{
			"node": map[string]any{
				"type":       "object",
				"properties": map[string]any{}, // 展开时由 anyOf 环回
			},
		},
		"$ref": "#/$defs/node",
	}
	// 把环接上：node.properties.self → $defs/node
	params["$defs"].(map[string]any)["node"].(map[string]any)["properties"] =
		map[string]any{"self": map[string]any{"$ref": "#/$defs/node"}}

	breakToolSchemaCycles(map[string]any{"tools": []any{
		map[string]any{"function": map[string]any{"parameters": params}},
	}})

	// 算法语义：展开链内首个成环 $ref 被清空。node.properties.self 是 node 子树内
	// 唯一的环回点，必然被清空；root.$ref 与 anyOf[1] 是指向 node 的另一次出现，
	// 目标子树已无环，原样保留（展开可终止）。
	self := params["$defs"].(map[string]any)["node"].(map[string]any)["properties"].(map[string]any)["self"].(map[string]any)
	if len(self) != 0 {
		t.Fatalf("展开链内成环点应被清空，得到 %v", self)
	}
	anyOf := params["properties"].(map[string]any)["format"].(map[string]any)["anyOf"].([]any)
	cycleBranch := anyOf[1].(map[string]any)
	if cycleBranch["$ref"] != "#/$defs/node" {
		t.Fatalf("指向已无环目标的出现应保留 $ref，得到 %v", cycleBranch)
	}
	// 根 $ref 应保留
	if params["$ref"] != "#/$defs/node" {
		t.Fatalf("根 $ref 应保留，得到 %v", params["$ref"])
	}
	// anyOf[0] 不受影响
	if b0 := anyOf[0].(map[string]any)["type"]; b0 != "string" {
		t.Fatalf("非环分支不应被改，得到 %v", b0)
	}
	// 整体展开无环（模拟 Moonshot 校验）
	assertClean(t, params)
}

func TestBreakToolSchemaCycles_MutualRecursion(t *testing.T) {
	params := map[string]any{
		"$defs": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/b"},
			"b": map[string]any{"$ref": "#/$defs/a"},
		},
		"$ref": "#/$defs/a",
	}
	breakToolSchemaCycles(map[string]any{"tools": []any{
		map[string]any{"function": map[string]any{"parameters": params}},
	}})
	a := params["$defs"].(map[string]any)["a"].(map[string]any)
	b := params["$defs"].(map[string]any)["b"].(map[string]any)
	// a→b→a 互递归：至少一端被清空（Go map 遍历序随机，清哪端不固定），
	// 且从入口模拟 Moonshot 展开（带 visited 集合）必然终止。
	if len(a) != 0 && len(b) != 0 {
		t.Fatalf("互递归至少一端应被清空，a=%v b=%v", a, b)
	}
	assertClean(t, params)
}

// hasRefCycle 模拟 Moonshot 的 schema 展开：遇到 $ref 解析到目标继续展开，
// 展开栈上重复到达即环。栈按分支复制（兄弟分支互不污染），与消毒器同构。
func hasRefCycle(node, root any, stack map[string]bool) bool {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if ptr, isLocal := localRefPtr(ref); isLocal {
				if ptr == "" || stack[ptr] {
					return true
				}
				target := resolveLocalRef(root, ptr)
				if target == nil {
					return false
				}
				child := make(map[string]bool, len(stack)+1)
				for k := range stack {
					child[k] = true
				}
				child[ptr] = true
				return hasRefCycle(target, root, child)
			}
			return false
		}
		for _, v := range t {
			if hasRefCycle(v, root, stack) {
				return true
			}
		}
	case []any:
		for _, v := range t {
			if hasRefCycle(v, root, stack) {
				return true
			}
		}
	}
	return false
}

func assertClean(t *testing.T, params any) {
	t.Helper()
	if hasRefCycle(params, params, map[string]bool{}) {
		t.Fatal("消毒后展开仍有环")
	}
}

func TestBreakToolSchemaCycles_NonCyclicRefUntouched(t *testing.T) {
	params := map[string]any{
		"$defs": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"properties": map[string]any{
			"x": map[string]any{"$ref": "#/$defs/name"},
			"y": map[string]any{"$ref": "#/$defs/name"},
		},
	}
	breakToolSchemaCycles(map[string]any{"tools": []any{
		map[string]any{"function": map[string]any{"parameters": params}},
	}})
	x := params["properties"].(map[string]any)["x"].(map[string]any)
	if x["$ref"] != "#/$defs/name" {
		t.Fatalf("菱形非环 $ref 不应被改，得到 %v", x)
	}
}

func TestBreakToolSchemaCycles_RootHashRef(t *testing.T) {
	params := map[string]any{
		"type":       "object",
		"properties": map[string]any{"child": map[string]any{"$ref": "#"}},
	}
	breakToolSchemaCycles(map[string]any{"tools": []any{
		map[string]any{"function": map[string]any{"parameters": params}},
	}})
	child := params["properties"].(map[string]any)["child"].(map[string]any)
	if len(child) != 0 {
		t.Fatalf("\"#\" 自引用应被清空，得到 %v", child)
	}
	if params["type"] != "object" {
		t.Fatal("其余字段不应受影响")
	}
}

func TestBreakToolSchemaCycles_NoToolsNoPanic(t *testing.T) {
	breakToolSchemaCycles(map[string]any{})
	breakToolSchemaCycles(map[string]any{"tools": "not-array"})
	breakToolSchemaCycles(map[string]any{"tools": []any{"str-item"}})
}

func TestIsMoonshotBacked(t *testing.T) {
	yes := []string{"kimi-k3", "Kimi-K3", "kimi-k2.6", "kimi-k3-auto", "moonshot-v1-8k"}
	for _, m := range yes {
		if !isMoonshotBacked(m) {
			t.Errorf("%s 应命中", m)
		}
	}
	no := []string{"deepseek-v4.1", "Doubao-Seed-2.1-Pro", "glm-5.3", "qwen3.8-max"}
	for _, m := range no {
		if isMoonshotBacked(m) {
			t.Errorf("%s 不应命中", m)
		}
	}
}

// 端到端：PrepareBody 对 kimi-k3 的 tools 完成消毒后再字符串化。
func TestPrepareBodyKimiToolSanitized(t *testing.T) {
	req := `{"model": "kimi-k3", "messages": [{"role": "user", "content": "hi"}], "tools": [{"type": "function", "function": {"name": "render", "description": "r", "parameters": {"type": "object", "properties": {"format": {"anyOf": [{"type": "string"}, {"$ref": "#"}]}}}}}]}`
	out := PrepareBody([]byte(req), Function)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出非法 JSON: %v", err)
	}
	tools := obj["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	ps, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 应被字符串化，得到 %T", fn["parameters"])
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(ps), &schema); err != nil {
		t.Fatalf("parameters 字符串非法: %v", err)
	}
	anyOf := schema["properties"].(map[string]any)["format"].(map[string]any)["anyOf"].([]any)
	if len(anyOf[1].(map[string]any)) != 0 {
		t.Fatalf("kimi 模型的成环分支应被清空，得到 %v", anyOf[1])
	}
}

// 对照：非 Kimi 模型不做消毒，递归 $ref 原样透传（其他后端容忍）。
func TestPrepareBodyNonKimiUntouched(t *testing.T) {
	req := `{"model":"Doubao-Seed-2.1-Pro","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"render","parameters":{"type":"object","properties":{"child":{"$ref":"#"}}}}}]}`
	out := PrepareBody([]byte(req), Function)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出非法 JSON: %v", err)
	}
	fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	ps := fn["parameters"].(string)
	if !strings.Contains(ps, "\"$ref\":\"#\"") {
		t.Fatalf("非 Kimi 模型不应消毒，得到 %s", ps)
	}
}
