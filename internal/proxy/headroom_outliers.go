package proxy

import (
	"sort"
)

// 结构离群检测（对应 ai-gateway headroom/json_outliers.mjs）：
// 找出"稀有字段"与"稀有取值"所在项，压缩采样时保留它们避免丢失异常信息。

// valueKey 生成值的去重/比较键（对应 valueKey）：null → __null__；原始类型 → 字面量；其余 → JSON。
func valueKey(value any) string {
	if value == nil {
		return "__null__"
	}
	switch v := value.(type) {
	case string:
		return v
	case float64, bool:
		return jsonStringify(v)
	default:
		return jsonStringify(v)
	}
}

// commonFields 返回出现次数 >= max(1, ceil(len*0.8)) 的字段（排序），对应 commonFields。
func commonFields(items []any) []string {
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
	threshold := max(1, (len(items)*8+9)/10) // ceil(len*0.8)
	out := make([]string, 0, len(counts))
	for key, count := range counts {
		if count >= threshold {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// rareFieldIndices 返回含"稀有字段"（出现率 < 20%）的 plain-object 下标，对应 rareFieldIndices。
func rareFieldIndices(items []any) []int {
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
	rare := map[string]bool{}
	for key, count := range counts {
		if float64(count) < float64(len(items))*0.2 {
			rare[key] = true
		}
	}
	if len(rare) == 0 {
		return nil
	}
	out := make([]int, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		hasRare := false
		for key := range m {
			if rare[key] {
				hasRare = true
				break
			}
		}
		if hasRare {
			out = append(out, idx)
		}
	}
	return out
}

// rareStatusIndices 返回"稀有取值"所在项下标，对应 rareStatusIndices：
// 对每个共同字段，若唯一取值数在 [2,50] 且头部取值（覆盖 80%）不超过 5 个，
// 则不在头部取值集合的项算离群。
func rareStatusIndices(items []any, fields []string) []int {
	var out []int
	for _, field := range fields {
		var values []string
		var valueIndexes []int // 有该字段的项下标（与 values 对齐）
		for idx, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			v, has := m[field]
			if !has {
				continue
			}
			values = append(values, valueKey(v))
			valueIndexes = append(valueIndexes, idx)
		}
		unique := map[string]bool{}
		for _, v := range values {
			unique[v] = true
		}
		if len(unique) < 2 || len(unique) > 50 {
			continue
		}
		counts := map[string]int{}
		for _, v := range values {
			counts[v]++
		}
		type kv struct {
			key   string
			count int
		}
		sorted := make([]kv, 0, len(counts))
		for k, c := range counts {
			sorted = append(sorted, kv{key: k, count: c})
		}
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].count != sorted[j].count {
				return sorted[i].count > sorted[j].count
			}
			return sorted[i].key < sorted[j].key
		})
		threshold := (len(values)*8 + 9) / 10 // ceil(len*0.8)
		top := map[string]bool{}
		total := 0
		for _, kv := range sorted {
			top[kv.key] = true
			total += kv.count
			if total >= threshold {
				break
			}
		}
		if len(top) > 5 {
			continue
		}
		for i, v := range values {
			if !top[v] {
				out = append(out, valueIndexes[i])
			}
		}
	}
	return out
}

// structuralOutlierIndices 合并稀有字段 + 稀有取值下标，去重升序（对应 structuralOutlierIndices）。
func structuralOutlierIndices(items []any) []int {
	if len(items) < 5 {
		return nil
	}
	seen := map[int]bool{}
	for _, idx := range rareFieldIndices(items) {
		seen[idx] = true
	}
	for _, idx := range rareStatusIndices(items, commonFields(items)) {
		seen[idx] = true
	}
	out := make([]int, 0, len(seen))
	for idx := range seen {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}
