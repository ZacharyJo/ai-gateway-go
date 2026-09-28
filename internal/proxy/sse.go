package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// doneLinePattern 匹配 SSE 的 [DONE] 收尾行（data: [DONE] 或 data:[DONE]），
// 用于区分协议哨兵与响应 JSON 文本里恰好含 [DONE] 字样的情况。
var doneLinePattern = regexp.MustCompile(`(?m)^data:\s*\[DONE\]`)

// isSSE 判断上游响应是否为 SSE 流。
func isSSE(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// primeMaxEmptyReads 是预检阶段连续空读（Read 返回 (0,nil)）的次数上限。
// (0,nil) 本身合法，但病理 reader（包装的 idle-timeout / gzip / MultiReader）可能
// 反复空读而不阻塞——超过上限即 fail-open 交回正常交付，避免首包预检忙旋挂死请求。
const primeMaxEmptyReads = 100

// primeSSEEvent 读取上游 SSE 流的第一个完整事件（data 内容），用于交付前预检。
// 返回 (首帧 data、是否已读到完整事件、已读原始字节、error)。
// 只有“已读到完整事件”才算预检通过；上游迟迟不吐数据、或流在首个完整事件前结束，
// 都返回 err（调用方可据此重试）。一旦读到完整事件，即使它是错误帧，也已读的字节
// 必须由调用方回接，不能丢弃。
func primeSSEEvent(r io.Reader, maxBytes int) (data string, complete bool, buffered []byte, err error) {
	buf := make([]byte, 32*1024)
	var pending []byte
	var sawEOF bool
	emptyReads := 0
	for len(pending) < maxBytes {
		n, readErr := r.Read(buf)
		if n > 0 {
			emptyReads = 0
			pending = append(pending, buf[:n]...)
			if end := lastSSEBoundary(string(pending)); end > 0 {
				first := pending[:end]
				frames := parseSSEFrames(string(first))
				if len(frames) > 0 {
					return frames[0].data, true, pending, nil
				}
			}
		} else if readErr == nil {
			// (0,nil) 空读：计数达到上限即 fail-open，交回正常交付（不重试、不挂死）。
			emptyReads++
			if emptyReads >= primeMaxEmptyReads {
				return "", false, pending, nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				sawEOF = true
				break
			}
			// 非 EOF：无论是否已有部分数据，都视为“首包前失败”。
			return "", false, pending, readErr
		}
	}
	// 达到 maxBytes 但没读到完整事件：fail-open，由调用方把已读字节接回正常交付。
	if !sawEOF {
		return "", false, pending, nil
	}
	// 流在首个完整事件前干净结束：视为失败，调用方可重试。
	return "", false, pending, io.EOF
}

// isFailureSSEData 判断一个完整 SSE 事件的 data 载荷是否为失败终态。
// 覆盖 Chat/Responses 的 error、response.failed 等形态；首帧即失败时应整体重打。
func isFailureSSEData(data string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return false
	}
	var doc map[string]any
	if json.Unmarshal([]byte(trimmed), &doc) != nil {
		return false
	}
	// 只认「非 nil 的 error」为失败：部分网关每个 chunk 都回显 "error":null，
	// 按键存在判定会把健康首帧误判成失败终态。
	if errVal, has := doc["error"]; has && errVal != nil {
		return true
	}
	if t := stringifyAny(doc["type"]); t == "response.failed" || t == "response.incomplete" {
		return true
	}
	return false
}

// sseEventTransformer 是 SSE 增量变换器的通用接口（toolShapeRepairer /
// imageBridgeSSETransformer / MessagesSSETransformer 都实现它）。Push 返回可下发的文本
// （可能为空=帧被吸收），Flush 处理流末尾残留。
type sseEventTransformer interface {
	Push(chunk string) string
	Flush() string
}

// protocolIncompleteReporter 由能判断"上游是否缺了协议终止事件"的转换器实现
// （Chat 看 finish_reason，Messages 看 message_stop）。适配路径据此把截断判定从 TCP 层
// （读错误）扩展到协议层：干净 EOF 但上游没正常收尾时，客户端拿到的仍是不完整轮次，
// 监控不该记成功。未实现该接口的转换器按"无法判定"处理，不额外上报截断。
type protocolIncompleteReporter interface {
	ProtocolIncomplete() bool
}

// protocolIncomplete 在转换器能判定时返回"上游是否缺了协议终止事件"，否则 false。
func protocolIncomplete(t sseEventTransformer) bool {
	if r, ok := t.(protocolIncompleteReporter); ok {
		return r.ProtocolIncomplete()
	}
	return false
}

