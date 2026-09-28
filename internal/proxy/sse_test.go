package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamSSE 是测试专用的无 usage 嗅探包装（生产路径走 streamSSEUsage）。
func streamSSE(w http.ResponseWriter, body io.Reader, doneNet bool) (kind string, truncated bool) {
	k, t, _, _ := streamSSEUsage(w, body, doneNet, nil)
	return k, t
}

func TestSSEDoneTracker(t *testing.T) {
	feedFinish := func(chunks ...string) string {
		tr := newSSEDoneTracker()
		for _, c := range chunks {
			tr.feed([]byte(c))
		}
		rr := httptest.NewRecorder()
		tr.finish(rr)
		return rr.Body.String()
	}
	// 已含 [DONE] → 不补帧
	if got := feedFinish("data: x\n\ndata: [DONE]\n\n"); got != "" {
		t.Errorf("already-terminated SSE got appended: %q", got)
	}
	// OpenAI 风格但没 [DONE] → 补帧
	if got := feedFinish(`data: {"type":"response.output_text.delta"}` + "\n\n"); !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("missing [DONE] not appended: %q", got)
	}
	// 没有 data: 行（非 OpenAI 风格）→ 不补帧
	if got := feedFinish(": comment\n\n"); got != "" {
		t.Errorf("non-OpenAI SSE got appended: %q", got)
	}
	// marker 跨 chunk 被切断也要识别
	if got := feedFinish("data: x\n\ndata: [DO", "NE]\n\n"); got != "" {
		t.Errorf("split [DONE] marker not detected: %q", got)
	}
}

func TestStreamSSEAppendsDone(t *testing.T) {
	rr := httptest.NewRecorder()
	_, _ = streamSSE(rr, strings.NewReader("data: hello\n\n"), true)
	if !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("stream without [DONE] not appended: %q", rr.Body.String())
	}
}

func TestStreamSSENoDoneNetForNativeAnthropic(t *testing.T) {
	// 原生 Anthropic 流以 message_stop 收尾、不认 [DONE]，doneNet=false 时不能补帧
	native := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rr := httptest.NewRecorder()
	_, _ = streamSSE(rr, strings.NewReader(native), false)
	if rr.Body.String() != native {
		t.Errorf("native Anthropic stream was modified:\n got %q\nwant %q", rr.Body.String(), native)
	}
	if strings.Contains(rr.Body.String(), "[DONE]") {
		t.Error("[DONE] injected into native Anthropic stream")
	}
}

type fixedChunkReader struct {
	chunks [][]byte
	index  int
}

func (r *fixedChunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	copy(p, chunk)
	return len(chunk), nil
}

func TestStreamSSEDoneWithLargeEventAndSplitMarker(t *testing.T) {
	large := "data: " + strings.Repeat("x", 512) + "\n\n"
	reader := &fixedChunkReader{chunks: [][]byte{
		[]byte(large), []byte("data: [DO"), []byte("NE]\n\n"),
	}}
	rr := httptest.NewRecorder()
	_, _ = streamSSE(rr, reader, true)
	if strings.Count(rr.Body.String(), "data: [DONE]") != 1 {
		t.Errorf("unexpected DONE handling: %q", rr.Body.String())
	}

	reader = &fixedChunkReader{chunks: [][]byte{[]byte(large)}}
	rr = httptest.NewRecorder()
	_, _ = streamSSE(rr, reader, true)
	if !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("large event missing fallback DONE: %q", rr.Body.String())
	}
}

// truncatedReader 先吐一段数据，再返回一个真网络错误（模拟 RST / 空闲超时取消）。
type truncatedReader struct {
	data []byte
	done bool
	err  error
}

func (r *truncatedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

// zeroNilReader 第一次返回 (0,nil)（合法空读），第二次干净 EOF。
type zeroNilReader struct {
	reads int
}

func (r *zeroNilReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return 0, nil
	}
	return 0, io.EOF
}

func TestStreamSSEZeroNilReadDoesNotSpin(t *testing.T) {
	// (0,nil) 是合法但异常的空读：实现必须继续读取而不是忙旋返回。
	// 空流（干净 EOF 但一个字节都没收到）现在按截断/失败终态上报（review 采纳：
	// 空 200 SSE 让客户端挂死且监控按成功记，属异常）。
	rr := httptest.NewRecorder()
	reader := &zeroNilReader{}
	kind, truncated := streamSSE(rr, reader, false)
	if kind != "" {
		t.Errorf("streamSSE = %q,%v, want no soft error", kind, truncated)
	}
	if !truncated {
		t.Errorf("streamSSE = %q,%v, want truncated (empty clean stream 是异常)", kind, truncated)
	}
}

