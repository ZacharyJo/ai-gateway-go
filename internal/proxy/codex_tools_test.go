package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// liteRequest 是 codex use_responses_lite 形状的请求骨架（结构照 2026-09-28 实抓的
// 客户端原始请求：工具不在 tools 字段，而在 input 的 additional_tools 项里，按 namespace 分组；
// functions 下的 exec 是 custom/freeform，其余是 function）。
func liteRequest() map[string]any {
	return map[string]any{
		"model": "DeepSeek-V4.1-Flash",
		"input": []any{
			map[string]any{
				"type": "additional_tools", "role": "developer", "id": "at_1",
				"tools": []any{
					map[string]any{
						"type": "namespace", "name": "functions", "description": "",
						"tools": []any{
							map[string]any{"type": "custom", "name": "exec", "description": "run js",
								"format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}},
							map[string]any{"type": "function", "name": "wait", "description": "wait on cell",
								"parameters": map[string]any{"type": "object"}},
						},
					},
					map[string]any{
						"type": "namespace", "name": "collaboration", "description": "sub-agents",
						"tools": []any{
							map[string]any{"type": "function", "name": "spawn_agent", "description": "spawn",
								"parameters": map[string]any{"type": "object"}},
						},
					},
				},
			},
			map[string]any{"type": "message", "role": "developer",
				"content": []any{map[string]any{"type": "input_text", "text": "You are Codex."}}},
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	}
}

func toolNames(tools []any) []string {
	var out []string
	for _, t := range tools {
		out = append(out, toolDefinitionName(t))
	}
	return out
}

func TestFlattenNamespaceToolName(t *testing.T) {
	// 短名直接拼接
	if got := flattenNamespaceToolName("functions", "exec_command"); got != "functions__exec_command" {
		t.Errorf("short name = %q", got)
	}
	// 超 64 字节：前缀 + __ + sha256 前 8 字节（16 hex），总长 ≤ 64
	long := strings.Repeat("a", 80)
	got := flattenNamespaceToolName("mcp__srv__", long)
	if len(got) > codexToolNameMaxLen {
		t.Errorf("flat name too long: %d", len(got))
	}
	if !strings.Contains(got, "__") || len(got) != codexToolNameMaxLen {
		t.Errorf("truncated form unexpected: %q (len=%d)", got, len(got))
	}
	// 确定性：同一输入必须得到同一结果（展开与还原靠它对齐，不传状态）
	if again := flattenNamespaceToolName("mcp__srv__", long); again != got {
		t.Errorf("not deterministic: %q vs %q", got, again)
	}
}

func TestBuildCodexToolContextLiftsCarriedTools(t *testing.T) {
	ctx, err := buildCodexToolContext(liteRequest())
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	if !ctx.rewritten {
		t.Fatal("有载体时应标记 rewritten")
	}
	names := toolNames(ctx.tools)
	want := []string{"functions__exec", "functions__wait", "collaboration__spawn_agent"}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("tools[%d] = %q, want %q（全部 %v）", i, names[i], n, names)
		}
	}
	// custom 子工具必须被提升且保留 type=custom（cc-switch 会丢掉它，我们有意偏离）
	if got := stringifyAny(toolField(ctx.tools[0], "type")); got != "custom" {
		t.Errorf("functions__exec 的 type = %q, want custom", got)
	}
	if got := stringifyAny(toolField(ctx.tools[0], "format")); got == "" {
		t.Error("custom 子工具的 format 定义应随提升保留")
	}
	// 还原表：只含来自 namespace 的工具
	if id, ok := ctx.restore["collaboration__spawn_agent"]; !ok || id.Namespace != "collaboration" || id.Name != "spawn_agent" {
		t.Errorf("restore map 缺 collaboration__spawn_agent: %+v", ctx.restore)
	}
	if _, ok := ctx.restore["functions__exec"]; !ok {
		t.Error("restore map 应含 functions__exec")
	}
}

func TestBuildCodexToolContextNoCarrierIsUntouched(t *testing.T) {
	// 无载体：工具列表原样返回，且不标记 rewritten（经典形状逐字节不变的保证）
	doc := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "plain", "parameters": map[string]any{}},
		},
	}
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	if ctx.rewritten {
		t.Error("无载体时不应标记 rewritten")
	}
	if len(ctx.tools) != 1 || toolDefinitionName(ctx.tools[0]) != "plain" {
		t.Errorf("tools = %v, want 原样", toolNames(ctx.tools))
	}
	if len(ctx.restore) != 0 {
		t.Errorf("无 namespace 时 restore 应为空: %v", ctx.restore)
	}
}

