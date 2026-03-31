package memory

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-ego/gse"
)

func applyTimeDecay(entries []*Entry) []*Entry {
	if len(entries) == 0 {
		return entries
	}

	const alpha = 0.7         // BM25 权重
	const beta = 0.3          // 时间权重
	const halfLifeDays = 30.0 // 时间分数减半的日期

	maxBM25 := entries[0].Score
	for _, e := range entries {
		if e.Score > maxBM25 {
			maxBM25 = e.Score
		}
	}
	for _, e := range entries {
		now := time.Now()
		bm25Norm := 0.0
		if maxBM25 > 0 {
			//归一化
			bm25Norm = e.Score / maxBM25
		}
		//时间衰减分数
		ageDays := now.Sub(e.CreatedAt).Hours() / 24
		timeScore := math.Exp(-math.Log(2) / halfLifeDays * ageDays)
		//综合分数
		e.Score = alpha*bm25Norm + beta*timeScore
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})
	return entries
}

// tokenize 用于 Jaccard 相似度计算，接入 gse 支持中文分词
// 注意：这是 search.go 里的包级函数，和 SQLiteStore.tokenize 方法不同
// SQLiteStore.tokenize 返回空格拼接的字符串（给 FTS5 用）
// 这里返回词集合（给 Jaccard 用）
func tokenize(seg *gse.Segmenter, text string) map[string]bool {
	words := seg.Slice(strings.ToLower(text), true)
	set := make(map[string]bool)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w != "" {
			set[w] = true
		}
	}
	return set
}

// jaccardsimilarity 计算两段文本的交集并集相似度
func jaccardSimilarity(seg *gse.Segmenter, a, b string) float64 {
	setA := tokenize(seg, a)
	setB := tokenize(seg, b)
	intersection := 0
	for w := range setA {
		if _, ok := setB[w]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// MMR（最大边际相关性）去重
// 避免返回内容高度相似的多条记忆
// 在记忆数量很多时使用，Phase 8 可选
func mmrRerank(seg *gse.Segmenter, entries []*Entry, lambda float64, k int) []*Entry {
	if len(entries) <= k {
		return entries
	}

	selected := make([]*Entry, 0, k)
	remaining := make([]*Entry, len(entries))
	copy(remaining, entries)

	// 贪心选择：每次选综合分数最高的，同时惩罚与已选内容相似的
	for len(selected) < k && len(remaining) > 0 {
		bestIdx := 0
		bestScore := -1.0

		for i, candidate := range remaining {
			// 与已选记忆的最大相似度（简单版：基于文字重叠率）
			maxSim := 0.0
			for _, sel := range selected {
				sim := jaccardSimilarity(seg, candidate.Content, sel.Content)
				if sim > maxSim {
					maxSim = sim
				}
			}
			// MMR 分数 = λ×相关性 - (1-λ)×冗余度
			mmrScore := lambda*candidate.Score - (1-lambda)*maxSim
			if mmrScore > bestScore {
				bestScore = mmrScore
				bestIdx = i
			}
		}

		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}

	return selected
}
