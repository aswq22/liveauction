package buyersearch

import (
	"errors"
	"math"
	"sort"
)

// Quality refuses unjudged retrieved results. Grades are human judgments 0..3,
// not embedding distances or labels invented by the model being evaluated.
func Quality(ids []string, grades map[string]int, recallK, ndcgK int) (float64, float64, error) {
	if recallK < 1 || ndcgK < 1 {
		return 0, 0, errors.New("invalid evaluation cutoff")
	}
	relevant := 0
	ideal := []int{}
	for _, g := range grades {
		if g < 0 || g > 3 {
			return 0, 0, errors.New("grades must be 0..3")
		}
		if g > 0 {
			relevant++
			ideal = append(ideal, g)
		}
	}
	seen := map[string]bool{}
	hits := 0
	dcg := 0.0
	for i, id := range ids {
		if i >= recallK && i >= ndcgK {
			break
		}
		if seen[id] {
			return 0, 0, errors.New("duplicate retrieved lot")
		}
		seen[id] = true
		g, ok := grades[id]
		if !ok {
			return 0, 0, errors.New("retrieved result lacks human judgment")
		}
		if i < recallK && g > 0 {
			hits++
		}
		if i < ndcgK {
			dcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i+2))
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for i, g := range ideal {
		if i >= ndcgK {
			break
		}
		idcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i+2))
	}
	if relevant == 0 {
		return 0, 0, nil
	}
	if idcg == 0 {
		return float64(hits) / float64(relevant), 0, nil
	}
	return float64(hits) / float64(relevant), dcg / idcg, nil
}
