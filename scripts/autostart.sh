#!/usr/bin/env bash
#
# autostart.sh — 配置 proxy 开机自启（独立脚本，不依赖 setup.sh）。
#
# 平台支持：
#   macOS  → launchd（~/Library/LaunchAgents/com.ai-gateway.proxy.plist）
#   Linux  → systemd user 服务（~/.config/systemd/user/ai-gateway.service）
#
# 用法：
#   ./scripts/autostart.sh install    安装自启（写入 launchd/systemd，立即启动 proxy）
#   ./scripts/autostart.sh uninstall  卸载自启（停掉 proxy 并移除配置）
#   ./scripts/autostart.sh status     查看自启状态
#   ./scripts/autostart.sh template   打印平台对应的配置模板（不安装）
#
# 前置条件：
#   - proxy 已安装（make install，即 ~/.ai-gateway/bin/proxy）
#   - 登录用户可写 ~/Library/LaunchAgents 或 ~/.config/systemd/user
#
# 说明：
#   - launchd 的 RunAtLoad 在用户登录时拉起 proxy start；KeepAlive 在崩溃/退出后自动重启。
#   - systemd 的 Restart=on-failure 在异常退出后自动重启。
#   - 自启用的是 proxy 二进制的守护子命令（proxy start），就绪后写 ready 文件。

set -euo pipefail

# ---------- 常量 ----------
PROXY_HOME="${PROXY_HOME:-$HOME/.ai-gateway}"
PROXY_BIN="$PROXY_HOME/bin/proxy"
LOG_DIR="$HOME/.ai-gateway"
LAUNCHD_LABEL="com.ai-gateway.proxy"
LAUNCHD_PLIST="$HOME/Library/LaunchAgents/$LAUNCHD_LABEL.plist"
SYSTEMD_SERVICE="$HOME/.config/systemd/user/ai-gateway.service"

# ---------- 工具函数 ----------
log()  { printf '\033[1;32m[autostart]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[autostart]\033[0m %s\n' "$*"; }
err()  { printf '\033[1;31m[autostart]\033[0m %s\n' "$*" >&2; exit 1; }

os_kind() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux)  echo "linux" ;;
    *)      err "不支持的系统: $(uname -s)（仅支持 macOS / Linux）" ;;
  esac
}

check_prereq() {
  [ -x "$PROXY_BIN" ] || err "proxy 未安装: 请先执行 make install（$PROXY_BIN）"
}

# ---------- macOS launchd ----------
install_darwin() {
  mkdir -p "$HOME/Library/LaunchAgents" "$LOG_DIR"
  # 直接跑前台模式（proxy 不带参数），由 launchd 管理生命周期：
  # KeepAlive=true 在崩溃/退出后自动拉起。若用 `proxy start`（内部 fork 守护），
  # launchd 会以为进程已退出而反复重启，故这里用前台模式。
  # 不设 StandardOutPath/StandardErrorPath：proxy 日志由 Logger 统一写 ai-gateway.log
  # （RotatingWriter 轮转），重定向 stdout/stderr 只会产生重复的 autostart.out/err。
  cat > "$LAUNCHD_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LAUNCHD_LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$PROXY_BIN</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
EOF
  # 先卸载旧实例再加载，保证幂等
  launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true
  launchctl load "$LAUNCHD_PLIST"
  log "launchd 已配置并启动: $LAUNCHD_PLIST"
}

uninstall_darwin() {
  if [ -f "$LAUNCHD_PLIST" ]; then
    launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true
    rm -f "$LAUNCHD_PLIST"
    log "launchd 已卸载: $LAUNCHD_PLIST"
  else
    warn "launchd 未配置: $LAUNCHD_PLIST"
  fi
}

status_darwin() {
  if [ -f "$LAUNCHD_PLIST" ]; then
    echo "launchd: 已配置 ($LAUNCHD_PLIST)"
    launchctl list | grep -F "$LAUNCHD_LABEL" && echo "  → 已加载运行" || echo "  → 未加载"
  else
    echo "launchd: 未配置"
  fi
}