func TestBuildCodexToolContextTopLevelWinsOnDuplicate(t *testing.T) {
	// 载体工具与顶层同名：以顶层声明为准（与 cc-switch 一致）
	doc := liteRequest()
	doc["tools"] = []any{
		map[string]any{"type": "function", "name": "wait", "description": "top-level wait",
			"parameters": map[string]any{"type": "object"}},
	}
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	count := 0
	for _, n := range toolNames(ctx.tools) {
		if n == "wait" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("wait 出现 %d 次，want 1（以顶层为准去重）: %v", count, toolNames(ctx.tools))
	}
	if _, ok := ctx.restore["wait"]; ok {
		t.Error("顶层 wait 不应进还原表")
	}
}

func TestBuildCodexToolContextErrorsOnCollision(t *testing.T) {
	// 两个不同身份折叠到同一个扁平名 → 必须报错，不能静默丢一个
	doc := map[string]any{
		"input": []any{
			map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
				map[string]any{"type": "namespace", "name": "a", "tools": []any{
					map[string]any{"type": "function", "name": "b__c", "parameters": map[string]any{}},
				}},
				map[string]any{"type": "namespace", "name": "a__b", "tools": []any{
					map[string]any{"type": "function", "name": "c", "parameters": map[string]any{}},
				}},
			}},
		},
	}
	if _, err := buildCodexToolContext(doc); err == nil {
		t.Fatal("扁平名冲突应报错（两个不同身份都展开为 a__b__c）")
	} else if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误信息应说明冲突: %v", err)
	}
}

func TestRewriteInputNamespaceCalls(t *testing.T) {
	doc := liteRequest()
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	// 多轮历史里早先的调用带 namespace
	doc["input"] = append(anySlice(doc["input"]),
		map[string]any{"type": "function_call", "name": "spawn_agent", "namespace": "collaboration",
			"call_id": "c1", "arguments": "{}"},
		map[string]any{"type": "custom_tool_call", "name": "exec", "namespace": "functions",
			"call_id": "c2", "input": "x"},
		map[string]any{"type": "function_call", "name": "plain_tool", "call_id": "c3", "arguments": "{}"},
	)
	rewriteInputNamespaceCalls(doc["input"], ctx)

	items := anySlice(doc["input"])
	fc := items[len(items)-3].(map[string]any)
	if fc["name"] != "collaboration__spawn_agent" {
		t.Errorf("历史调用未改写: %v", fc["name"])
	}
	if _, has := fc["namespace"]; has {
		t.Error("改写后应删掉 namespace 字段")
	}
	if fc["call_id"] != "c1" {
		t.Errorf("call_id 不该被改动: %v", fc["call_id"])
	}
	cc := items[len(items)-2].(map[string]any)
	if cc["name"] != "functions__exec" {
		t.Errorf("custom 历史调用未改写: %v", cc["name"])
	}
	// 不带 namespace 的调用不动
	plain := items[len(items)-1].(map[string]any)
	if plain["name"] != "plain_tool" {
		t.Errorf("无 namespace 的调用被误改: %v", plain["name"])
	}
}

func TestNeutralizeNamespaceToolChoice(t *testing.T) {
	ctx, err := buildCodexToolContext(liteRequest())
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	// 整个 namespace：降级为 auto
	doc := map[string]any{"tool_choice": map[string]any{"type": "namespace", "name": "functions"}}
	neutralizeNamespaceToolChoice(doc, ctx.restore)
	if doc["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", doc["tool_choice"])
	}
	// 指定具体函数：名字改写成扁平名，并去掉 namespace 字段
	doc = map[string]any{"tool_choice": map[string]any{
		"type": "function", "name": "spawn_agent", "namespace": "collaboration"}}
	neutralizeNamespaceToolChoice(doc, ctx.restore)
	choice := doc["tool_choice"].(map[string]any)
	if choice["name"] != "collaboration__spawn_agent" {
		t.Errorf("tool_choice name = %v, want 扁平名", choice["name"])
	}
	if _, has := choice["namespace"]; has {
		t.Error("tool_choice 的 namespace 应被删除")
	}
	// 非 namespace 形状不动
	doc = map[string]any{"tool_choice": map[string]any{"type": "function", "name": "x"}}
	neutralizeNamespaceToolChoice(doc, ctx.restore)
	if doc["tool_choice"].(map[string]any)["name"] != "x" {
		t.Error("无 namespace 的 tool_choice 不应被改动")
	}
}

