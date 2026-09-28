package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Codex "responses lite" 工具形状支持。
//
// codex 客户端在 use_responses_lite 模式下把工具放在 input 里的一个 additional_tools 项
// （`{"type":"additional_tools","role":"developer","tools":[…]}`），而不是顶层 tools 字段；
// 并把插件/执行沙箱工具按 namespace 分组
// （`{"type":"namespace","name":"functions","tools":[…]}`）。两者都是 Responses 协议的私有
// 扩展：Anthropic Messages / Chat Completions 都没有对应概念，适配器必须展开——把 namespace
// 子工具提升为顶层工具，名字用确定性的 `<namespace>__<child>`（超长则前缀截断 + `__` +
// sha256 前 8 字节），回程再把名字还原成 `{name, namespace}`，客户端才能对上自己的命名空间
// 注册表（缺 namespace 时客户端补默认 `functions`，`collaboration.*` 会解析不到）。
//
// 设计参照 cc-switch 的 transform_codex_responses_namespace.rs 与 transform_codex_chat.rs
// 的 CodexToolContext（其设计又来自 sub2api/pkg/apicompat/responses_namespace.go）：
//   - 展开与还原都用同一个 flattenNamespaceToolName 推导，不必在转发与回程之间传状态；
//   - 扁平名冲突**直接报错**，绝不静默丢工具（静默丢工具正是这个 bug 长期没被发现的原因）；
//   - 不带 additional_tools 载体的请求，工具列表与之前逐字节相同。
//
// 与 cc-switch 的一处**有意偏离**：它只提升 namespace 里的 `function` 子工具，会丢掉
// `custom` 子工具；而 codex 的 Code Mode `exec`（freeform / lark 语法）恰恰是 `functions`
// 下的 custom 工具，丢不得。这里 custom 子工具一并提升（保留 type=custom，交给
// convertCustomToolDefinition 处理），回程也一并还原 custom_tool_call。

// codexToolNameMaxLen 是上游工具名长度上限（Anthropic 与 OpenAI 同为 64）。
const codexToolNameMaxLen = 64

// flattenNamespaceToolName 把 {namespace, name} 折叠成确定性的扁平工具名。
// 与 cc-switch 的 flatten_namespace_tool_name 一致：`<namespace>__<name>`；超过 64 字节时
// 取前缀（按字符边界）+ `__` + sha256 前 8 字节（16 个 hex 字符）。
func flattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= codexToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:8])
	keep := codexToolNameMaxLen - len(suffix)
	for keep > 0 && !utf8.RuneStart(full[keep]) {
		keep--
	}
	return full[:keep] + suffix
}

// codexToolIdentity 是一个工具在 codex 侧的身份（回程还原用）。Namespace 为空表示顶层工具。
type codexToolIdentity struct {
	Namespace string
	Name      string
}

// codexToolContext 是一次请求的工具注册表。
type codexToolContext struct {
	// tools 是合并 + 展开后的工具定义（Responses 形状），可直接交给 convertTools。
	tools []any
	// restore 是扁平名 → 原始身份，仅含来自 namespace 的工具。
	restore map[string]codexToolIdentity
	// rewritten 表示本次确实改写了工具列表（存在载体或 namespace 工具）。为 false 时
	// 调用方不应改动 doc["tools"]，保证经典形状逐字节不变。
	rewritten bool
}

// applyCodexToolContext 在转换前把 responses lite 形状的工具展开进 doc["tools"]，并改写
// input 历史里的 namespace 调用与 namespace 形状的 tool_choice。返回回程要用的还原表。
//
// 所有"从原始 Responses body 重新适配"的路径都必须走它——prepareAdapter 与
// rewriteBodyOmitImages（图片降级重试拿的是适配前的快照，绕过它就会把工具丢光）。
// 无载体时工具列表原样不动，经典形状逐字节不变。
func applyCodexToolContext(doc map[string]any) (map[string]codexToolIdentity, error) {
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		return nil, err
	}
	if ctx.rewritten {
		doc["tools"] = ctx.tools
		doc["input"] = stripToolCarriers(doc["input"])
		rewriteInputNamespaceCalls(doc["input"], ctx)
		neutralizeNamespaceToolChoice(doc, ctx.restore)
	}
	return ctx.restore, nil
}

