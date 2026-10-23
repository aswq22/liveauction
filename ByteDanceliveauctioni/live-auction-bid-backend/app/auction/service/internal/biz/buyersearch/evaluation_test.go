package buyersearch

import (
	"math"
	"testing"
)

func TestRetrievalQualityMetrics(t *testing.T) {
	r, n, e := Quality([]string{"a", "b", "c"}, map[string]int{"a": 3, "b": 0, "c": 1}, 50, 10)
	if e != nil || r != 1 || n <= 0 || n >= 1 {
		t.Fatal(r, n, e)
	}
	r, n, e = Quality([]string{"a", "c"}, map[string]int{"a": 3, "c": 1}, 50, 10)
	if e != nil || r != 1 || math.Abs(n-1) > 1e-9 {
		t.Fatal(r, n, e)
	}
	if _, _, e = Quality([]string{"unjudged"}, map[string]int{"a": 1}, 50, 10); e == nil {
		t.Fatal("unjudged result counted as irrelevant")
	}
}