// TestApplyCodexToolContextIsIdempotent 锁定：同一份 doc 重复应用不会误报工具名冲突。
// 载体在展开后被消费掉（工具已提升到顶层），第二次应用应是无操作。
func TestApplyCodexToolContextIsIdempotent(t *testing.T) {
	doc := liteRequest()
	if _, err := applyCodexToolContext(doc); err != nil {
		t.Fatalf("首次应用: %v", err)
	}
	first := toolNames(anySlice(doc["tools"]))
	if _, err := applyCodexToolContext(doc); err != nil {
		t.Fatalf("重复应用不应报错（载体应已被消费）: %v", err)
	}
	second := toolNames(anySlice(doc["tools"]))
	if len(first) != len(second) {
		t.Errorf("重复应用改变了工具列表: %v → %v", first, second)
	}
}

func TestRestoreNamespaceNamesNonStreaming(t *testing.T) {
	doc := liteRequest()
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	body := map[string]any{
		"output": []any{
			map[string]any{"type": "function_call", "name": "collaboration__spawn_agent",
				"call_id": "c1", "arguments": "{}"},
			map[string]any{"type": "custom_tool_call", "name": "functions__exec",
				"call_id": "c2", "input": "js"},
			map[string]any{"type": "function_call", "name": "plain_tool", "call_id": "c3"},
		},
	}
	if !restoreNamespaceNames(body, ctx.restore) {
		t.Fatal("应报告有改动")
	}
	items := anySlice(body["output"])
	fc := items[0].(map[string]any)
	if fc["name"] != "spawn_agent" || fc["namespace"] != "collaboration" {
		t.Errorf("function_call 未还原: %v", fc)
	}
	cc := items[1].(map[string]any)
	if cc["name"] != "exec" || cc["namespace"] != "functions" {
		t.Errorf("custom_tool_call 未还原: %v", cc)
	}
	plain := items[2].(map[string]any)
	if plain["name"] != "plain_tool" {
		t.Errorf("未映射的调用被误改: %v", plain["name"])
	}
	if _, has := plain["namespace"]; has {
		t.Error("未映射的调用不该被加上 namespace")
	}
}

func TestNamespaceRestoreSSETransformerStreaming(t *testing.T) {
	doc := liteRequest()
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	tr := newNamespaceRestoreSSETransformer(ctx.restore)

	added := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"collaboration__spawn_agent","call_id":"c1","arguments":""}}` + "\n\n"
	done := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","name":"functions__exec","call_id":"c2","input":"js"}}` + "\n\n"
	text := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"

	// 逐字节喂进去（模拟跨 chunk），结果应与整段喂入一致
	var got strings.Builder
	for _, part := range []string{text + added, done} {
		got.WriteString(tr.Push(part))
	}
	got.WriteString(tr.Flush())
	out := got.String()

	if !strings.Contains(out, `"name":"spawn_agent"`) || !strings.Contains(out, `"namespace":"collaboration"`) {
		t.Errorf("流式 function_call 未还原:\n%s", out)
	}
	if !strings.Contains(out, `"name":"exec"`) || !strings.Contains(out, `"namespace":"functions"`) {
		t.Errorf("流式 custom_tool_call 未还原:\n%s", out)
	}
	// 非工具帧原样保留
	if !strings.Contains(out, "response.created") {
		t.Errorf("非工具帧被破坏:\n%s", out)
	}
	// 每帧仍是合法 JSON（能解析回来）
	for _, frame := range strings.Split(out, "\n\n") {
		i := strings.Index(frame, "data: ")
		if i < 0 {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(strings.TrimSpace(frame[i+6:])), &parsed); err != nil {
			t.Errorf("帧不是合法 JSON: %v\n%s", err, frame)
		}
	}
}

