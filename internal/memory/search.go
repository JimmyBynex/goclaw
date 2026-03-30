package memory

import (
	"math"
	"sort"
	"time"
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

func
