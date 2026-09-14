// ai-gateway-go 是一个独立的本地 OpenAI 兼容代理服务。
//
// 用于把 AI 客户端（codex/claude 等）的 /v1/* 请求转发到第三方 OpenAI 兼容上游，
// 提供协议适配（Responses↔Messages/Chat Completions）、重试、SSE 流式、Headroom
// 上下文压缩、监控仪表盘等能力。不依赖任何内部服务，可独立部署给第三方用户。
package main

import (
	"os"

	"ai-gateway-go/internal/proxy"
)

func main() {
	os.Exit(proxy.Main())
}
