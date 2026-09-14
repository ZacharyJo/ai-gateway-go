package proxy

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestMonitorCounters(t *testing.T) {
	m := NewMonitor(10)
	m.Record(Event{Name: "request_received", ReqID: 1, Method: "POST", Path: "/v1/responses"})
	m.Record(Event{Name: "upstream_retry", ReqID: 1, Method: "POST", Path: "/v1/responses", Status: 429, Attempts: 1})
	m.Record(Event{Name: "upstream_retry", ReqID: 1, Method: "POST", Path: "/v1/responses", Status: 500, Attempts: 2})
	m.Record(Event{Name: "request_finish", ReqID: 1, Method: "POST", Path: "/v1/responses", Status: 200, DurMs: 50})
	m.Record(Event{Name: "request_received", ReqID: 2, Method: "POST", Path: "/v1/x"})
	m.Record(Event{Name: "upstream_fetch_failed", Level: "error", ReqID: 2, Method: "POST", Path: "/v1/x"})
	m.Record(Event{Name: "request_finish", ReqID: 2, Method: "POST", Path: "/v1/x", Status: 503, DurMs: 10})

	s := m.Snapshot("")
	if s.Requests != 2 {
		t.Errorf("Requests = %d, want 2", s.Requests)
	}
	if s.Responses != 2 {
		t.Errorf("Responses = %d, want 2", s.Responses)
	}
	if s.RetriesUpstream != 2 {
		t.Errorf("RetriesUpstream = %d, want 2", s.RetriesUpstream)
	}
	if s.Retries429 != 1 {
		t.Errorf("Retries429 = %d, want 1", s.Retries429)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (upstream_fetch_failed)", s.Errors)
	}
	// 非 200 只认终态：req=1 先 429 再 500 最后 200 成功，不该留下任何非200 记录；
	// req=2 终态 503 才算一条
	if s.Non200.Total != 1 {
		t.Errorf("Non200.Total = %d, want 1（只有 req=2 的终态 503）", s.Non200.Total)
	}
	if s.Non200.ByStatus["503"] != 1 {
		t.Errorf("Non200.ByStatus = %v, want {503:1}", s.Non200.ByStatus)
	}
	if s.Non200.ByStatus["429"] != 0 || s.Non200.ByStatus["500"] != 0 {
		t.Errorf("重试中间态被计入状态码分布: %v", s.Non200.ByStatus)
	}
	// 最近事件倒序：最后记录的在最前
	if len(s.RecentEvents) != 7 {
		t.Errorf("RecentEvents len = %d, want 7", len(s.RecentEvents))
	}
	if s.RecentEvents[0].Name != "request_finish" {
		t.Errorf("first recent event = %q, want request_finish (newest first)", s.RecentEvents[0].Name)
	}
}

func TestMonitorSourceAggregation(t *testing.T) {
	m := NewMonitor(10)
	src := "abc123"
	m.Record(Event{Name: "request_received", ReqID: 1, Source: src})
	m.Record(Event{Name: "upstream_retry", ReqID: 1, Status: 429, Source: src})
	m.Record(Event{Name: "request_finish", ReqID: 1, Status: 200, DurMs: 100, Source: src})
	m.Record(Event{Name: "request_received", ReqID: 2, Source: src})
	m.Record(Event{Name: "request_finish", ReqID: 2, Status: 500, DurMs: 300, Source: src})
	m.Record(Event{Name: "request_finish", ReqID: 3, Status: 200, DurMs: 100, Source: "other"})

	s := m.Snapshot("")
	if len(s.Sources) != 2 {
		t.Fatalf("Sources len = %d, want 2", len(s.Sources))
	}
	var a, other *SourceView
	for i := range s.Sources {
		switch s.Sources[i].Source {
		case src:
			a = &s.Sources[i]
		case "other":
			other = &s.Sources[i]
		}
	}
	if a == nil {
		t.Fatal("source not found")
	}
	if a.Requests != 2 || a.Responses != 2 {
		t.Errorf("source requests/responses = %d/%d, want 2/2", a.Requests, a.Responses)
	}
	if a.Retries429 != 1 {
		t.Errorf("source Retries429 = %d, want 1", a.Retries429)
	}
	// 非 200 只认终态：429 是重试中间态，不计入；只有第二个请求的终态 500 算一条
	if a.Non200 != 1 {
		t.Errorf("source Non200 = %d, want 1（只有终态 500）", a.Non200)
	}
	if a.AvgDurMs != 200 { // (100+300)/2
		t.Errorf("source AvgDurMs = %d, want 200", a.AvgDurMs)
	}
	if a.MaxDurMs != 300 {
		t.Errorf("source MaxDurMs = %d, want 300", a.MaxDurMs)
	}
	if other == nil || other.Responses != 1 {
		t.Errorf("other source = %+v, want 1 response", other)
	}
	// 按 LastSeenTs 降序
	if s.Sources[0].LastSeenMs < s.Sources[1].LastSeenMs {
		t.Error("sources not sorted by LastSeenTs desc")
	}
}