// stripToolCarriers 从 input 里移除工具载体项（其中的工具已被提取到顶层）。
// 两个转换器本来就会丢弃载体（Messages 走 default 分支、Chat 显式跳过），所以移除不改变
// 发给上游的内容；它的作用是让"载体已消费"成为显式状态——否则同一个 doc 被重复应用时，
// 载体里的工具会与已提升到顶层的同名工具撞车而误报冲突。
func stripToolCarriers(input any) any {
	arr, ok := input.([]any)
	if !ok {
		return input
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok && isToolCarrierItem(m) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// buildCodexToolContext 合并请求里声明的全部工具：顶层 tools 加 input 里
// additional_tools / tool_search_output 载体携带的工具，并展开 namespace。
//
// 同名冲突以**顶层声明为准**（与 cc-switch 一致）；展开名撞车（两个不同身份折叠到同一
// 个扁平名、或与顶层工具同名）则返回错误——上游无法区分，报错优于静默丢一个工具。
func buildCodexToolContext(doc map[string]any) (*codexToolContext, error) {
	ctx := &codexToolContext{restore: map[string]codexToolIdentity{}}
	top := anySlice(doc["tools"])
	carried := collectCarriedTools(doc["input"])
	if len(carried) == 0 {
		// 无载体：完全不碰工具列表，经典形状逐字节不变。
		ctx.tools = top
		return ctx, nil
	}
	ctx.rewritten = true

	// 顶层已占用的名字（含 function 与 custom），载体工具与它们同名时以顶层为准。
	taken := map[string]bool{}
	for _, t := range top {
		if name := toolDefinitionName(t); name != "" {
			taken[name] = true
		}
	}

	out := make([]any, 0, len(top)+len(carried))
	out = append(out, top...)
	for _, t := range carried {
		// 载体里的顶层工具（不在 namespace 里的）：名字原样，仅做去重。
		if isNamespaceTool(t) {
			ns := strings.TrimSpace(stringifyAny(toolField(t, "name")))
			if ns == "" {
				continue
			}
			for _, child := range namespaceChildren(t) {
				kind := stringifyAny(toolField(child, "type"))
				if kind != "function" && kind != "custom" {
					continue
				}
				childName := strings.TrimSpace(stringifyAny(toolField(child, "name")))
				if childName == "" {
					continue
				}
				flat := flattenNamespaceToolName(ns, childName)
				if taken[flat] {
					return nil, newAdapterError(
						"工具名冲突：namespace %q 的子工具 %q 展开为 %q，与顶层工具同名；请改名",
						ns, childName, flat)
				}
				if prev, ok := ctx.restore[flat]; ok && prev != (codexToolIdentity{Namespace: ns, Name: childName}) {
					return nil, newAdapterError(
						"工具名冲突：%q/%q 与 %q/%q 都展开为 %q；请改名",
						prev.Namespace, prev.Name, ns, childName, flat)
				}
				ctx.restore[flat] = codexToolIdentity{Namespace: ns, Name: childName}
				taken[flat] = true
				out = append(out, renameToolDefinition(child, flat))
			}
			continue
		}
		name := toolDefinitionName(t)
		if name == "" || taken[name] {
			continue // 无名或与顶层同名：以顶层为准
		}
		taken[name] = true
		out = append(out, t)
	}
	ctx.tools = out
	return ctx, nil
}

// isToolCarrierItem 判断是否为"工具载体"项：additional_tools / tool_search_output。
// 它们是 Responses 的私有扩展，工具本身由 applyCodexToolContext 提取；载体项带 role 却没有
// content，一旦被当成消息下发就会伪造一条空 user 消息（严格网关直接 400 —— cc-switch #7454
// 踩的就是这个坑）。Messages 转换器的类型分发天然丢弃未知类型，Chat 转换器没有兜底分支，
// 必须显式跳过。
func isToolCarrierItem(m map[string]any) bool {
	switch stringifyAny(m["type"]) {
	case "additional_tools", "tool_search_output":
		return true
	}
	return false
}

// collectCarriedTools 收集 input 里内联声明的工具：additional_tools 载体
// （codex 0.154+ 用它携带 functions/collaboration 执行沙箱与插件工具）与
// tool_search_output（动态加载的工具组）。返回各载体 tools 数组的拼接。
func collectCarriedTools(input any) []any {
	var out []any
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case []any:
			for _, item := range node {
				walk(item)
			}
		case map[string]any:
			if isToolCarrierItem(node) {
				out = append(out, anySlice(node["tools"])...)
				return
			}
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(input)
	return out
}

// rewriteInputNamespaceCalls 把 input 历史里带 namespace 的工具调用改写成扁平名。
// 多轮历史里早先的调用也带着 namespace，不改写的话模型会看到两套名字。
func rewriteInputNamespaceCalls(input any, ctx *codexToolContext) {
	if ctx == nil || len(ctx.restore) == 0 {
		return
	}
	walkToolItems(input, func(item map[string]any) {
		ns := strings.TrimSpace(stringifyAny(item["namespace"]))
		if ns == "" {
			return
		}
		name := strings.TrimSpace(stringifyAny(item["name"]))
		if name == "" {
			return
		}
		flat := flattenNamespaceToolName(ns, name)
		if identity, ok := ctx.restore[flat]; ok && identity == (codexToolIdentity{Namespace: ns, Name: name}) {
			item["name"] = flat
			delete(item, "namespace")
		}
	})
}

// neutralizeNamespaceToolChoice 处理 namespace 形状的 tool_choice：展开后 namespace 已不存在，
// 指定整个 namespace 的约束无法表达，降级为 auto；指定具体函数时把名字改写成扁平名
// （否则 tool_choice 引用的裸名在上游工具表里找不到）。
func neutralizeNamespaceToolChoice(doc map[string]any, restore map[string]codexToolIdentity) {
	choice, ok := doc["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	if stringifyAny(choice["type"]) == "namespace" {
		doc["tool_choice"] = "auto"
		return
	}
	ns := strings.TrimSpace(stringifyAny(choice["namespace"]))
	name := strings.TrimSpace(stringifyAny(choice["name"]))
	if ns == "" || name == "" {
		return
	}
	flat := flattenNamespaceToolName(ns, name)
	if identity, ok := restore[flat]; ok && identity == (codexToolIdentity{Namespace: ns, Name: name}) {
		choice["name"] = flat
		delete(choice, "namespace")
	}
}

// restoreNamespaceNames 把回程 payload 里扁平名的工具调用还原成 {name, namespace}。
// 适用于整份非流式响应体，也适用于单个 SSE 事件的 data JSON。
// 返回是否有改动。
func restoreNamespaceNames(value any, restore map[string]codexToolIdentity) bool {
	if len(restore) == 0 {
		return false
	}
	changed := false
	walkToolItems(value, func(item map[string]any) {
		flat := strings.TrimSpace(stringifyAny(item["name"]))
		identity, ok := restore[flat]
		if !ok {
			return
		}
		item["name"] = identity.Name
		if identity.Namespace != "" {
			item["namespace"] = identity.Namespace
		}
		changed = true
	})
	return changed
}

// walkToolItems 遍历 JSON 值，对 function_call / custom_tool_call 项调用 fn。
func walkToolItems(value any, fn func(map[string]any)) {
	switch node := value.(type) {
	case []any:
		for _, item := range node {
			walkToolItems(item, fn)
		}
	case map[string]any:
		switch stringifyAny(node["type"]) {
		case "function_call", "custom_tool_call":
			fn(node)
		}
		for _, child := range node {
			walkToolItems(child, fn)
		}
	}
}

// --- 小工具 ---

func anySlice(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return nil
}

func toolField(tool any, key string) any {
	if m, ok := tool.(map[string]any); ok {
		return m[key]
	}
	return nil
}

// toolDefinitionName 取工具定义的裸名（function/custom 都用 name 字段）。
func toolDefinitionName(tool any) string {
	return strings.TrimSpace(stringifyAny(toolField(tool, "name")))
}

func isNamespaceTool(tool any) bool {
	return stringifyAny(toolField(tool, "type")) == "namespace"
}

// namespaceChildren 取 namespace 工具的子工具（tools 或 children 字段）。
func namespaceChildren(tool any) []any {
	m, ok := tool.(map[string]any)
	if !ok {
		return nil
	}
	if arr := anySlice(m["tools"]); arr != nil {
		return arr
	}
	return anySlice(m["children"])
}

// renameToolDefinition 浅拷贝工具定义并改名（不改动原对象，避免污染请求）。
func renameToolDefinition(tool any, name string) any {
	m, ok := tool.(map[string]any)
	if !ok {
		return tool
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	out["name"] = name
	return out
}

// newNamespaceRestore 在还原表非空时返回链尾还原变换器，否则返回 nil。
// 经典形状（无 additional_tools 载体）下返回 nil，调用方走原来的路径，行为逐字节不变。
func newNamespaceRestore(restore map[string]codexToolIdentity) sseEventTransformer {
	if len(restore) == 0 {
		return nil
	}
	return newNamespaceRestoreSSETransformer(restore)
}

// chainRestorer 把链尾还原变换器接在 second 之后；restorer 为 nil 时原样返回 second。
func chainRestorer(second, restorer sseEventTransformer) sseEventTransformer {
	if restorer == nil {
		return second
	}
	return &chainedSSETransformer{first: second, second: restorer}
}

// namespaceRestoreSSETransformer 是链尾变换器：把适配器产出的 SSE 里扁平名的工具调用
// 还原成 {name, namespace}。按完整帧切分，未完成的帧尾留到下一次，跨 chunk 安全。
type namespaceRestoreSSETransformer struct {
	restore map[string]codexToolIdentity
	pending strings.Builder
}

func newNamespaceRestoreSSETransformer(restore map[string]codexToolIdentity) *namespaceRestoreSSETransformer {
	return &namespaceRestoreSSETransformer{restore: restore}
}

func (t *namespaceRestoreSSETransformer) Push(chunk string) string {
	if len(t.restore) == 0 || chunk == "" {
		return chunk
	}
	t.pending.WriteString(chunk)
	text := t.pending.String()
	// 最后一个空行之后的内容是未完成帧，留到下次
	cut := strings.LastIndex(text, "\n\n")
	if cut < 0 {
		t.pending.Reset()
		t.pending.WriteString(text)
		return ""
	}
	complete := text[:cut+2]
	t.pending.Reset()
	t.pending.WriteString(text[cut+2:])
	return t.restoreFrames(complete)
}

func (t *namespaceRestoreSSETransformer) Flush() string {
	if len(t.restore) == 0 {
		return ""
	}
	rest := t.pending.String()
	t.pending.Reset()
	if rest == "" {
		return ""
	}
	return t.restoreFrames(rest)
}

// restoreFrames 逐帧改写：把 data: 后面的 JSON 解析出来做名字还原，解析失败则原样保留。
func (t *namespaceRestoreSSETransformer) restoreFrames(text string) string {
	var out strings.Builder
	for _, frame := range strings.SplitAfter(text, "\n\n") {
		if frame == "" {
			continue
		}
		out.WriteString(t.restoreFrame(frame))
	}
	return out.String()
}

func (t *namespaceRestoreSSETransformer) restoreFrame(frame string) string {
	idx := strings.Index(frame, "data: ")
	if idx < 0 {
		return frame
	}
	head := frame[:idx+len("data: ")]
	body := frame[idx+len("data: "):]
	trailer := ""
	if strings.HasSuffix(body, "\n\n") {
		body, trailer = strings.TrimSuffix(body, "\n\n"), "\n\n"
	}
	body = strings.TrimSuffix(body, "\n")
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || trimmed == "[DONE]" {
		return frame
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return frame
	}
	if !restoreNamespaceNames(parsed, t.restore) {
		return frame
	}
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return frame
	}
	return head + string(rewritten) + trailer
}

// ProtocolIncomplete / TerminalFailed / BufferingThink 不实现：本变换器只做名字还原，
// 判定类信号由链首的适配器变换器提供（chainedSSETransformer 会把它们委托给链首）。
