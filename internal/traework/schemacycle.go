package traework

import (
	"strconv"
	"strings"
)

// Kimi/Moonshot 系上游对 tools.function.parameters 的 JSON Schema 校验极严：
// 展开 $ref 时若展开路径回到链上已展开过的节点（成环），整体 400——
// 「detected infinite recursion without termination condition」（2026-10-13 实测
// kimi-k3 code=4027，路径 properties.format.anyOf）。官方客户端不发递归 schema，
// 代理客户端（如经 MCP 接入的工具）会发；OpenAI/Anthropic 后端容忍递归，
// Moonshot 不容忍。故仅对 Kimi 系模型消毒：把成环 $ref 原地替换为空 schema
// （JSON Schema 语义 = 接受任意值，展开终止），非环 $ref 原样保留。
func isMoonshotBacked(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "kimi") || strings.Contains(m, "moonshot")
}

// breakToolSchemaCycles 就地打断 tools[].function.parameters 中的 $ref 递归环。
// 必须在 normalizeTools 把 parameters 序列化为字符串之前调用（map 形态才能改写）。
func breakToolSchemaCycles(obj map[string]any) {
	raw, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, item := range raw {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		walkSchemaCycles(params, params, map[string]bool{})
	}
}

// walkSchemaCycles 深度优先遍历 schema；root 为整个 parameters 文档（$ref 解析基准），
// active 为当前展开链上已入栈的 $ref 目标指针集合（分叉时复制，互不污染）。
func walkSchemaCycles(node, root any, active map[string]bool) {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if ptr, isLocal := localRefPtr(ref); isLocal {
				if ptr == "" || active[ptr] {
					// "#" 指向整个文档（必环），或展开链成环：空 schema 终止。
					for k := range t {
						delete(t, k)
					}
					return
				}
				if target := resolveLocalRef(root, ptr); target != nil {
					child := make(map[string]bool, len(active)+1)
					for k := range active {
						child[k] = true
					}
					child[ptr] = true
					walkSchemaCycles(target, root, child)
				}
			}
		}
		for k, v := range t {
			if k == "$ref" {
				continue // 已处理：环已替换 / 非环保留原样
			}
			walkSchemaCycles(v, root, active)
		}
	case []any:
		for _, v := range t {
			walkSchemaCycles(v, root, active)
		}
	}
}

// localRefPtr 解析本地 $ref："#/a/b" → "/a/b"；"#" → ""（必环）；非本地引用
// （外链 / 非 JSON Pointer 形态）返回 false 不处理。
func localRefPtr(ref string) (string, bool) {
	r := strings.TrimSpace(ref)
	if !strings.HasPrefix(r, "#") {
		return "", false
	}
	p := strings.TrimPrefix(r, "#")
	if p == "" {
		return "", true
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// resolveLocalRef 按 JSON Pointer（RFC 6901）在文档内取目标节点；不可达返回 nil。
func resolveLocalRef(root any, ptr string) any {
	cur := root
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			nxt, ok := c[seg]
			if !ok {
				return nil
			}
			cur = nxt
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	return cur
}