# ---------- Linux systemd ----------
install_linux() {
  mkdir -p "$HOME/.config/systemd/user" "$LOG_DIR"
  # 直接跑前台模式（proxy 不带参数），由 systemd 管理生命周期：
  # Restart=on-failure 在异常退出后自动拉起。`proxy start` 内部 fork 守护进程后会立即返回，
  # systemd 会误判进程退出而反复重启，故这里用前台模式。
  # 不重定向 stdout/stderr：proxy 日志由 Logger 统一写 ai-gateway.log。
  cat > "$SYSTEMD_SERVICE" <<EOF
[Unit]
Description=ai-gateway proxy
After=network-online.target

[Service]
Type=simple
ExecStart=$PROXY_BIN
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now ai-gateway.service
  log "systemd 已配置并启动: $SYSTEMD_SERVICE"
}

uninstall_linux() {
  systemctl --user disable --now ai-gateway.service 2>/dev/null || true
  rm -f "$SYSTEMD_SERVICE"
  systemctl --user daemon-reload
  log "systemd 已卸载: $SYSTEMD_SERVICE"
}

status_linux() {
  if [ -f "$SYSTEMD_SERVICE" ]; then
    echo "systemd: 已配置 ($SYSTEMD_SERVICE)"
    systemctl --user is-enabled ai-gateway.service 2>/dev/null && echo "  → 已启用" || echo "  → 未启用"
    systemctl --user is-active ai-gateway.service 2>/dev/null && echo "  → 运行中" || echo "  → 未运行"
  else
    echo "systemd: 未配置"
  fi
}

# ---------- 模板输出 ----------
template_darwin() {
  cat <<'EOF'
# macOS launchd 自启模板（安装到 ~/Library/LaunchAgents/com.ai-gateway.proxy.plist）
# 用 `launchctl load ~/Library/LaunchAgents/com.ai-gateway.proxy.plist` 加载
# 注意：直接跑前台模式（不带 start 参数），由 launchd 的 KeepAlive 管理重启
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ai-gateway.proxy</string>
    <key>ProgramArguments</key>
    <array>
        <string>__PROXY_BIN__</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
EOF
  echo ""
  echo "# 把 __PROXY_BIN__ 替换为实际路径: $PROXY_BIN"
}

template_linux() {
  cat <<'EOF'
# Linux systemd user 服务自启模板（安装到 ~/.config/systemd/user/ai-gateway.service）
# 用 `systemctl --user enable --now ai-gateway.service` 启用
# 注意：直接跑前台模式（不带 start 参数），由 systemd 的 Restart 管理重启
[Unit]
Description=ai-gateway proxy
After=network-online.target

[Service]
Type=simple
ExecStart=__PROXY_BIN__
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF
  echo ""
  echo "# 把 __PROXY_BIN__ 替换为实际路径: $PROXY_BIN"
}

# ---------- 主流程 ----------
cmd="${1:-}"

case "$cmd" in
  install)
    check_prereq
    case "$(os_kind)" in
      darwin) install_darwin ;;
      linux)  install_linux ;;
    esac
    log "完成！proxy 已配置开机自启。"
    ;;
  uninstall)
    case "$(os_kind)" in
      darwin) uninstall_darwin ;;
      linux)  uninstall_linux ;;
    esac
    log "完成！已移除开机自启。"
    ;;
  status)
    case "$(os_kind)" in
      darwin) status_darwin ;;
      linux)  status_linux ;;
    esac
    ;;
  template)
    case "$(os_kind)" in
      darwin) template_darwin ;;
      linux)  template_linux ;;
    esac
    ;;
  help|-h|--help|"")
    cat <<'EOF'
用法: scripts/autostart.sh [命令]

命令:
  install     安装自启（launchd / systemd user 服务）并立即启动 proxy
  uninstall   卸载自启并停止
  status      查看自启状态
  template    打印当前平台的配置模板（不安装）

前置条件: proxy 已安装（make install，即 ~/.ai-gateway/bin/proxy）
EOF
    ;;
  *)
    err "未知命令: $cmd (可用: install / uninstall / status / template / help)"
    ;;
esac