// terminalFailureReporter 由能判断"本轮已下发失败终态"的转换器实现。
// 这类轮次协议上正常终止（收到 message_stop / finish_reason），但代理合成了
// response.failed（如 upstream_tool_call_dropped / stream_truncated）——客户端看到失败，
// 监控不应记成 200 成功。
type terminalFailureReporter interface {
	TerminalFailed() bool
}

// terminalFailed 在转换器能判定时返回"本轮是否已下发失败终态"，否则 false。
func terminalFailed(t sseEventTransformer) bool {
	if r, ok := t.(terminalFailureReporter); ok {
		return r.TerminalFailed()
	}
	return false
}

// thinkingBufferReporter 由会缓冲未闭合 think 块（对下游零输出）的转换器实现。
// 流循环据此在缓冲期发保活注释帧，防客户端把长思考误判为断流。
type thinkingBufferReporter interface {
	BufferingThink() bool
}

// bufferingThink 在转换器能判定时返回"当前是否处于 think 缓冲期"，否则 false。
func bufferingThink(t sseEventTransformer) bool {
	if r, ok := t.(thinkingBufferReporter); ok {
		return r.BufferingThink()
	}
	return false
}

// chainedSSETransformer 顺序组合两个变换器：第二个消费第一个的输出。
// 适配路径开 bridge 桥接时用：MessagesSSETransformer 先转 Responses，再喂给
// imageBridgeSSETransformer 执行 bridge_imagegen。
type chainedSSETransformer struct {
	first  sseEventTransformer
	second sseEventTransformer
}

func (c *chainedSSETransformer) Push(chunk string) string {
	return c.second.Push(c.first.Push(chunk))
}

func (c *chainedSSETransformer) Flush() string {
	var out strings.Builder
	out.WriteString(c.second.Push(c.first.Flush()))
	out.WriteString(c.second.Flush())
	return out.String()
}

// ProtocolIncomplete 委托链首（Messages 转换器）：链尾的图片桥接不关心协议终止事件。
func (c *chainedSSETransformer) ProtocolIncomplete() bool { return protocolIncomplete(c.first) }

// TerminalFailed 委托链首（Messages 转换器）：链尾的图片桥接不产生失败终态。
func (c *chainedSSETransformer) TerminalFailed() bool { return terminalFailed(c.first) }

// BufferingThink 委托链首（Messages 转换器）：链尾的图片桥接不缓冲 think。
func (c *chainedSSETransformer) BufferingThink() bool { return bufferingThink(c.first) }

// streamSSEUsage 把上游 SSE 逐块写回客户端并刷新（保证首字低延迟），同时嗅探
// GPT 透传流里的 usage（response.completed 帧）。
// doneNet=true 时做 [DONE] 兜底：OpenAI 风格 SSE 若没以 data: [DONE] 收尾则补一帧，防客户端卡住。
// 原生 Anthropic 流（/v1/messages 透传）必须传 false —— 它以 message_stop 收尾、不认 [DONE]。
// transformer 非 nil 时把每个 chunk 先过变换器（透传路径修降级的 custom 工具），
// 写入/嗅探/软错误检测都基于变换后的文本（usage 在 completed 帧里原样保留）。
// 返回 (软错误类型, 流是否截断, inTokens, outTokens)。
// 软错误：命中时已向下游写 server_overloaded 帧并结束流，kind 为类型字符串；
// 终态错误（指纹重放冷却等）不发 server_overloaded，避免诱导客户端重试撞冷却。
// 截断：true 表示流在 EOF 之前就结束（空闲超时/RST/客户端断开），调用方可据此记非 200 终态。
// 空流（一个字节都没吐）：补 [DONE] 防客户端挂死，并标记截断（不算干净成功）。
func streamSSEUsage(w http.ResponseWriter, body io.Reader, doneNet bool, transformer sseEventTransformer) (kind string, truncated bool, inTokens, outTokens int64) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	tracker := newSSEDoneTracker()
	detector := &streamErrorDetector{}
	sniff := &responsesUsageSniffer{}
	var readErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if transformer != nil {
				repaired := transformer.Push(string(chunk))
				chunk = []byte(repaired)
			}
			if len(chunk) > 0 {
				if _, werr := w.Write(chunk); werr != nil {
					// 客户端中途断开：算截断（监控记非成功）
					return "", true, sniff.in, sniff.out
				}
				if doneNet {
					tracker.feed(chunk)
				}
				sniff.feed(chunk)
				rc.Flush()
				if k := detector.feed(chunk); k != "" {
					// 终态错误（如指纹重放冷却）不发 server_overloaded，避免诱导客户端重试撞冷却；
					// 上游原始错误帧已随内容原样转发给客户端。
					if !isTerminalStreamErrorKind(k) {
						_, _ = w.Write([]byte(overloadedErrorFrame))
						rc.Flush()
					}
					return k, false, sniff.in, sniff.out
				}
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	clean := isCleanStreamEnd(readErr)
	if transformer != nil {
		if rest := transformer.Flush(); rest != "" {
			_, _ = w.Write([]byte(rest))
			tracker.feed([]byte(rest))
			sniff.feed([]byte(rest))
			rc.Flush()
		}
	}
	if doneNet {
		// 干净结束或截断都补 [DONE]：截断时同样不能让客户端拿断尾流挂死。
		// codex 按 response.completed 判定轮次完成，[DONE] 只终止传输、不误判成功。
		tracker.finish(w)
	}
	// 空流：上游 200 SSE 一个字节都没吐（含 reasoning 缓冲阶段被空闲超时/取消掐掉的情况）。
	// 补 [DONE] 防止客户端挂死等待，并标记截断——调用方据此按失败终态记账（不计费），
	// 而不是当成干净的 200 成功（内容一个字节都没交付）。干净或截断的空流都算异常。
	if !tracker.sawData {
		if doneNet {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			rc.Flush()
		}
		return "", true, sniff.in, sniff.out
	}
	return "", !clean, sniff.in, sniff.out
}

