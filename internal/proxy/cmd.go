package proxy

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// proxy 子命令：让二进制自管生命周期（start/stop/restart/status/logs），
// 不再依赖 make 或 launchd 包一层。
//
//	proxy            前台运行（默认）
//	proxy start      后台守护：脱离终端 + PID 文件 + 就绪校验
//	proxy stop       停止：SIGTERM 优雅退出，超时再 SIGKILL
//	proxy restart    停止后重启
//	proxy status     PID + /healthz
//	proxy logs       跟踪日志
//
// 后台守护用"自我 re-exec"实现（Go 无 fork）：父进程 spawn 无参子进程并 Setsid
// 脱离终端，写 PID 文件后轮询就绪；子进程即正常的前台服务器。
//
// 就绪信号是子进程自己写的 ready 文件（bind 成功后写、退出时删），不能用
// /healthz 探测代替 —— 端口上可能已有一个别的 proxy 实例（如 launchd），
// 探测到健康不代表"我的子进程"绑上了端口。

// envDaemonMarker 标记子进程是后台守护实例。
const envDaemonMarker = "PROXY_DAEMON_MARKER"

// daemonReadyTimeout 是 start 等待就绪的上限。
const daemonReadyTimeout = 3 * time.Second

// daemonStopGrace 是 stop 等待优雅退出的上限，超时后 SIGKILL。
const daemonStopGrace = 5 * time.Second

// pidFilePath 返回 PID 文件路径（放在日志目录下，与 log 同生命周期）。
func pidFilePath(cfg *Config) string {
	return filepath.Join(cfg.LogDir, "ai-gateway.pid")
}

// readyFilePath 返回守护就绪标记文件路径（子进程 bind 成功后写入自身 PID）。
func readyFilePath(cfg *Config) string {
	return filepath.Join(cfg.LogDir, "ai-gateway.ready")
}

// outFilePath 返回后台守护的 stdout/stderr 落盘路径。
func outFilePath(cfg *Config) string {
	return filepath.Join(cfg.LogDir, "ai-gateway.out")
}

// logFilePath 返回代理自身日志路径（与 Logger 写的一致）。
func logFilePath(cfg *Config) string {
	return filepath.Join(cfg.LogDir, "ai-gateway.log")
}

// readPID 读取 PID 文件；不存在或非法返回 error。
func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// processAlive 判断进程是否存活（kill 0 探测；zombie 也算存活，见 daemonStop 用 ready 文件兜底）。
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// readyFileMatches 判断 ready 文件是否由指定 PID 的子进程写出（即它确实绑上了端口）。
func readyFileMatches(cfg *Config, pid int) bool {
	b, err := os.ReadFile(readyFilePath(cfg))
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(string(b))
	return err == nil && n == pid
}

