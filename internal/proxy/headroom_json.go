package proxy

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// jsonObjectTables 的 key 排序用（encoding/json 对 map 键按字母序输出，确定性足够）。

// importantLine 判断 JSON 项是否"重要"：
// lineScore >= 0.8 或命中错误关键词。
func importantLine(value string) bool {
	return lineScore(value) >= 0.8 || errorLinePattern.MatchString(value)
}

var errorLinePattern = regexp.MustCompile(`error|exception|failed|failure|fatal|panic|traceback|warning|warn|denied|timeout|timed out|cannot|unable|segfault|assert`)

// crushJsonArray 采样数组：
// 前 4 条 → 后 3 条 → 重要行 → 结构离群 → 均匀补采样，按 JSON 值去重，截断到 maxItems。
func crushJsonArray(items []any, maxItems int) []any {
	// 重要行（最多 maxItems 条）
	errorItems := make([]any, 0, maxItems)
	for _, item := range items {
		if importantLine(jsonStringify(item)) {
			errorItems = append(errorItems, item)
			if len(errorItems) >= maxItems {
				break
			}
		}
	}
	keep := make([]any, 0, maxItems)
	seen := map[string]bool{}
	add := func(item any) {
		key := jsonStringify(item)
		if seen[key] {
			return
		}
		seen[key] = true
		keep = append(keep, item)
	}
	// items.slice(0, 4)
	for _, item := range items[:min(4, len(items))] {
		add(item)
	}
	// items.slice(-3)：后 3 条（数组不足 3 条时是全部，但去重兜底）
	for _, item := range items[max(0, len(items)-3):] {
		add(item)
	}
	for _, item := range errorItems {
		add(item)
	}
	// structuralOutlierIndices 前 maxItems 个
	outliers := structuralOutlierIndices(items)
	for _, idx := range outliers[:min(maxItems, len(outliers))] {
		add(items[idx])
	}
	// 均匀补采样
	if len(keep) < maxItems && len(items) > 8 {
		slots := maxItems - len(keep)
		step := max(1, len(items)/(slots+1))
		for i := step; i < len(items)-3 && len(keep) < maxItems; i += step {
			add(items[i])
		}
	}
	if len(keep) > maxItems {
		keep = keep[:maxItems]
	}
	return keep
}

// inferType 推断字段值类型（对应 inferType）：过滤 nil，数组/对象/原始类型；混合返回 mixed。
func inferType(values []any) string {
	types := map[string]bool{}
	for _, value := range values {
		if value == nil {
			continue
		}
		switch value.(type) {
		case []any:
			types["array"] = true
		case map[string]any:
			types["object"] = true
		case string:
			types["string"] = true
		case float64:
			types["number"] = true
		case bool:
			types["boolean"] = true
		default:
			types["mixed"] = true
		}
	}
	if len(types) == 0 {
		return "null"
	}
	if len(types) == 1 {
		for k := range types {
			return k
		}
	}
	return "mixed"
}

// csvCell 转 CSV 单元格：含逗号/引号/换行时加引号并转义（对应 csvCell）。
func csvCell(value any) string {
	if value == nil {
		return ""
	}
	raw, ok := value.(string)
	if !ok {
		raw = jsonStringify(value)
	}
	if strings.ContainsAny(raw, ",\"\n\r") {
		return `"` + strings.ReplaceAll(raw, `"`, `""`) + `"`
	}
	return raw
}

// schemaColumns 提取数组对象的列：出现次数 >= max(2, floor(len*0.8))，
// 按频次降序、同频按名称升序，最多 24 列（对应 schemaColumns）。
func schemaColumns(items []any) []string {
	counts := map[string]int{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for key := range m {
			counts[key]++
		}
	}
	threshold := max(2, len(items)*8/10)
	type pair struct {
		key  string
		freq int
	}
	pairs := make([]pair, 0, len(counts))
	for key, freq := range counts {
		if freq >= threshold {
			pairs = append(pairs, pair{key: key, freq: freq})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].freq != pairs[j].freq {
			return pairs[i].freq > pairs[j].freq
		}
		return pairs[i].key < pairs[j].key
	})
	if len(pairs) > 24 {
		pairs = pairs[:24]
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.key)
	}
	return out
}

// looksLikeHomogeneousObjectArray 判断是否为同质对象数组（对应同名函数）：
// 数量 >= 20、plain-object 占比 >= 0.9、schema 列数 >= 2。
func looksLikeHomogeneousObjectArray(items []any) bool {
	if len(items) < 20 {
		return false
	}
	objectCount := 0
	for _, item := range items {
		if isPlainObject(item) {
			objectCount++
		}
	}
	if float64(objectCount)/float64(len(items)) < 0.9 {
		return false
	}
	return len(schemaColumns(items)) >= 2
}

// columnValues 取某列在所有对象上的值（非对象项取 nil，对应 JS item[column] → undefined）。
func columnValues(items []any, column string) []any {
	values := make([]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			values = append(values, m[column])
		} else {
			values = append(values, nil)
		}
	}
	return values
}

