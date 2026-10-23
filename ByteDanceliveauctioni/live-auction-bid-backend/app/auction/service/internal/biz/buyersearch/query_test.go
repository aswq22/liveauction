package buyersearch

import (
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"strings"
	"testing"
)

func TestParseBudgetAndModelNumbers(t *testing.T) {
	for _, tc := range []struct {
		text   string
		amount int64
	}{
		{"给妈妈的礼物，三千以内", 300000}, {"不超过三千的玉石", 300000}, {"预算 2k 翡翠", 200000}, {"3000元以下手镯", 300000},
		{"iPhone 15", -1}, {"型号3000", -1},
	} {
		t.Run(tc.text, func(t *testing.T) {
			p, e := Parse(Query{Text: tc.text})
			if e != nil {
				t.Fatal(e)
			}
			if tc.amount < 0 {
				if p.Filters.MaxPrice != nil {
					t.Fatal("model number treated as budget")
				}
			} else if p.Filters.MaxPrice == nil || *p.Filters.MaxPrice != tc.amount {
				t.Fatalf("budget=%v want %d", p.Filters.MaxPrice, tc.amount)
			}
		})
	}
	max := int64(0)
	p, e := Parse(Query{Text: "预算三千", MaxPrice: &max})
	if e != nil || p.Filters.MaxPrice == nil || *p.Filters.MaxPrice != 0 {
		t.Fatal("explicit zero must override inferred budget", e)
	}
}
func TestNegatedStatusDoesNotApplyOppositeFilter(t *testing.T) {
	for _, text := range []string{"不要正在拍的手镯", "不是正在拍的手镯", "不想要即将开拍的玉"} {
		p, e := Parse(Query{Text: text})
		if e != nil || len(p.Filters.Statuses) != 3 || len(p.Warnings) == 0 {
			t.Fatal(text, p, e)
		}
	}
	p, e := Parse(Query{Text: "不要正在拍的手镯", Statuses: []v1.LotStatus{v1.LotStatus_LOT_STATUS_QUEUED}})
	if e != nil || len(p.Filters.Statuses) != 1 || p.Filters.Statuses[0] != v1.LotStatus_LOT_STATUS_QUEUED {
		t.Fatal(p, e)
	}
}
func TestParseBounds(t *testing.T) {
	for _, q := range []Query{{Text: " "}, {Text: strings.Repeat("玉", 257)}, {Text: "玉", PageSize: 51}, {Text: "玉", Statuses: []v1.LotStatus{v1.LotStatus_LOT_STATUS_DRAFT}}, {Text: "预算2000元，不超过3000元"}} {
		if _, e := Parse(q); e == nil {
			t.Fatalf("accepted invalid query: %+v", q)
		}
	}
	p, e := Parse(Query{Text: strings.Repeat("玉", 256)})
	if e != nil || p.PageSize != 20 {
		t.Fatal(p, e)
	}
	min, max := int64(100), int64(0)
	if _, e := Parse(Query{Text: "玉", MinPrice: &min, MaxPrice: &max}); e == nil {
		t.Fatal("accepted inverted range")
	}
}
func TestTokenizeChineseAndOperators(t *testing.T) {
	tokens := Tokenize("翡翠手镯 iPhone１５ ':&|!")
	joined := strings.Join(tokens, " ")
	for _, want := range []string{"翡翠", "翠手", "手镯", "iphone15"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q missing %q", joined, want)
		}
	}
	for _, token := range tokens {
		if strings.ContainsAny(token, "':&|!") {
			t.Fatal("unsafe token", token)
		}
	}
}
func TestNegationWarnings(t *testing.T) {
	p, e := Parse(Query{Text: "不要镀金的耳饰"})
	if e != nil || !strings.Contains(p.SemanticText, "不要") || len(p.Warnings) == 0 {
		t.Fatal(p, e)
	}
}
func TestRRFAndSemanticHash(t *testing.T) {
	out := Fuse([]Candidate{{LotID: "a"}, {LotID: "b"}}, []Candidate{{LotID: "b"}, {LotID: "c"}}, 10)
	if len(out) != 3 || out[0].LotID != "b" {
		t.Fatal(out)
	}
	g := Generation{Model: "m", Dimensions: 2, TemplateVersion: "1"}
	d := Document{LotID: "a", Text: "翡翠", Revision: Revision{Semantic: 1, Metadata: 1}}
	h := SemanticHash(d, g)
	d.Status = v1.LotStatus_LOT_STATUS_LIVE
	d.Revision.Metadata = 2
	if SemanticHash(d, g) != h {
		t.Fatal("metadata changed semantic hash")
	}
	d.Text = "和田玉"
	if SemanticHash(d, g) == h {
		t.Fatal("content must change hash")
	}
}