// markReady / clearReady 由前台服务器路径调用：bind 成功后写、退出时删。
// 放在本文件避免与 Main 的启动逻辑纠缠。
func markReady(cfg *Config) {
	if os.Getenv(envDaemonMarker) != "1" {
		return // 前台运行不需要
	}
	_ = os.WriteFile(readyFilePath(cfg), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func clearReady(cfg *Config) {
	if os.Getenv(envDaemonMarker) != "1" {
		return
	}
	_ = os.Remove(readyFilePath(cfg))
}

// healthzClient 用于探测代理 /healthz；短超时避免无响应进程让 proxy status/stop 永久卡住。
var healthzClient = &http.Client{Timeout: 3 * time.Second}

// healthzOK 探测代理 /healthz 是否可访问。
func healthzOK(cfg *Config) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	resp, err := healthzClient.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// daemonStart 后台启动：已运行则提示并返回 0。
func daemonStart(cfg *Config) int {
	pidPath := pidFilePath(cfg)
	if pid, err := readPID(pidPath); err == nil && processAlive(pid) {
		fmt.Printf("proxy 已在运行 (PID %d)\n", pid)
		return 0
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: 创建日志目录失败: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	out, err := os.OpenFile(outFilePath(cfg), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer out.Close()

	// 无参 re-exec；Setsid 脱离终端，关闭终端后守护继续跑
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envDaemonMarker+"=1")
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: 启动失败: %v\n", err)
		return 1
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: 写 PID 文件失败: %v\n", err)
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		return 1
	}

	// 后台 goroutine Wait 回收子进程：子进程提前退出（端口被占/配置错）会被精确检测到，
	// 而不是因为 zombie 的 kill 0 仍返回存活而拖到超时。
	childDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(childDone)
	}()

	// 等子进程写出 ready（bind 成功）：子进程提前退出立即判失败。
	ready := false
	deadline := time.Now().Add(daemonReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-childDone:
			fmt.Fprintf(os.Stderr, "error: proxy 启动失败（进程已退出），见 %s\n", outFilePath(cfg))
			_ = os.Remove(pidPath)
			return 1
		default:
		}
		if readyFileMatches(cfg, cmd.Process.Pid) {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		fmt.Fprintf(os.Stderr, "error: proxy 启动超时（%s 内未就绪），见 %s\n", daemonReadyTimeout, outFilePath(cfg))
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		_ = os.Remove(pidPath)
		return 1
	}
	fmt.Printf("proxy 已后台启动 (PID %d)，日志 %s\n", cmd.Process.Pid, logFilePath(cfg))
	return 0
}

// daemonStop 停止后台守护：SIGTERM 优雅退出，按 ready 文件消失判断，超时 SIGKILL。
func daemonStop(cfg *Config) int {
	path := pidFilePath(cfg)
	pid, err := readPID(path)
	if err != nil {
		fmt.Println("proxy 未在后台运行")
		return 0
	}
	// 发信号前先确认 ready 文件也指向同一 PID：
	// PID 可能被 OS 复用给了另一个进程，直接 kill 会误杀无关进程。
	// ready 文件由子进程在 bind 成功后写入自身 PID，是比 PID 文件更强的身份证明。
	if !readyFileMatches(cfg, pid) {
		fmt.Printf("PID %d 不再是代理进程（ready 文件不匹配），清理陈旧文件\n", pid)
		_ = os.Remove(path)
		_ = os.Remove(readyFilePath(cfg))
		return 0
	}
	fmt.Printf("停止 proxy (PID %d)...\n", pid)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(daemonStopGrace)
	for time.Now().Before(deadline) {
		if !readyFileMatches(cfg, pid) {
			break // 子进程退出时删了 ready 文件
		}
		time.Sleep(200 * time.Millisecond)
	}
	if readyFileMatches(cfg, pid) {
		fmt.Println("优雅退出超时，SIGKILL")
		_ = syscall.Kill(pid, syscall.SIGKILL)
	} else {
		fmt.Println("已停止")
	}
	_ = os.Remove(path)
	_ = os.Remove(readyFilePath(cfg))
	return 0
}

// daemonRestart 停止后重启。
func daemonRestart(cfg *Config) int {
	if code := daemonStop(cfg); code != 0 {
		return code
	}
	return daemonStart(cfg)
}

// daemonStatus 显示 PID + /healthz 摘要。
func daemonStatus(cfg *Config) int {
	path := pidFilePath(cfg)
	if pid, err := readPID(path); err == nil && processAlive(pid) {
		fmt.Printf("proxy 运行中 (PID %d)\n", pid)
	} else {
		fmt.Println("proxy 未在后台运行")
	}
	if healthzOK(cfg) {
		resp, err := healthzClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz")
		if err == nil {
			defer resp.Body.Close()
			buf := make([]byte, 300)
			n, _ := resp.Body.Read(buf)
			fmt.Println("healthz:", string(buf[:n]))
		}
	} else {
		fmt.Println("healthz: 不可访问")
	}
	return 0
}

// daemonLogs 跟踪代理日志（tail -f）。
func daemonLogs(cfg *Config) int {
	path := logFilePath(cfg)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "日志不存在（%s），代理可能没跑过\n", path)
		return 1
	}
	cmd := exec.Command("tail", "-n", "80", "-f", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
	return 0
}