func TestPrimeSSEEvent(t *testing.T) {
	// 完整首帧：返回 data、complete=true、已读字节。
	reader := strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\ndata: next\n\n")
	data, complete, buffered, err := primeSSEEvent(reader, 1024)
	if err != nil || !complete {
		t.Fatalf("primeSSEEvent = %q,%v,%v, want complete first frame", data, complete, err)
	}
	if !strings.Contains(data, "message_start") || !strings.Contains(string(buffered), "message_start") {
		t.Errorf("first frame not captured: data=%q buffered=%q", data, buffered)
	}

	// 空流：返回 io.EOF（调用方据此重试）。
	_, complete, _, err = primeSSEEvent(strings.NewReader(""), 1024)
	if err == nil || !errors.Is(err, io.EOF) || complete {
		t.Fatalf("empty stream: err=%v complete=%v, want EOF", err, complete)
	}

	// 非 EOF 断流且无任何数据：返回该错误。
	_, complete, _, err = primeSSEEvent(&truncatedReader{err: errors.New("read tcp: reset")}, 1024)
	if err == nil || complete {
		t.Fatalf("truncated empty stream: err=%v complete=%v, want error", err, complete)
	}

	// 有部分数据但无完整事件，随后非 EOF：也视为首包前失败。
	_, complete, buffered, err = primeSSEEvent(&truncatedReader{data: []byte("data: partial"), err: errors.New("read tcp: reset")}, 1024)
	if err == nil || complete {
		t.Fatalf("partial truncated stream: err=%v complete=%v, want error", err, complete)
	}
	if len(buffered) == 0 {
		t.Fatal("partial buffered bytes not returned")
	}

	// 超过 maxBytes 仍无完整事件：fail-open，返回已读字节和 nil。
	big := strings.NewReader("data: " + strings.Repeat("x", 512))
	_, complete, buffered, err = primeSSEEvent(big, 256)
	if err != nil || complete {
		t.Fatalf("oversized prime: err=%v complete=%v, want fail-open", err, complete)
	}
	if len(buffered) == 0 {
		t.Fatal("oversized buffered bytes not returned")
	}
}

func TestStreamSSETruncatedStreamGetsDone(t *testing.T) {
	// 截断流兜底：读到一个字节都没吐、或吐了数据但没给 [DONE] 的截断流，都补 [DONE] 终止传输，
	// 杜绝断尾流让客户端挂死。codex 按 response.completed 判定轮次完成，[DONE] 只终止传输、
	// 不会把残缺响应误判成完整成功（此前不补正是担心这个，实际换来的是 codex 端挂起/断开）。
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"连接被重置", errors.New("read tcp: connection reset by peer")},
		{"空闲超时取消", context.Canceled},
		{"上下文超时", context.DeadlineExceeded},
	} {
		rr := httptest.NewRecorder()
		_, _ = streamSSE(rr, &truncatedReader{data: []byte("data: partial\n\n"), err: tc.err}, true)
		body := rr.Body.String()
		if !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Errorf("%s：截断流应补 [DONE] 收尾: %q", tc.name, body)
		}
		if !strings.Contains(body, "data: partial") {
			t.Errorf("%s：已读到的数据没有转发: %q", tc.name, body)
		}
	}
	// 空截断流（一个字节都没吐）同样补 [DONE] 并标记截断
	rr := httptest.NewRecorder()
	_, _ = streamSSE(rr, &truncatedReader{data: nil, err: errors.New("connection reset by peer")}, true)
	if !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("空截断流应补 [DONE] 收尾: %q", rr.Body.String())
	}
	// 对照：干净 EOF 仍补
	rr = httptest.NewRecorder()
	_, _ = streamSSE(rr, &truncatedReader{data: []byte("data: full\n\n"), err: io.EOF}, true)
	if !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("干净结束的流应补 [DONE]: %q", rr.Body.String())
	}
}

func TestStreamSSEAdaptedTruncatedStreamGetsDone(t *testing.T) {
	// 适配路径同理：转换器已发出的帧照常转发，截断时补 [DONE] 终止传输、杜绝断尾流挂死
	sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\"}}\n\n"
	rr := httptest.NewRecorder()
	tr := newMessagesSSETransformer("m", nil)
	_, _ = streamSSEAdapted(rr, &truncatedReader{data: []byte(sse), err: errors.New("connection reset by peer")}, tr)
	body := rr.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("截断的适配流应补 [DONE] 收尾: %q", body)
	}
	if !strings.Contains(body, "response.created") {
		t.Errorf("已转换的帧没有转发: %q", body)
	}
}