// formatJsonTable 生成顶层 json_table 表格（对应 formatJsonTable）。
func formatJsonTable(items []any, maxItems int, hash string, originalChars int) string {
	columns := schemaColumns(items)
	sample := crushJsonArray(items, maxItems)
	schema := make([]string, 0, len(columns))
	for _, column := range columns {
		schema = append(schema, column+":"+inferType(columnValues(items, column)))
	}
	rows := make([]string, 0, len(sample))
	for _, item := range sample {
		cells := make([]string, 0, len(columns))
		for _, column := range columns {
			var v any
			if m, ok := item.(map[string]any); ok {
				v = m[column]
			}
			cells = append(cells, csvCell(v))
		}
		rows = append(rows, strings.Join(cells, ","))
	}
	header := "[ai-gateway headroom: compressed tool output; kind=json_table; original_items=" +
		itoa(len(items)) + "; kept_rows=" + itoa(len(sample)) + "; omitted_rows=" + itoa(max(0, len(items)-len(sample))) +
		"; columns=" + strings.Join(columns, ",") + "; original_chars=" + itoa(originalChars) + "; sha256=" + hash + "]"
	lines := []string{header, "[" + itoa(len(items)) + "]{" + strings.Join(schema, ",") + "}", strings.Join(columns, ",")}
	lines = append(lines, rows...)
	return strings.Join(lines, "\n")
}

// formatNestedJsonTable 生成嵌套对象的 json_object_tables（对应 formatNestedJsonTable）。
func formatNestedJsonTable(key string, items []any, maxItems int) string {
	columns := schemaColumns(items)
	sample := crushJsonArray(items, maxItems)
	schema := make([]string, 0, len(columns))
	for _, column := range columns {
		schema = append(schema, column+":"+inferType(columnValues(items, column)))
	}
	rows := make([]string, 0, len(sample))
	for _, item := range sample {
		cells := make([]string, 0, len(columns))
		for _, column := range columns {
			var v any
			if m, ok := item.(map[string]any); ok {
				v = m[column]
			}
			cells = append(cells, csvCell(v))
		}
		rows = append(rows, strings.Join(cells, ","))
	}
	header := "[ai-gateway headroom table; key=" + key + "; original_items=" + itoa(len(items)) +
		"; kept_rows=" + itoa(len(sample)) + "; omitted_rows=" + itoa(max(0, len(items)-len(sample))) +
		"; columns=" + strings.Join(columns, ",") + "]"
	lines := []string{header, "[" + itoa(len(items)) + "]{" + strings.Join(schema, ",") + "}", strings.Join(columns, ",")}
	lines = append(lines, rows...)
	return strings.Join(lines, "\n")
}

// minifyJson 无损兜底：去掉无意义空白，必须严格更短（对应 minifyJson）。
func minifyJson(parsed any, text string) *Compression {
	minified, err := json.Marshal(parsed)
	if err != nil || charLen(string(minified)) >= charLen(text) {
		return nil
	}
	return &Compression{Kind: "json_minified", Compressed: string(minified), Lossless: true}
}

// tryCompressJsonText 尝试 JSON 压缩（对应 tryCompressJsonText）：
// 顶层数组 → json_table / json_array；顶层对象 → json_object_tables / json_object_arrays；兜底 minify。
func tryCompressJsonText(text string, maxItems int, hash string) *Compression {
	trimmed := strings.TrimSpace(text)
	if !(strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")) || charLen(trimmed) >= 2_000_000 {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil
	}
	originalChars := charLen(text)
	if arr, ok := parsed.([]any); ok && len(arr) >= 5 {
		if looksLikeHomogeneousObjectArray(arr) {
			return &Compression{Kind: "json_table", Compressed: formatJsonTable(arr, maxItems, hash, originalChars)}
		}
		sample := crushJsonArray(arr, maxItems)
		header := "[ai-gateway headroom: compressed tool output; kind=json_array; original_items=" + itoa(len(arr)) +
			"; kept_items=" + itoa(len(sample)) + "; original_chars=" + itoa(originalChars) + "; sha256=" + hash + "]"
		return &Compression{Kind: "json_array", Compressed: header + "\n" + indentJSON(sample)}
	}
	if obj, ok := parsed.(map[string]any); ok {
		out := map[string]any{}
		changed := false
		tableChanged := false
		for key, value := range obj {
			if arr, ok := value.([]any); ok && len(arr) >= 5 {
				if looksLikeHomogeneousObjectArray(arr) {
					out[key] = formatNestedJsonTable(key, arr, maxItems)
					tableChanged = true
				} else {
					out[key] = crushJsonArray(arr, maxItems)
				}
				changed = true
			} else {
				out[key] = value
			}
		}
		if changed {
			kind := "json_object_arrays"
			if tableChanged {
				kind = "json_object_tables"
			}
			header := "[ai-gateway headroom: compressed tool output; kind=" + kind + "; original_chars=" + itoa(originalChars) + "; sha256=" + hash + "]"
			return &Compression{Kind: kind, Compressed: header + "\n" + indentJSON(out)}
		}
	}
	return minifyJson(parsed, text)
}
