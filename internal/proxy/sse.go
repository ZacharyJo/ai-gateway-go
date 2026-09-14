package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// doneLinePattern 匹配 SSE 的 [DONE] 收尾行（data: [DONE] 或 data:[DONE]），
// 用于区分协议哨兵与响应 JSON 文本里恰好含 [DONE] 字样的情况。
var doneLinePattern = regexp.MustCompile(`(?m)^data:\s*\[DONE\]`)

// isSSE 判断上游响应是否为 SSE 流。
func isSSE(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// streamSSE 把上游 SSE 逐块写回客户端并刷新（保证首字低延迟）。
// doneNet=true 时做 [DONE] 兜底：OpenAI 风格 SSE 若没以 data: [DONE] 收尾则补一帧，防客户端卡住。
// 原生 Anthropic 流（/v1/messages 透传）必须传 false —— 它以 message_stop 收尾、不认 [DONE]。
// 返回 (软错误类型, 流是否截断)。
// 软错误：命中时已向下游写 server_overloaded 帧并结束流，kind 为类型字符串。
// 截断：true 表示流在 EOF 之前就结束（空闲超时/RST），调用方可据此记非 200 终态。
func streamSSE(w http.ResponseWriter, body io.Reader, doneNet bool) (kind string, truncated bool) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	tracker := newSSEDoneTracker()
	detector := &streamErrorDetector{}
	var readErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return "", false // 客户端断开
			}
			if doneNet {
				tracker.feed(buf[:n])
			}
			rc.Flush()
			if k := detector.feed(buf[:n]); k != "" {
				_, _ = w.Write([]byte(overloadedErrorFrame))
				rc.Flush()
				return k, false
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	clean := isCleanStreamEnd(readErr)
	if doneNet && clean {
		tracker.finish(w)
	}
	return "", !clean
}

// streamSSEAdapted 把上游 Messages SSE 增量转换成 Responses SSE 后逐块写回并刷新。
// 返回 (软错误类型, 流是否截断)，语义与 streamSSE 一致。
func streamSSEAdapted(w http.ResponseWriter, body io.Reader, t *MessagesSSETransformer) (kind string, truncated bool) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	tracker := newSSEDoneTracker()
	detector := &streamErrorDetector{}
	write := func(s string) bool {
		if s == "" {
			return true
		}
		if _, err := w.Write([]byte(s)); err != nil {
			return false
		}
		tracker.feed([]byte(s))
		rc.Flush()
		return true
	}
	var readErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			k := detector.feed(buf[:n])
			// 软错误优先：先写 server_overloaded 帧，再写转换后内容；
			// 反过来写的话转换器会先发 response.failed + [DONE]，客户端在 [DONE] 处停读，
			// 后续的 server_overloaded 帧永远到不了客户端。
			if k != "" {
				_, _ = w.Write([]byte(overloadedErrorFrame))
				rc.Flush()
				write(t.Push(string(buf[:n])))
				return k, false
			}
			if !write(t.Push(string(buf[:n]))) {
				return "", false
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	// 截断流（readErr != io.EOF）不调 Flush，避免把不完整的 pending 数据发给客户端
	if isCleanStreamEnd(readErr) {
		write(t.Flush())
		tracker.finish(w)
	}
	return "", !isCleanStreamEnd(readErr)
}

// streamSSEChat 把上游 Chat Completions SSE 增量转换成 Responses SSE 后逐块写回并刷新。
// 返回 (软错误类型, 流是否截断)，语义与 streamSSE 一致。
func streamSSEChat(w http.ResponseWriter, body io.Reader, t *ChatSSETransformer) (kind string, truncated bool) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	detector := &streamErrorDetector{}
	write := func(s string) bool {
		if s == "" {
			return true
		}
		if _, err := w.Write([]byte(s)); err != nil {
			return false
		}
		rc.Flush()
		return true
	}
	pending := ""
	var readErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			k := detector.feed(buf[:n])
			if k != "" {
				_, _ = w.Write([]byte(overloadedErrorFrame))
				rc.Flush()
				return k, false
			}
			// 按行切分处理 SSE data 行
			chunk := pending + string(buf[:n])
			pending = ""
			lines := splitLines(chunk)
			for i, line := range lines {
				if i == len(lines)-1 {
					// 最后一段可能不完整
					pending = line
					break
				}
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "data:") {
					data := strings.TrimSpace(line[5:])
					if out := t.Push(data); out != "" {
						if !write(out) {
							return "", false
						}
					}
				}
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	// 处理剩余 pending
	if pending != "" {
		pending = strings.TrimSpace(pending)
		if strings.HasPrefix(pending, "data:") {
			data := strings.TrimSpace(pending[5:])
			write(t.Push(data))
		}
	}
	if isCleanStreamEnd(readErr) {
		write(t.Flush())
	}
	return "", !isCleanStreamEnd(readErr)
}

// isCleanStreamEnd 判断上游流是否**干净结束**（读到 EOF）。
// 空闲超时取消 context、连接被 RST 等都会返回非 EOF 错误，属于截断。
// err==nil：io.Reader 合约要求"读完后下一次 Read 才返回 EOF"，这里 readErr
// 来自循环的末次 err，不会是 nil（err!=nil 才 break）；保留只是防御性兜底。
func isCleanStreamEnd(err error) bool {
	return errors.Is(err, io.EOF)
}

// sseDoneTracker 只保留 marker 长度所需的尾部，避免大事件被 256 字节窗口截断。
// feed 也覆盖 marker 恰好跨 chunk 的情况。
type sseDoneTracker struct {
	sawData bool
	sawDone bool
	tail    []byte
}

func newSSEDoneTracker() *sseDoneTracker { return &sseDoneTracker{} }

func (t *sseDoneTracker) feed(chunk []byte) {
	if len(chunk) == 0 || t.sawDone && t.sawData {
		return
	}
	combined := append(append([]byte(nil), t.tail...), chunk...)
	if !t.sawData {
		t.sawData = bytes.Contains(combined, []byte("data:"))
	}
	if !t.sawDone {
		// 只匹配 SSE 协议里 data 字段的标准结尾格式 "data: [DONE]" 或 "data:[DONE]"，
		// 避免响应 JSON 文本里恰好含 "[DONE]" 字样时抑制了真正的收尾哨兵。
		t.sawDone = doneLinePattern.Match(combined)
	}
	const keep = len("data: [DONE]") - 1 // 跨 chunk 时保留足够字节覆盖整个收尾行
	if len(combined) > keep {
		t.tail = append(t.tail[:0], combined[len(combined)-keep:]...)
	} else {
		t.tail = append(t.tail[:0], combined...)
	}
}

func (t *sseDoneTracker) finish(w http.ResponseWriter) {
	if !t.sawData || t.sawDone {
		return
	}
	w.Write([]byte("data: [DONE]\n\n"))
	http.NewResponseController(w).Flush()
}