// 上游已产出正文、但流在 message_stop 之前结束（mock 写完直接 return 也是这种：
// HTTP 响应正常终止 → 客户端读到干净 EOF）。必须补终态帧并上报截断，不能留断尾流。
const truncatedMessagesSSE = `data: {"type":"message_start","message":{"id":"m1","model":"m"}}` + "\n\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n"

func TestStreamSSEAdaptedNoMessageStopEmitsIncomplete(t *testing.T) {
	rr := httptest.NewRecorder()
	tr := newMessagesSSETransformer("m", nil)
	_, truncated := streamSSEAdapted(rr, &truncatedReader{data: []byte(truncatedMessagesSSE), err: io.EOF}, tr)
	out := rr.Body.String()
	if !truncated {
		t.Error("无 message_stop 的流应上报 truncated=true（干净 EOF 也一样）")
	}
	if !strings.Contains(out, "response.incomplete") {
		t.Errorf("应补 response.incomplete:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Errorf("不得谎报 completed:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("应以 [DONE] 收尾:\n%s", out)
	}
}

// 负向对照：正常发到 message_stop 的流不得被误报截断。
func TestStreamSSEAdaptedCompleteRoundNotTruncated(t *testing.T) {
	sse := truncatedMessagesSSE +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	rr := httptest.NewRecorder()
	tr := newMessagesSSETransformer("m", nil)
	_, truncated := streamSSEAdapted(rr, &truncatedReader{data: []byte(sse), err: io.EOF}, tr)
	out := rr.Body.String()
	if truncated {
		t.Error("正常收尾的流不该上报截断")
	}
	if !strings.Contains(out, "response.completed") {
		t.Errorf("应发 response.completed:\n%s", out)
	}
	if strings.Contains(out, "response.incomplete") || strings.Contains(out, "response.failed") {
		t.Errorf("正常收尾不得出现 incomplete/failed:\n%s", out)
	}
}

// 干净 EOF 但上游没发 finish_reason：同样是截断（Chat 路径）。
func TestStreamSSEChatCleanEOFWithoutFinishIsTruncated(t *testing.T) {
	chunk := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, truncated := streamSSEChat(rr, &truncatedReader{data: []byte(chunk), err: io.EOF}, tr)
	out := rr.Body.String()
	if !truncated {
		t.Error("无 finish_reason 的干净 EOF 应上报 truncated=true")
	}
	if !strings.Contains(out, "response.incomplete") {
		t.Errorf("应补 response.incomplete:\n%s", out)
	}
}

// 负向对照：正常收到 finish_reason 的 Chat 轮次不得被误报截断。
func TestStreamSSEChatCompleteRoundNotTruncated(t *testing.T) {
	chunk := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, truncated := streamSSEChat(rr, &truncatedReader{data: []byte(chunk), err: io.EOF}, tr)
	out := rr.Body.String()
	if truncated {
		t.Error("收到 finish_reason 的轮次不该上报截断")
	}
	if !strings.Contains(out, "response.completed") {
		t.Errorf("应发 response.completed:\n%s", out)
	}
}

func TestIsCleanStreamEnd(t *testing.T) {
	if !isCleanStreamEnd(io.EOF) {
		t.Error("io.EOF 应视为干净结束")
	}
	if !isCleanStreamEnd(fmt.Errorf("wrapped: %w", io.EOF)) {
		t.Error("包装过的 io.EOF 也应识别")
	}
	if isCleanStreamEnd(io.ErrUnexpectedEOF) || isCleanStreamEnd(context.Canceled) {
		t.Error("ErrUnexpectedEOF / context.Canceled 属于截断")
	}
	// nil 在 readErr 路径上不可达（err!=nil 才 break），但不应 panic
	if isCleanStreamEnd(nil) {
		t.Error("nil 不是真正的 EOF，现在不再视为干净结束")
	}
}

func TestSSEDoneTrackerJsonTextWithDONEDoesNotSuppressFallback(t *testing.T) {
	// G 项：响应 JSON 文本里恰好含 "[DONE]" 字样时不应抑制真正的收尾哨兵
	feedFinish := func(chunks ...string) string {
		tr := newSSEDoneTracker()
		for _, c := range chunks {
			tr.feed([]byte(c))
		}
		rr := httptest.NewRecorder()
		tr.finish(rr)
		return rr.Body.String()
	}
	// 响应体里含 "[DONE]" 字面量（不是 SSE 协议行）→ 仍应补 [DONE] 哨兵
	jsonWithDone := `data: {"text":"see [DONE] here"}` + "\n\n"
	if got := feedFinish(jsonWithDone); !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("JSON 文本含 [DONE] 字样时哨兵被误抑制: %q", got)
	}
	// 真正的 SSE 协议收尾行 → 不补
	if got := feedFinish("data: hello\n\n", "data: [DONE]\n\n"); got != "" {
		t.Errorf("真正的 [DONE] 行应阻止重复补帧: %q", got)
	}
}

