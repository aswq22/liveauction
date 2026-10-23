package buyersearch

import (
	"fmt"
	"golang.org/x/text/unicode/norm"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const amountText = `([0-9]+(?:\.[0-9]{1,2})?|[零〇一二两三四五六七八九十百千万]+)\s*([kK万]?)`

var budgetPrefix = regexp.MustCompile(`(?:预算|不超过|不高于|最多|低于|小于)\s*` + amountText + `\s*(?:元|块)?`)
var budgetSuffix = regexp.MustCompile(amountText + `\s*(?:元|块)?\s*(?:以内|以下|之内)`)
var negatedConstraint = regexp.MustCompile(`不要|不想|不是|不含|排除|不看|别要|不需要|不正在|不即将`)

func canonical(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(norm.NFKC.String(text)), " "))
}
func Parse(q Query) (ParsedQuery, error) {
	p := ParsedQuery{SemanticText: canonical(q.Text), PageSize: q.PageSize, Warnings: []string{}, Filters: Filters{RoomID: strings.TrimSpace(q.RoomID), MinPrice: q.MinPrice, MaxPrice: q.MaxPrice}}
	invalid := func(reason string) (ParsedQuery, error) {
		return ParsedQuery{}, fmt.Errorf("%w: %s", ErrInvalidQuery, reason)
	}
	if !utf8.ValidString(q.Text) || utf8.RuneCountInString(p.SemanticText) < 1 || utf8.RuneCountInString(p.SemanticText) > 256 {
		return invalid("query must contain 1–256 characters")
	}
	if len(p.Filters.RoomID) > 64 || len(q.Cursor) > 2048 {
		return invalid("invalid room or cursor length")
	}
	if p.PageSize == 0 {
		p.PageSize = 20
	}
	if p.PageSize < 1 || p.PageSize > 50 {
		return invalid("page size must be 1–50")
	}
	if (q.MinPrice != nil && *q.MinPrice < 0) || (q.MaxPrice != nil && *q.MaxPrice < 0) {
		return invalid("negative budget")
	}
	if q.MinPrice != nil && q.MaxPrice != nil && *q.MinPrice > *q.MaxPrice {
		return invalid("inverted price range")
	}
	for _, s := range q.Statuses {
		if !PublicStatus(s) {
			return invalid("non-public status")
		}
		if !containsStatus(p.Filters.Statuses, s) {
			p.Filters.Statuses = append(p.Filters.Statuses, s)
		}
	}
	if len(p.Filters.Statuses) == 0 {
		switch {
		case negatedConstraint.MatchString(p.SemanticText):
			p.Filters.Statuses = []v1.LotStatus{v1.LotStatus_LOT_STATUS_QUEUED, v1.LotStatus_LOT_STATUS_LIVE, v1.LotStatus_LOT_STATUS_EXTENDED}
		case strings.Contains(p.SemanticText, "正在拍") || strings.Contains(p.SemanticText, "正在竞拍") || strings.Contains(p.SemanticText, "竞拍中"):
			p.Filters.Statuses = []v1.LotStatus{v1.LotStatus_LOT_STATUS_LIVE, v1.LotStatus_LOT_STATUS_EXTENDED}
		case strings.Contains(p.SemanticText, "即将开拍"):
			p.Filters.Statuses = []v1.LotStatus{v1.LotStatus_LOT_STATUS_QUEUED}
		default:
			p.Filters.Statuses = []v1.LotStatus{v1.LotStatus_LOT_STATUS_QUEUED, v1.LotStatus_LOT_STATUS_LIVE, v1.LotStatus_LOT_STATUS_EXTENDED}
		}
	}
	sort.Slice(p.Filters.Statuses, func(i, j int) bool { return p.Filters.Statuses[i] < p.Filters.Statuses[j] })
	semantic := p.SemanticText
	if q.MaxPrice == nil {
		var found *int64
		for _, rx := range []*regexp.Regexp{budgetPrefix, budgetSuffix} {
			for _, m := range rx.FindAllStringSubmatch(p.SemanticText, -1) {
				n, e := parseAmount(m[1], m[2])
				if e != nil {
					return invalid("invalid or overflowing budget")
				}
				if found != nil && *found != n {
					return invalid("conflicting budgets")
				}
				v := n
				found = &v
			}
		}
		p.Filters.MaxPrice = found
	}
	if p.Filters.MinPrice != nil && p.Filters.MaxPrice != nil && *p.Filters.MinPrice > *p.Filters.MaxPrice {
		return invalid("inverted price range")
	}
	// Remove recognized numeric constraints from the vector query only. Negation
	// is retained and declared unhandled until the domain supports it explicitly.
	semantic = budgetPrefix.ReplaceAllString(semantic, " ")
	semantic = budgetSuffix.ReplaceAllString(semantic, " ")
	if negatedConstraint.MatchString(p.SemanticText) {
		p.Warnings = append(p.Warnings, "排除条件尚未作为严格筛选，请核对商品详情")
	}
	p.CanonicalText = p.SemanticText
	p.SemanticText = strings.TrimSpace(semantic)
	if p.SemanticText == "" {
		p.SemanticText = p.CanonicalText
	}
	p.Tokens = Tokenize(p.SemanticText)
	return p, nil
}
func containsStatus(a []v1.LotStatus, s v1.LotStatus) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func parseAmount(text, unit string) (int64, error) {
	if strings.ContainsAny(text, "零〇一二两三四五六七八九十百千万") {
		v, e := chineseInteger(text)
		if e != nil {
			return 0, e
		}
		text = strconv.FormatInt(v, 10)
	}
	multiplier := int64(100)
	switch strings.ToLower(unit) {
	case "k":
		multiplier *= 1000
	case "万":
		multiplier *= 10000
	}
	parts := strings.Split(text, ".")
	whole, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || whole < 0 || whole > math.MaxInt64/multiplier {
		return 0, ErrInvalidQuery
	}
	out := whole * multiplier
	if len(parts) == 2 {
		fraction := parts[1]
		if len(fraction) == 1 {
			fraction += "0"
		}
		f, e := strconv.ParseInt(fraction, 10, 64)
		if e != nil || len(fraction) != 2 {
			return 0, ErrInvalidQuery
		}
		extra := f * (multiplier / 100)
		if out > math.MaxInt64-extra {
			return 0, ErrInvalidQuery
		}
		out += extra
	}
	return out, nil
}
func chineseInteger(text string) (int64, error) {
	digits := map[rune]int64{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	units := map[rune]int64{'十': 10, '百': 100, '千': 1000, '万': 10000}
	var total, section, digit int64
	for _, r := range text {
		if n, ok := digits[r]; ok {
			digit = n
			continue
		}
		u, ok := units[r]
		if !ok {
			return 0, ErrInvalidQuery
		}
		if u == 10000 {
			section += digit
			if section == 0 {
				section = 1
			}
			total += section * u
			section = 0
		} else {
			if digit == 0 {
				digit = 1
			}
			section += digit * u
		}
		digit = 0
	}
	return total + section + digit, nil
}