func TestMonitorRingCap(t *testing.T) {
	m := NewMonitor(3)
	for i := 0; i < 10; i++ {
		m.Record(Event{Name: "request_received", ReqID: int64(i)})
	}
	s := m.Snapshot("")
	if len(s.RecentEvents) != 3 {
		t.Errorf("RecentEvents len = %d, want 3 (ring cap)", len(s.RecentEvents))
	}
	// 最新 3 条保留（9,8,7 倒序）
	if s.RecentEvents[0].ReqID != 9 || s.RecentEvents[2].ReqID != 7 {
		t.Errorf("ring contents = %v, want [9,8,7]", s.RecentEvents)
	}
}

func TestSourceKey(t *testing.T) {
	// 无会话标识 → 空
	if k := sourceKey(httptest.NewRequest("POST", "/v1/responses", nil)); k != "" {
		t.Errorf("no session = %q, want empty", k)
	}
	// Session-Id 命中 → 12 位 hex
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("Session-Id", "session-1")
	k1 := sourceKey(r)
	if len(k1) != 12 {
		t.Errorf("sourceKey len = %d, want 12 (sha256 前 12 位)", len(k1))
	}
	// 相同会话 → 相同 key；不同会话 → 不同 key
	r2 := httptest.NewRequest("POST", "/v1/responses", nil)
	r2.Header.Set("Session-Id", "session-2")
	if k1 == sourceKey(r2) {
		t.Error("different sessions produced same source key")
	}
	// Thread-Id 兜底
	r3 := httptest.NewRequest("POST", "/v1/responses", nil)
	r3.Header.Set("Thread-Id", "thread-1")
	if k := sourceKey(r3); len(k) != 12 {
		t.Errorf("Thread-Id sourceKey len = %d, want 12", len(k))
	}
}