// streamSSEAdapted 把上游 Messages SSE 增量转换成 Responses SSE 后逐块写回并刷新。
// t 是 sseEventTransformer（*MessagesSSETransformer 或 chainedSSETransformer 均可）。
// 返回 (软错误类型, 流是否截断)，语义与 streamSSEUsage 一致。
func streamSSEAdapted(w http.ResponseWriter, body io.Reader, t sseEventTransformer) (kind string, truncated bool) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	tracker := newSSEDoneTracker()
	detector := &streamErrorDetector{}
	// 保活注释帧可能与内容帧并发写回：写入经 wMu 串行化。
	var wMu sync.Mutex
	write := func(s string) bool {
		if s == "" {
			return true
		}
		wMu.Lock()
		_, err := w.Write([]byte(s))
		if err == nil {
			tracker.feed([]byte(s))
			rc.Flush()
		}
		wMu.Unlock()
		return err == nil
	}
	// 长思考静默防护：Messages 路径同样会缓冲未闭合 <thinking>（对下游零输出），
	// 期间以 SSE 注释帧保活，防客户端空闲超时断连。thinking 由主循环置位、goroutine 原子读。
	var thinking atomic.Bool
	defer sseKeepalive(w, rc, &wMu, &thinking)()
	var readErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			k := detector.feed(buf[:n])
			// 软错误优先：先写 server_overloaded 帧，再写转换后内容；
			// 反过来写的话转换器会先发 response.failed + [DONE]，客户端在 [DONE] 处停读，
			// 后续的 server_overloaded 帧永远到不了客户端。
			if k != "" {
				if !isTerminalStreamErrorKind(k) {
					write(overloadedErrorFrame)
				}
				write(t.Push(string(buf[:n])))
				return k, false
			}
			if !write(t.Push(string(buf[:n]))) {
				return "", true // 客户端断开：算截断（监控非成功）
			}
			thinking.Store(bufferingThink(t))
		}
		if err != nil {
			readErr = err
			break
		}
	}
	// 截断流也调 Flush：pending 里的完整帧（差个换行没触发解析的收尾帧）冲刷出来，
	// 不完整帧会被 parseSSEFrames 解析失败丢弃，不会把半截 JSON 发出去。
	write(t.Flush())
	// 干净结束或截断都统一收尾：有数据但上游没给 [DONE] 就补一帧，杜绝断尾流让客户端挂死。
	tracker.finish(w)
	// 空流：与透传路径一致——上游 200 SSE 一个字节都没吐时补 [DONE] 并标记截断，
	// 否则 Messages 适配路径的客户端会挂死等待、且被按干净成功记账。干净或截断的空流都算异常。
	if !tracker.sawData {
		write("data: [DONE]\n\n")
		return "", true
	}
	// 截断判定：TCP 层读错误（RST/空闲超时）之外，上游没发协议终止事件（message_stop）
	// 同样算截断——干净 EOF 只是 HTTP 响应正常终止，不代表这一轮正常收尾。已下发失败终态
	// （如 upstream_tool_call_dropped）的轮次同样不算成功。
	return "", !isCleanStreamEnd(readErr) || protocolIncomplete(t) || terminalFailed(t)
}