// TestStreamSSEChatTruncatedGetsTerminalFrame 验证截断流兜底：上游非 EOF 结束时，
// 转换器被冲刷，codex 拿到 response.incomplete + [DONE] 的干净轮次终止，而不是断尾流。
// 回归：此前截断不调 Flush、不发 [DONE]，codex 把没有终止帧的 200 流当"还没执行完"挂起。
func TestStreamSSEChatTruncatedGetsTerminalFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"连接被重置", errors.New("read tcp: connection reset by peer")},
		{"空闲超时取消", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := []string{
				`data: {"id":"c","choices":[{"index":0,"delta":{"content":"<thinking>思考"}}]}` + "\n\n",
				`data: {"id":"c","choices":[{"index":0,"delta":{"content":"</thinking>答案"}}]}` + "\n\n",
			}
			rr := httptest.NewRecorder()
			tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
			_, truncated := streamSSEChat(rr, &truncatedReader{data: []byte(strings.Join(chunks, "")), err: tc.err}, tr)
			if !truncated {
				t.Error("截断应上报 truncated=true")
			}
			out := rr.Body.String()
			if !strings.Contains(out, "response.incomplete") {
				t.Errorf("截断流应补发 response.incomplete:\n%s", out)
			}
			if !strings.HasSuffix(out, "data: [DONE]\n\n") {
				t.Errorf("截断流应以 [DONE] 收尾:\n%s", out)
			}
			if got := collectDeltaText(out, "response.output_text.delta"); got != "答案" {
				t.Errorf("text = %q, want 答案\n%s", got, out)
			}
		})
	}
}