func TestNamespaceRestoreSSETransformerSplitsAcrossChunks(t *testing.T) {
	doc := liteRequest()
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	tr := newNamespaceRestoreSSETransformer(ctx.restore)
	frame := `data: {"type":"response.output_item.done","item":{"type":"function_call","name":"functions__wait"}}` + "\n\n"

	// 把一个帧切成三段喂入：只有最后一段才会产出完整帧
	var out strings.Builder
	out.WriteString(tr.Push(frame[:20]))
	out.WriteString(tr.Push(frame[20:45]))
	out.WriteString(tr.Push(frame[45:]))
	out.WriteString(tr.Flush())

	if !strings.Contains(out.String(), `"namespace":"functions"`) {
		t.Errorf("跨 chunk 切分后未还原:\n%s", out.String())
	}
}

// --- 端到端：走完整的 Forward 路径（请求侧展开 + 回程还原）---

// TestForwardResponsesLiteCarriedToolsReachUpstream 验证 lite 形状的请求里，载体携带的
// 工具（含 namespace 下的 custom 工具）会被展开并真正发给上游。
// 回归：此前 additional_tools 项被当成消息遍历的 default 分支丢掉，模型一个工具都拿不到。
func TestForwardResponsesLiteCarriedToolsReachUpstream(t *testing.T) {
	clearProxyEnv(t)
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude sonnet 5"}}`, ``,
			`data: {"type":"message_stop"}`, ``,
		}, "\n") + "\n"))
	}))
	defer upstream.Close()

	reqBody := liteRequest()
	reqBody["model"] = "claude sonnet 5" // 非 GPT → 走 Messages 适配
	reqBody["stream"] = true
	raw, _ := json.Marshal(reqBody)

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got == nil {
		t.Fatal("上游未收到请求")
	}
	names := toolNames(anySlice(got["tools"]))
	for _, want := range []string{"functions__exec", "functions__wait", "collaboration__spawn_agent"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("上游 tools 缺 %q，实际 %v", want, names)
		}
	}
	// 载体不能变成一条 content 为空的 system 消息（严格网关会 400）
	for i, m := range anySlice(got["messages"]) {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if c, has := mm["content"]; !has || c == nil {
			t.Errorf("messages[%d] 丢了 content: %v", i, mm)
		}
	}
}

// TestForwardResponsesLiteRestoresNamespaceOnToolCall 验证回程把展开名还原成
// codex 认的 {name, namespace}：function_call 与 custom_tool_call 两条都要还原，
// 且扁平名不得泄漏给客户端（否则客户端会把它当未知工具）。
func TestForwardResponsesLiteRestoresNamespaceOnToolCall(t *testing.T) {
	clearProxyEnv(t)
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude sonnet 5"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"collaboration__spawn_agent"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"task\":\"x\"}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"functions__exec"}}`, ``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"1+1\"}"}}`, ``,
		`data: {"type":"content_block_stop","index":1}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	reqBody := liteRequest()
	reqBody["model"] = "claude sonnet 5"
	reqBody["stream"] = true
	raw, _ := json.Marshal(reqBody)

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	out := rr.Body.String()
	// function_call 还原
	if !strings.Contains(out, `"name":"spawn_agent"`) || !strings.Contains(out, `"namespace":"collaboration"`) {
		t.Errorf("function_call 未还原 {name, namespace}:\n%s", out)
	}
	// custom_tool_call 还原（functions 下的 exec 是 freeform）
	if !strings.Contains(out, `"type":"custom_tool_call"`) || !strings.Contains(out, `"name":"exec"`) ||
		!strings.Contains(out, `"namespace":"functions"`) {
		t.Errorf("custom_tool_call 未还原:\n%s", out)
	}
	// 扁平名不得泄漏
	for _, leak := range []string{"collaboration__spawn_agent", "functions__exec"} {
		if strings.Contains(out, leak) {
			t.Errorf("扁平名 %q 泄漏到客户端:\n%s", leak, out)
		}
	}
}