func TestMonitorSourceEviction(t *testing.T) {
	m := NewMonitor(10)
	// 灌入超过上限的会话；每条 LastSeenTs 递增，最旧的应被淘汰
	base := time.Now()
	for i := 0; i < maxTrackedSources+50; i++ {
		m.Record(Event{
			Name: "request_finish", ReqID: int64(i), Status: 200,
			Source: "src-" + itoa(i), Ts: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	s := m.Snapshot("")
	if len(s.Sources) > maxTrackedSources {
		t.Errorf("sources = %d, want <= %d (evicted)", len(s.Sources), maxTrackedSources)
	}
	// 最新的会话必须还在
	newest := "src-" + itoa(maxTrackedSources+49)
	found := false
	for _, v := range s.Sources {
		if v.Source == newest {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("newest source %q was evicted", newest)
	}
	// 最旧的应已被淘汰
	for _, v := range s.Sources {
		if v.Source == "src-0" {
			t.Error("oldest source src-0 should have been evicted")
		}
	}
}

func TestMonitorSnapshotNowFormat(t *testing.T) {
	m := NewMonitor(5)
	s := m.Snapshot("http://upstream")
	if !s.OK {
		t.Error("OK = false, want true")
	}
	if s.UpstreamBase != "http://upstream" {
		t.Errorf("UpstreamBase = %q", s.UpstreamBase)
	}
	if _, err := time.Parse("2006-01-02 15:04:05", s.Now); err != nil {
		t.Errorf("Now not formatted as local time: %q", s.Now)
	}
}

func TestMonitorTimeline(t *testing.T) {
	m := NewMonitor(10)
	now := time.Now().Truncate(time.Minute)
	// 同一分钟内：2 请求、1 完成、1 错误
	m.Record(Event{Name: "request_received", ReqID: 1, Ts: now})
	m.Record(Event{Name: "request_received", ReqID: 2, Ts: now})
	m.Record(Event{Name: "request_finish", ReqID: 1, Status: 200, Ts: now})
	m.Record(Event{Name: "upstream_fetch_failed", Level: "error", ReqID: 2, Ts: now})
	// 跳到 2 分钟后的完成事件（中间 1 分钟应补空桶，保持 x 轴连续）
	m.Record(Event{Name: "request_finish", ReqID: 2, Status: 200, Ts: now.Add(2 * time.Minute)})

	s := m.Snapshot("")
	if len(s.Timeline) != 3 {
		t.Fatalf("timeline len = %d, want 3 (gap filled)", len(s.Timeline))
	}
	b0 := s.Timeline[0]
	if b0.R != 2 || b0.S != 1 || b0.E != 1 {
		t.Errorf("bucket0 = %+v, want {r:2,s:1,e:1}", b0)
	}
	if b1 := s.Timeline[1]; b1.R != 0 || b1.S != 0 || b1.E != 0 {
		t.Errorf("bucket1 (gap) = %+v, want zeros", b1)
	}
	if b2 := s.Timeline[2]; b2.S != 1 {
		t.Errorf("bucket2 = %+v, want s:1", b2)
	}
	if s.Timeline[2].T-s.Timeline[0].T != 120 {
		t.Errorf("timeline span = %d, want 120s", s.Timeline[2].T-s.Timeline[0].T)
	}
}

func TestMonitorTimelineRingCap(t *testing.T) {
	m := NewMonitor(10)
	now := time.Now().Truncate(time.Minute)
	// 灌入超过上限的分钟桶
	for i := 0; i < timelineMinutes+20; i++ {
		m.Record(Event{Name: "request_received", ReqID: int64(i), Ts: now.Add(time.Duration(i) * time.Minute)})
	}
	s := m.Snapshot("")
	if len(s.Timeline) > timelineMinutes {
		t.Errorf("timeline len = %d, want <= %d", len(s.Timeline), timelineMinutes)
	}
	// 最新的分钟桶必须保留（末尾 Ts 对应最后一分钟）
	last := s.Timeline[len(s.Timeline)-1]
	if last.T != now.Add(time.Duration(timelineMinutes+19)*time.Minute).Truncate(time.Minute).Unix() {
		t.Errorf("last bucket Ts = %d, want newest minute", last.T)
	}
}

func TestMonitorRetrySuccessKeepsCleanSuccessRate(t *testing.T) {
	// A1 回归：一个"先 429 后重试成功"的请求，仪表盘成功率应为 100%。
	// 此前 upstream_retry 携带的中间态 429 被计入 non200，成功率（响应-非200）/响应
	// 会被打到 0%，状态码分布里还会出现根本没交付给客户端的假 429。
	m := NewMonitor(10)
	m.Record(Event{Name: "request_received", ReqID: 1, Source: "s1"})
	m.Record(Event{Name: "upstream_retry", ReqID: 1, Status: 429, Attempts: 1, Source: "s1"})
	m.Record(Event{Name: "request_finish", ReqID: 1, Status: 200, DurMs: 80, Source: "s1"})

	s := m.Snapshot("")
	if s.Responses != 1 {
		t.Fatalf("Responses = %d, want 1", s.Responses)
	}
	if s.Non200.Total != 0 || len(s.Non200.ByStatus) != 0 {
		t.Errorf("非200 = %d %v, want 0（重试中间态不该计入）", s.Non200.Total, s.Non200.ByStatus)
	}
	// 429 重试次数仍然要统计到（用于观察限流压力），只是不算作非200
	if s.Retries429 != 1 || s.RetriesUpstream != 1 {
		t.Errorf("重试计数 = %d/%d, want 1/1", s.Retries429, s.RetriesUpstream)
	}
	// 仪表盘成功率算法：(responses - non200) / responses
	if ok := s.Responses - s.Non200.Total; ok != 1 {
		t.Errorf("成功数 = %d, want 1（成功率应为 100%%）", ok)
	}
	if len(s.Sources) != 1 || s.Sources[0].Non200 != 0 {
		t.Errorf("会话聚合的非200 = %+v, want 0", s.Sources)
	}
}