// TestStreamSSEChatTruncatedMidThinkEmitsFallbackText 验证思考中途被掐断（无闭合标签）时，
// 兜底 Flush 把缓冲的思考当正文下发，codex 不会拿到只有 reasoning、没有 message 的空轮。
func TestStreamSSEChatTruncatedMidThinkEmitsFallbackText(t *testing.T) {
	chunk := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"<thinking>思考到一半被掐断"}}]}` + "\n\n"
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, truncated := streamSSEChat(rr, &truncatedReader{data: []byte(chunk), err: errors.New("connection reset by peer")}, tr)
	if !truncated {
		t.Error("截断应上报 truncated=true")
	}
	out := rr.Body.String()
	if got := collectDeltaText(out, "response.output_text.delta"); got != "思考到一半被掐断" {
		t.Errorf("未闭合 think 应兜底成正文, got %q\n%s", got, out)
	}
	if !strings.Contains(out, "response.incomplete") || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("截断流应补 response.incomplete + [DONE]:\n%s", out)
	}
}

// blockReader 依次返回 data[i]，在返回第二个及以后的 chunk 前阻塞 wait——
// 模拟上游长思考期间停顿（转换器缓冲 think、对客户端零输出）的窗口。
type blockReader struct {
	data  []string
	wait  time.Duration
	index int
}

func (r *blockReader) Read(p []byte) (int, error) {
	if r.index >= len(r.data) {
		return 0, io.EOF
	}
	chunk := r.data[r.index]
	r.index++
	if r.index > 1 {
		time.Sleep(r.wait)
	}
	n := copy(p, chunk)
	return n, nil
}

// TestStreamSSEChatKeepaliveDuringThink 验证长思考静默防护：think 缓冲期间流循环
// 发 SSE 注释帧保活，杜绝客户端因流空闲超时误判断流。
func TestStreamSSEChatKeepaliveDuringThink(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 5 * time.Millisecond
	defer func() { keepaliveInterval = old }()

	chunks := []string{
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"<thinking>长思考"}}]}` + "\n\n",
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"</thinking>正文"}}]}` + "\n\n",
	}
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, _ = streamSSEChat(rr, &blockReader{data: chunks, wait: 300 * time.Millisecond}, tr)
	out := rr.Body.String()
	if !strings.Contains(out, keepaliveFrame) {
		t.Errorf("思考缓冲期应发保活注释帧:\n%q", out)
	}
	if !strings.Contains(out, "response.incomplete") {
		t.Errorf("流应在无 finish_reason 时以 incomplete 收尾:\n%s", out)
	}
}

// zeroNilForeverReader 永远返回 (0,nil)：验证预检不会忙旋挂死。
type zeroNilForeverReader struct{ reads int }

func (r *zeroNilForeverReader) Read(p []byte) (int, error) {
	r.reads++
	return 0, nil
}

func TestPrimeSSEEventBoundedOnZeroNilReads(t *testing.T) {
	r := &zeroNilForeverReader{}
	data, complete, buffered, err := primeSSEEvent(r, 1024)
	if complete || data != "" || len(buffered) != 0 || err != nil {
		t.Errorf("primeSSEEvent = (%q,%v,%d bytes,%v), want fail-open empty", data, complete, len(buffered), err)
	}
	if r.reads > primeMaxEmptyReads+1 {
		t.Errorf("read %d times, want bounded by %d（否则忙旋挂死）", r.reads, primeMaxEmptyReads)
	}
}

func TestIsFailureSSEDataNullErrorIsNotFailure(t *testing.T) {
	// 网关常在每个 chunk 回显 "error":null：不能判为失败终态。
	if isFailureSSEData(`{"choices":[{"delta":{"content":"hi"}}],"error":null}`) {
		t.Error(`"error":null 不应判为失败终态`)
	}
	if !isFailureSSEData(`{"error":{"message":"boom"}}`) {
		t.Error("非 nil error 应判为失败终态")
	}
}

// TestStreamSSEChatBareDoneSynthesizesFailure 验证"裸 [DONE] 空轮"（上游只发了一帧 [DONE]、
// 没有任何协议帧）按截断失败收尾：客户端拿不到 response.completed 会自行重试
// （见 codex codex-rs/core/tests/suite/stream_no_completed.rs），代理不该把它记成 200 成功。
// 与 Messages 路径同口径（见 TestStreamSSEAdaptedBareDoneSynthesizesFailure）。
func TestStreamSSEChatBareDoneSynthesizesFailure(t *testing.T) {
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, truncated := streamSSEChat(rr, &truncatedReader{data: []byte("data: [DONE]\n\n"), err: io.EOF}, tr)
	out := rr.Body.String()
	if !truncated {
		t.Errorf("裸 [DONE] 空轮应上报截断:\n%s", out)
	}
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Errorf("应合成 response.failed(stream_truncated):\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("应以 [DONE] 收尾:\n%q", out)
	}
}

// TestStreamSSEAdaptedBareDoneSynthesizesFailure 验证 Messages 路径的裸 [DONE] 空轮同样按
// 截断失败收尾：上游的 [DONE] 被吞掉，由 synthesizeTerminal 统一补终态 + [DONE]。
func TestStreamSSEAdaptedBareDoneSynthesizesFailure(t *testing.T) {
	rr := httptest.NewRecorder()
	tr := newMessagesSSETransformer("m", nil)
	_, truncated := streamSSEAdapted(rr, &truncatedReader{data: []byte("data: [DONE]\n\n"), err: io.EOF}, tr)
	out := rr.Body.String()
	if !truncated {
		t.Errorf("裸 [DONE] 空轮应上报截断:\n%s", out)
	}
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Errorf("应合成 response.failed(stream_truncated):\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("应以 [DONE] 收尾:\n%q", out)
	}
}

// TestStreamSSEChatTrulyEmptyStillTruncated 对照：一个字节都没收到的**完全空流**仍按
// stream_truncated 失败上报——裸 [DONE] 门的例外只覆盖"上游声明了结束"的情形。
func TestStreamSSEChatTrulyEmptyStillTruncated(t *testing.T) {
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, truncated := streamSSEChat(rr, &truncatedReader{data: nil, err: io.EOF}, tr)
	out := rr.Body.String()
	if !truncated {
		t.Error("完全空流应上报 truncated=true")
	}
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Errorf("完全空流应报 response.failed(stream_truncated):\n%s", out)
	}
}

// TestStreamSSEChatEventsWithoutFinishStillTruncated 对照：发过协议帧但没到 finish_reason
// 时仍算截断（有产出但没收尾，不能谎报成功）。
func TestStreamSSEChatEventsWithoutFinishStillTruncated(t *testing.T) {
	chunk := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	_, truncated := streamSSEChat(rr, &truncatedReader{data: []byte(chunk), err: io.EOF}, tr)
	if !truncated {
		t.Error("发过协议帧但无 finish_reason 应上报截断")
	}
}
