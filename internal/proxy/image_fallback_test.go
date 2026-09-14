package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsImageUnsupportedError(t *testing.T) {
	// 真实网关实测（2026-09-09 GLM-5.2）
	realError := `{"error":{"message":"Model do not support image input. Request id: 021788951958711f5b3d0a7c8863bd3ab04f9ae7f041b1016fea8","type":"BadRequest","param":"image_url","code":"InvalidParameter"}}`
	if !isImageUnsupportedError(realError) {
		t.Error("真实网关错误未被识别")
	}

	// 其他网关的常见写法（缺三单 s）
	if !isImageUnsupportedError(`{"error":{"message":"Model only support text input"}}`) {
		t.Error("only support text 未识别")
	}
	if !isImageUnsupportedError(`{"error":{"message":"Model only supports text input"}}`) {
		t.Error("only supports text 未识别")
	}

	// 中文错误
	if !isImageUnsupportedError(`{"error":{"message":"模型不支持图片输入"}}`) {
		t.Error("中文图片不支持错误未识别")
	}

	// 其他类型错误不应触发
	for _, body := range []string{
		`{"error":{"message":"rate limit exceeded"}}`,
		`{"error":{"message":"invalid model"}}`,
		`{"error":{"message":"authorization failed"}}`,
		`{"error":{"message":"token expired"}}`,
	} {
		if isImageUnsupportedError(body) {
			t.Errorf("非图片错误被误判: %s", body)
		}
	}
}

func TestRequestHasImages(t *testing.T) {
	withImage := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,abc"},
			}},
		},
	}
	if !requestHasImages(withImage) {
		t.Error("含图片的请求未检测出来")
	}

	withoutImage := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	if requestHasImages(withoutImage) {
		t.Error("纯文本请求被误判含图片")
	}
}

func TestRewriteBodyOmitImages(t *testing.T) {
	original := []byte(`{
		"model": "glm-5.2",
		"input": [{
			"role": "user",
			"content": [{
				"type": "input_image",
				"image_url": "data:image/png;base64,iVBORw0KGgo="
			}]
		}]
	}`)
	adapt := &adapterContext{model: "glm-5.2", upstreamPath: "/messages"}

	rewritten, ok := rewriteBodyOmitImages(original, adapt)
	if !ok {
		t.Fatal("含图片的请求应该被改写")
	}

	// 改写后不含 image 类型块
	if strings.Contains(string(rewritten), "image") {
		t.Errorf("改写后仍含 image: %s", string(rewritten))
	}
	// 改写后含占位文本
	var doc map[string]any
	if err := json.Unmarshal(rewritten, &doc); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v", err)
	}

	// 无图片请求不改写
	noImg := []byte(`{"model":"glm-5.2","input":[{"role":"user","content":"hello"}]}`)
	_, ok2 := rewriteBodyOmitImages(noImg, adapt)
	if ok2 {
		t.Error("无图片请求不应触发改写")
	}
}
