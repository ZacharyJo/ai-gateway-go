package proxy

import (
	"os"
	"path/filepath"
	"time"
)

// 落盘产物保留清理。headroom 原文与错误捕获都是"只写不删"，重度使用会累积成百 MB 的
// 本机文件内容/shell 输出（原实现也没有清理机制）。这里按修改时间过期删除：
// headroom 原文只对进行中的会话有用（/headroom-lite/<sha> 取回），过期即无价值。

// retentionDirs 是受保留策略管理的子目录（相对 LOG_DIR）。
var retentionDirs = []string{"headroom-lite-store", "ai-gateway-captures"}

// retentionSweepInterval 是清理周期。产物过期粒度是小时级，不需要频繁扫。
const retentionSweepInterval = time.Hour

// Retention 周期清理 LOG_DIR 下的过期产物。
type Retention struct {
	logDir   string
	maxAge   time.Duration
	interval time.Duration
	log      *Logger
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewRetention 构造清理器并立即扫一次，随后按周期扫。maxAge <= 0 表示不启用（返回 nil）。
func NewRetention(logDir string, maxAge time.Duration, log *Logger) *Retention {
	if logDir == "" || maxAge <= 0 {
		return nil
	}
	r := &Retention{
		logDir: logDir, maxAge: maxAge, interval: retentionSweepInterval, log: log,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	r.sweep()
	go r.loop()
	return r
}

// Close 停止周期清理。
func (r *Retention) Close() {
	if r == nil {
		return
	}
	close(r.stopCh)
	<-r.doneCh
}

func (r *Retention) loop() {
	defer close(r.doneCh)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.sweep()
		}
	}
}

// sweep 删除各产物目录里超过 maxAge 的文件。删不掉就跳过（fail-open，不影响转发）。
func (r *Retention) sweep() {
	cutoff := time.Now().Add(-r.maxAge)
	for _, sub := range retentionDirs {
		dir := filepath.Join(r.logDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录还不存在
		}
		removed := 0
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(dir, entry.Name())) == nil {
				removed++
			}
		}
		if removed > 0 && r.log != nil {
			r.log.Infof("retention_sweep dir=%s removed=%d older_than=%s", sub, removed, r.maxAge)
		}
	}
}