// chatErrorFrame 从 Chat 错误 chunk 的 data 行里抽出 error 对象，包成 Responses 的
// event: error 帧；解析不出具体错误时给通用失败帧（Chat 适配路径跨协议转换用）。
func chatErrorFrame(chunk string) string {
	for _, line := range splitLines(chunk) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(data), &doc) != nil {
			continue
		}
		if errObj, ok := doc["error"]; ok {
			return sseFrame("error", map[string]any{"type": "error", "error": errObj})
		}
	}
	return sseFrame("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "server_error", "message": "upstream terminal error"},
	})
}

// streamSSEChat 把上游 Chat Completions SSE 增量转换成 Responses SSE 后逐块写回并刷新。
// 返回 (软错误类型, 流是否截断)，语义与 streamSSEUsage 一致。
func streamSSEChat(w http.ResponseWriter, body io.Reader, t *ChatSSETransformer) (kind string, truncated bool) {
	return streamSSEChatChained(w, body, t, nil)
}

// streamSSEChatChained 同 streamSSEChat，但可在 Chat 转换之后链一个桥接变换器
// （BRIDGE_IMAGEGEN_ENABLED 时执行 bridge_imagegen）。second 为 nil 时等价 streamSSEChat。
// Chat 变换器按 data 行消费并产出 Responses SSE 帧，桥接变换器再对这些帧逐块处理，
// 与 Messages 适配路径的 chainedSSETransformer 语义一致。
func streamSSEChatChained(w http.ResponseWriter, body io.Reader, t *ChatSSETransformer, second sseEventTransformer) (kind string, truncated bool) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	detector := &streamErrorDetector{}
	// 保活注释帧可能插在内容帧之间写回：并发写 http.ResponseWriter 需串行化。
	var wMu sync.Mutex
	write := func(s string) bool {
		if s == "" {
			return true
		}
		wMu.Lock()
		_, err := w.Write([]byte(s))
		if err == nil {
			rc.Flush()
		}
		wMu.Unlock()
		return err == nil
	}
	// 长思考静默防护：内嵌 <thinking> 缓冲期转换器对下游零输出，客户端若按流空闲计时
	// 会把长思考误判为断流。期间以 SSE 注释帧保活；thinking 由主循环置位、goroutine 原子读。
	var thinking atomic.Bool
	defer sseKeepalive(w, rc, &wMu, &thinking)()

	pending := ""
	var readErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			k := detector.feed(buf[:n])
			if k != "" {
				if !isTerminalStreamErrorKind(k) {
					// 可重试软错误：发 server_overloaded 帧让客户端重试，并以 [DONE] 干净收尾。
					// **不再原样透传原始 Chat chunk**——那是 Chat Completions JSON，混进
					// Responses 流会协议污染；客户端拿到 server_overloaded 即重试，
					// 不需要看底层错误正文。
					write(overloadedErrorFrame)
					write("data: [DONE]\n\n")
					return k, false
				}
				// 终态错误（如指纹重放冷却）：把上游 Chat 错误转成 Responses 的 event: error 帧
				// + [DONE] 干净收尾——不跨协议裸透传 Chat JSON；客户端拿到 error 帧按失败处理。
				write(chatErrorFrame(string(buf[:n])))
				write("data: [DONE]\n\n")
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
						if second != nil {
							out = second.Push(out)
						}
						if !write(out) {
							return "", true // 客户端断开：算截断（监控非成功）
						}
					}
					thinking.Store(t.BufferingThink())
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
			out := t.Push(data)
			if second != nil {
				out = second.Push(out)
			}
			write(out)
			thinking.Store(t.BufferingThink())
		}
	}
	// 干净结束或截断都冲刷转换器收尾：Flush 兜底未闭合 <think> 为正文、finalize reasoning、
	// 补发 response.completed + [DONE]。截断时同样必须收尾——否则 codex 拿到一条没有终止帧的
	// 断尾流，轮次永远挂起（"断开"）。t.done 已置位时 Flush 幂等返回空。
	if second != nil {
		write(second.Push(t.Flush()))
		write(second.Flush())
	} else {
		write(t.Flush())
	}
	// 截断判定：TCP 层读错误之外，上游没发 finish_reason 同样算截断——干净 EOF 只是
	// HTTP 响应正常终止，不代表这一轮正常收尾。已下发失败终态（如 upstream_tool_call_dropped）
	// 的轮次同样不算成功。
	return "", !isCleanStreamEnd(readErr) || t.ProtocolIncomplete() || t.TerminalFailed()
}

