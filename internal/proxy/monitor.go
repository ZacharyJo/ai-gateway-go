package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 会话标识请求头，任一命中即作为 per-source 聚合 key。
var sessionHeaderKeys = []string{
	"Session-Id", "Thread-Id", "X-Client-Request-Id",
	"Session_Id", "Thread_Id",
}

// sourceKey 从请求头提取会话标识，sha256 前 12 位作为 per-source 聚合 key。
// 无会话标识返回空串（不聚合到具体 source）。
func sourceKey(r *http.Request) string {
	for _, h := range sessionHeaderKeys {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			sum := sha256.Sum256([]byte(v))
			return hex.EncodeToString(sum[:6])
		}
	}
	return ""
}

// Event 是一条监控事件。
type Event struct {
	Ts         time.Time `json:"ts"`
	Level      string    `json:"level"`
	Name       string    `json:"name"`
	ReqID      int64     `json:"req"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	Status     int       `json:"status,omitempty"`
	Attempts   int       `json:"try,omitempty"`
	MaxAttempt int       `json:"maxTry,omitempty"`
	DurMs      int64     `json:"durMs,omitempty"`
	Source     string    `json:"source,omitempty"`
}

// SourceStat 是按会话聚合的统计。
type SourceStat struct {
	Requests   int64
	Responses  int64
	Retries429 int64
	Errors     int64
	Non200     int64
	TotalDurMs int64
	MaxDurMs   int64
	LastSeenTs int64
}

// Monitor 是进程内监控。
// 维护最近事件环 + 全局计数 + 按会话（sourceKey）聚合；全部在内存，进程重启即清零。
type Monitor struct {
	mu        sync.Mutex
	maxEvents int
	events    []Event // 最近事件（新的追加在尾部）
	sources   map[string]*SourceStat

	requests        int64
	responses       int64
	retriesUpstream int64
	retries429      int64
	non200Total     int64
	non200ByStatus  map[int]int64
	errors          int64
	timeline        []timelinePoint // 分钟吞吐桶，按 Ts 递增，保留最近 timelineMinutes 条
	started         time.Time
}

// maxTrackedSources 是 per-source 统计的条数上限（超出淘汰最久未见的那条）。
const maxTrackedSources = 200

// timelineMinutes 是吞吐图保留的分钟桶数量（最近 60 分钟）。
const timelineMinutes = 60

// timelinePoint 是一分钟内的请求计数（吞吐图数据，仅进程内存，重启清零）。
type timelinePoint struct {
	Ts   int64 // 分钟起点（unix 秒）
	Reqs int64 // request_received
	Resp int64 // request_finish
	Errs int64 // 错误事件
}

// NewMonitor 构造监控器。maxEvents 是最近事件环上限。
func NewMonitor(maxEvents int) *Monitor {
	if maxEvents <= 0 {
		maxEvents = 300
	}
	return &Monitor{
		maxEvents:      maxEvents,
		sources:        map[string]*SourceStat{},
		non200ByStatus: map[int]int64{},
		started:        time.Now(),
	}
}

// evictOldestSource 淘汰 LastSeenTs 最小（最久未见）的一条 source 统计。调用方需持锁。
func (m *Monitor) evictOldestSource() {
	oldestKey := ""
	var oldestTs int64
	for k, v := range m.sources {
		if oldestKey == "" || v.LastSeenTs < oldestTs {
			oldestKey, oldestTs = k, v.LastSeenTs
		}
	}
	if oldestKey != "" {
		delete(m.sources, oldestKey)
	}
}

// touchTimeline 把吞吐时间线推进到 t 所在分钟。跨分钟/跨时段自动补空桶，
// 保持 x 轴连续（时段内没有事件时图表显示 0 而非断裂）。调用方需持锁。
func (m *Monitor) touchTimeline(t time.Time) {
	mint := t.Truncate(time.Minute).Unix()
	if len(m.timeline) == 0 {
		m.appendTimeline(mint)
		return
	}
	last := m.timeline[len(m.timeline)-1].Ts
	if mint <= last {
		return
	}
	for next := last + 60; next <= mint; next += 60 {
		m.appendTimeline(next)
	}
}

// appendTimeline 追加一个分钟桶，超出 timelineMinutes 时丢弃最旧的。调用方需持锁。
func (m *Monitor) appendTimeline(ts int64) {
	m.timeline = append(m.timeline, timelinePoint{Ts: ts})
	if len(m.timeline) > timelineMinutes {
		m.timeline = m.timeline[len(m.timeline)-timelineMinutes:]
	}
}

// Record 记录一条事件并更新计数。
func (m *Monitor) Record(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	isError := strings.Contains(e.Name, "failed") || strings.Contains(e.Name, "error") || e.Level == "error"
	// 非 200 只认**终态**（request_finish）的状态码：upstream_retry 携带的是中间态的
	// 429/5xx，一个"先 429 后重试成功"的请求若也计入，就会被算成 1 响应 + 1 非200，
	// 仪表盘成功率被打到 0%、状态码分布还会出现根本没交付给客户端的假 429。
	isNon200 := e.Name == "request_finish" && e.Status != 0 && e.Status != 200 && e.Status != 204

	// 吞吐分钟桶：推进到事件所在分钟并累加对应计数
	m.touchTimeline(e.Ts)
	if len(m.timeline) > 0 {
		p := &m.timeline[len(m.timeline)-1]
		switch e.Name {
		case "request_received":
			p.Reqs++
		case "request_finish":
			p.Resp++
		}
		if isError {
			p.Errs++
		}
	}

	switch e.Name {
	case "request_received":
		m.requests++
	case "request_finish":
		m.responses++
	case "upstream_retry":
		m.retriesUpstream++
		if e.Status == http.StatusTooManyRequests {
			m.retries429++
		}
	}
	if isNon200 {
		m.non200Total++
		m.non200ByStatus[e.Status]++
	}
	if isError {
		m.errors++
	}

	if e.Source != "" {
		s := m.sources[e.Source]
		if s == nil {
			// 长驻进程里每个新会话都会建一条，必须设上限：否则一天下来上万条永不释放，
			// 且 /api/stats 每次轮询都要全量导出+排序（持锁，会阻塞请求路径的 Record）。
			if len(m.sources) >= maxTrackedSources {
				m.evictOldestSource()
			}
			s = &SourceStat{}
			m.sources[e.Source] = s
		}
		s.LastSeenTs = e.Ts.UnixMilli()
		switch e.Name {
		case "request_received":
			s.Requests++
		case "request_finish":
			s.Responses++
			s.TotalDurMs += e.DurMs
			if e.DurMs > s.MaxDurMs {
				s.MaxDurMs = e.DurMs
			}
		case "upstream_retry":
			if e.Status == http.StatusTooManyRequests {
				s.Retries429++
			}
		}
		if isError {
			s.Errors++
		}
		if isNon200 {
			s.Non200++
		}
	}

	if len(m.events) >= m.maxEvents {
		m.events = append(m.events[:0], m.events[1:]...)
	}
	m.events = append(m.events, e)
}

// Stats 是 /api/stats 的返回结构。
type Stats struct {
	OK              bool           `json:"ok"`
	Now             string         `json:"now"`
	UpstreamBase    string         `json:"upstreamBase"`
	UptimeSec       int64          `json:"uptimeSec"`
	Requests        int64          `json:"requests"`
	Responses       int64          `json:"responses"`
	RetriesUpstream int64          `json:"retriesUpstream"`
	Retries429      int64          `json:"retries429"`
	Errors          int64          `json:"errors"`
	Non200          Non200Stats    `json:"non200"`
	RecentEvents    []Event        `json:"recentEvents"`
	Sources         []SourceView   `json:"sources"`
	Timeline        []TimelineView `json:"timeline"`
}

// Non200Stats 是错误状态码统计。
type Non200Stats struct {
	Total    int64            `json:"total"`
	ByStatus map[string]int64 `json:"byStatus"`
}

// SourceView 是导出给 /api/stats 的 per-source 视图。
type SourceView struct {
	Source     string `json:"source"`
	Requests   int64  `json:"requests"`
	Responses  int64  `json:"responses"`
	Retries429 int64  `json:"retries429"`
	Errors     int64  `json:"errors"`
	Non200     int64  `json:"non200"`
	AvgDurMs   int64  `json:"avgDurMs"`
	MaxDurMs   int64  `json:"maxDurMs"`
	LastSeenMs int64  `json:"lastSeenMs"`
}

// TimelineView 是导出给 /api/stats 的分钟吞吐视图。
type TimelineView struct {
	T int64 `json:"t"` // 分钟起点（unix 秒）
	R int64 `json:"r"` // 请求数
	S int64 `json:"s"` // 完成数
	E int64 `json:"e"` // 错误数
}

// Snapshot 生成 /api/stats 的 JSON 视图（对应 monitor.dashboardStats）。
func (m *Monitor) Snapshot(upstreamBase string) *Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Stats{
		OK:              true,
		Now:             time.Now().Format("2006-01-02 15:04:05"),
		UpstreamBase:    upstreamBase,
		UptimeSec:       int64(time.Since(m.started).Seconds()),
		Requests:        m.requests,
		Responses:       m.responses,
		RetriesUpstream: m.retriesUpstream,
		Retries429:      m.retries429,
		Errors:          m.errors,
		Non200:          Non200Stats{Total: m.non200Total, ByStatus: map[string]int64{}},
		RecentEvents:    []Event{},
		Sources:         []SourceView{},
		Timeline:        []TimelineView{},
	}
	for k, v := range m.non200ByStatus {
		s.Non200.ByStatus[strconv.Itoa(k)] = v
	}
	// 最近事件：新的在前（倒序）
	for i := len(m.events) - 1; i >= 0; i-- {
		s.RecentEvents = append(s.RecentEvents, m.events[i])
	}
	// 按 LastSeenTs 降序导出 sources
	for k, v := range m.sources {
		avg := int64(0)
		if v.Responses > 0 {
			avg = v.TotalDurMs / v.Responses
		}
		s.Sources = append(s.Sources, SourceView{
			Source: k, Requests: v.Requests, Responses: v.Responses,
			Retries429: v.Retries429, Errors: v.Errors, Non200: v.Non200,
			AvgDurMs: avg, MaxDurMs: v.MaxDurMs, LastSeenMs: v.LastSeenTs,
		})
	}
	// 按最近活跃降序
	sort.Slice(s.Sources, func(i, j int) bool { return s.Sources[i].LastSeenMs > s.Sources[j].LastSeenMs })
	// 分钟吞吐（已按 Ts 递增，无需再排）
	for _, p := range m.timeline {
		s.Timeline = append(s.Timeline, TimelineView{T: p.Ts, R: p.Reqs, S: p.Resp, E: p.Errs})
	}
	return s
}
