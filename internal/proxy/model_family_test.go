package proxy

// 以下两个薄封装仅测试使用：生产路径不需要通过无参全局函数访问内置表策略，
// 直接持有 *ModelPolicy 即可，故从 model_family.go 移到本测试文件。

// supportsImageInput 用内置表判定图片能力（薄封装，仅测试使用）。
func supportsImageInput(model string) bool { return defaultModelPolicy.SupportsImageInput(model) }

// classifyModelForResponses 用内置表分类（薄封装，仅测试使用）。
func classifyModelForResponses(model string) *ModelClass { return defaultModelPolicy.Classify(model) }
