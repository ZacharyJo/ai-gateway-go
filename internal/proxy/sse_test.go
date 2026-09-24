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

func TestStreamSSETruncatedStreamGetsNoDone(t *testing.T) {
	// A2 回归：读到真错误说明流是截断的，补 [DONE] 会让客户端把残缺响应当完整结果、不再重试
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
		if strings.Contains(body, "[DONE]") {
			t.Errorf("%s：截断流被补了 [DONE]: %q", tc.name, body)
		}
		if !strings.Contains(body, "data: partial") {
			t.Errorf("%s：已读到的数据没有转发: %q", tc.name, body)
		}
	}
	// 对照：干净 EOF 仍然补
	rr := httptest.NewRecorder()
	_, _ = streamSSE(rr, &truncatedReader{data: []byte("data: full\n\n"), err: io.EOF}, true)
	if !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("干净结束的流应补 [DONE]: %q", rr.Body.String())
	}
}

func TestStreamSSEAdaptedTruncatedStreamGetsNoDone(t *testing.T) {
	// 适配路径同理：转换器已发出的帧照常转发，但不补收尾哨兵
	sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\"}}\n\n"
	rr := httptest.NewRecorder()
	tr := newMessagesSSETransformer("m", nil)
	_, _ = streamSSEAdapted(rr, &truncatedReader{data: []byte(sse), err: errors.New("connection reset by peer")}, tr)
	body := rr.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Errorf("截断的适配流被补了 [DONE]: %q", body)
	}
	if !strings.Contains(body, "response.created") {
		t.Errorf("已转换的帧没有转发: %q", body)
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