// keepaliveInterval 是长思考静默期的保活注释帧间隔：远小于常见流空闲超时（60s+），
// 又足够低频避免思考期间刷出大量空帧。测试可临时改小以触发。
var keepaliveInterval = 15 * time.Second

// keepaliveFrame 是 SSE 注释帧（冒号开头，协议忽略），只用于维持连接活跃、防静默超时断连。
const keepaliveFrame = ": keepalive\n\n"

// sseKeepalive 在 think 缓冲期（转换器对下游零输出）以 SSE 注释帧保活，防客户端把长思考
// 误判为断流。thinking 由主循环置位、goroutine 只原子读；写入经 wMu 与内容帧串行化。
// 返回的 stop 函数须 defer 调用：它在主循环返回前停掉 goroutine 并等其退出，
// 避免对已归还的连接写残留字节。
func sseKeepalive(w http.ResponseWriter, rc *http.ResponseController, wMu *sync.Mutex, thinking *atomic.Bool) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !thinking.Load() {
					continue
				}
				wMu.Lock()
				_, err := w.Write([]byte(keepaliveFrame))
				if err == nil {
					rc.Flush()
				}
				wMu.Unlock()
				if err != nil {
					return // 客户端已断开：停止保活
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
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

// responsesUsageSniffer 从 GPT 透传的 Responses SSE 流里嗅探 usage（不缓冲整流）。
// Responses 协议在流末尾发 `event: response.completed`，其 data 里 response.usage 带
// input_tokens/output_tokens。usage 帧通常单独成行且不大，这里维护一个尾部滑窗累积可能
// 跨 chunk 的 completed 帧文本，解析出的最新 usage 覆盖旧值（最后一帧为准）。
type responsesUsageSniffer struct {
	tail  []byte
	carry []byte // completed 标记出现前，只留极小重叠尾部检测跨界，避免每 chunk 扫全窗
	seen  bool   // 是否已见 response.completed 标记（之后才累积并解析）
	in    int64
	out   int64
}

// usageSniffWindow 是尾部滑窗上限：completed 帧的 response 对象含少量元数据，
// 64KB 足以覆盖跨 chunk 的单帧，又不至于把整流缓冲进内存。
const usageSniffWindow = 64 * 1024

// usageMarker 是 Responses 完成帧的标志串，usage 就在其所在 completed 帧里。
const usageMarker = "response.completed"

func (s *responsesUsageSniffer) feed(chunk []byte) {
	if !s.seen {
		// 未见 completed：只用 len(marker)-1 的重叠尾部拼接检测标记跨界，避免长流里每个内容
		// chunk 都对整个 64KB 窗做一次 bytes.Contains（旧实现的 O(chunks×64KB) 扫描热点）。
		probe := append(s.carry, chunk...)
		if bytes.Contains(probe, []byte(usageMarker)) {
			s.seen = true
			s.carry = nil
			s.tail = append(s.tail[:0], probe...)
			s.parse(s.tail)
			return
		}
		keep := len(usageMarker) - 1
		if len(probe) > keep {
			probe = probe[len(probe)-keep:]
		}
		s.carry = append(s.carry[:0], probe...)
		return
	}
	// 已见标记：正常累积到窗口上限并重解析（usage 可能在后续 chunk 才补全/更新，最后一帧为准）。
	combined := append(s.tail, chunk...)
	s.parse(combined)
	if len(combined) > usageSniffWindow {
		combined = combined[len(combined)-usageSniffWindow:]
	}
	s.tail = append(s.tail[:0], combined...)
}

// usageDataPattern 抓取 SSE data 行里含 "usage" 的 JSON 片段。
var usageDataPattern = regexp.MustCompile(`(?m)^data:\s*(\{.*"usage".*\})\s*$`)

func (s *responsesUsageSniffer) parse(buf []byte) {
	matches := usageDataPattern.FindAllSubmatch(buf, -1)
	for _, m := range matches {
		var doc map[string]any
		if err := json.Unmarshal(m[1], &doc); err != nil {
			continue
		}
		// Responses 完成帧结构：{"type":"response.completed","response":{...,"usage":{...}}}
		resp, ok := doc["response"].(map[string]any)
		if !ok {
			continue
		}
		usage, ok := resp["usage"].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := usage["input_tokens"].(float64); ok {
			s.in = int64(v)
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			s.out = int64(v)
		}
	}
}
