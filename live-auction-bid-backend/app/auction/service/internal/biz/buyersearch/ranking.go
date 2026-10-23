package buyersearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func SemanticHash(d Document, g Generation) string {
	b, _ := json.Marshal([]any{canonical(d.Text), g.Model, g.Dimensions, g.TemplateVersion, g.ProviderVersion})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Fuse(a, b []Candidate, limit int) []Candidate {
	type scored struct {
		c Candidate
		s float64
	}
	m := map[string]*scored{}
	for _, list := range [][]Candidate{a, b} {
		seen := map[string]bool{}
		for i, c := range list {
			if c.LotID == "" || seen[c.LotID] {
				continue
			}
			seen[c.LotID] = true
			v := m[c.LotID]
			if v == nil {
				v = &scored{c: c}
				m[c.LotID] = v
			}
			v.s += 1 / float64(60+i+1)
			if c.Distance != nil {
				v.c.Distance = c.Distance
			}
			if c.LexicalScore > v.c.LexicalScore {
				v.c.LexicalScore = c.LexicalScore
				v.c.MatchedTokens = c.MatchedTokens
			}
		}
	}
	values := make([]*scored, 0, len(m))
	for _, v := range m {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].s == values[j].s {
			return values[i].c.LotID < values[j].c.LotID
		}
		return values[i].s > values[j].s
	})
	if limit > 0 && len(values) > limit {
		values = values[:limit]
	}
	out := make([]Candidate, 0, len(values))
	for i, v := range values {
		v.c.Rank = i + 1
		out = append(out, v.c)
	}
	return out
}