// TestForwardResponsesLiteImageFallbackKeepsTools 锁定：图片降级重试也必须带着工具。
// 降级拿的是**适配前**的原始 Responses body（工具还在 additional_tools 载体里），
// rewriteBodyOmitImages 若不走 applyCodexToolContext，重试请求会一个工具都不带——
// 与最初那个"工具静默全丢"是同一类问题，只是发生在次级路径上。
func TestForwardResponsesLiteImageFallbackKeepsTools(t *testing.T) {
	clearProxyEnv(t)
	var attempts atomic.Int32
	var retryTools []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Model do not support image input.","type":"BadRequest","param":"image_url","code":"InvalidParameter"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(b, &doc)
		retryTools = toolNames(anySlice(doc["tools"]))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	reqBody := liteRequest()
	reqBody["model"] = "claude sonnet 5"
	reqBody["stream"] = false
	// 追加一条带图片的 user 消息，触发反应式图片降级
	reqBody["input"] = append(anySlice(reqBody["input"]), map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,abc="}},
	})
	raw, _ := json.Marshal(reqBody)

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("server hits = %d, want 2（一次 400 + 一次降级重试）", attempts.Load())
	}
	for _, want := range []string{"functions__exec", "functions__wait", "collaboration__spawn_agent"} {
		found := false
		for _, n := range retryTools {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("降级重试的上游 tools 缺 %q，实际 %v", want, retryTools)
		}
	}
}

// TestNamespaceRestoreHandlesNestedCompletedFrame 锁定：还原要递归覆盖嵌套项。
// 适配器会在 response.completed 帧里带完整 output 数组，客户端可能以它为权威重建；
// 只还原顶层项会让客户端拿到的工具名与 .done 帧不一致。
func TestNamespaceRestoreHandlesNestedCompletedFrame(t *testing.T) {
	doc := liteRequest()
	ctx, err := buildCodexToolContext(doc)
	if err != nil {
		t.Fatalf("buildCodexToolContext: %v", err)
	}
	tr := newNamespaceRestoreSSETransformer(ctx.restore)
	frame := `data: {"type":"response.completed","response":{"output":[{"type":"function_call","name":"collaboration__spawn_agent","call_id":"c1","arguments":"{}"},{"type":"custom_tool_call","name":"functions__exec","call_id":"c2","input":"js"}]}}` + "\n\n"

	out := tr.Push(frame) + tr.Flush()
	if !strings.Contains(out, `"name":"spawn_agent"`) || !strings.Contains(out, `"namespace":"collaboration"`) {
		t.Errorf("completed 帧里的 function_call 未还原:\n%s", out)
	}
	if !strings.Contains(out, `"name":"exec"`) || !strings.Contains(out, `"namespace":"functions"`) {
		t.Errorf("completed 帧里的 custom_tool_call 未还原:\n%s", out)
	}
}

// TestForwardResponsesLiteChatAdapterRestoresNamespace 锁定 Chat 适配路径也挂了还原。
// 两条适配路径（Messages / Chat）各自接线，漏一条就会静默把扁平名透给客户端。
func TestForwardResponsesLiteChatAdapterRestoresNamespace(t *testing.T) {
	clearProxyEnv(t)
	sse := strings.Join([]string{
		`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"collaboration__spawn_agent","arguments":""}}]},"finish_reason":null}]}`, ``,
		`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"task\":\"x\"}"}}]},"finish_reason":null}]}`, ``,
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, ``,
		`data: [DONE]`, ``,
	}, "\n") + "\n"
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	reqBody := liteRequest()
	reqBody["model"] = "claude sonnet 5"
	reqBody["stream"] = true
	raw, _ := json.Marshal(reqBody)

	cfg := testConfig(upstream.URL)
	cfg.UpstreamWire = "chat" // 强制走 Chat 适配路径
	up := newTestUpstream(t, cfg)
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	out := rr.Body.String()
	if !strings.Contains(out, `"name":"spawn_agent"`) || !strings.Contains(out, `"namespace":"collaboration"`) {
		t.Errorf("Chat 路径未还原 {name, namespace}:\n%s", out)
	}
	if strings.Contains(out, "collaboration__spawn_agent") {
		t.Errorf("Chat 路径把扁平名泄漏给客户端:\n%s", out)
	}
	// 载体项不得变成空 content 的消息（严格网关会 400；cc-switch #7454 同类）
	if got != nil {
		for i, raw := range anySlice(got["messages"]) {
			mm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch c := mm["content"].(type) {
			case nil:
				t.Errorf("messages[%d] 的 content 为 null: %v", i, mm)
			case string:
				if strings.TrimSpace(c) == "" {
					t.Errorf("messages[%d] 的 content 为空串（载体被当成消息了）: %v", i, mm)
				}
			}
		}
	}
}
