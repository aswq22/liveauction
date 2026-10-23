package buyersearch

import (
	"strings"
	"unicode"
)

// Tokens contain only letters and numbers. The index/query share this tokenizer;
// raw user strings never become tsquery syntax. Domain aliases are versioned.
func Tokenize(text string) []string {
	text = canonical(text)
	seen := map[string]bool{}
	out := []string{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var han, latin []rune
	flushHan := func() {
		if len(han) == 1 {
			add(string(han))
		}
		for i := 0; i+1 < len(han); i++ {
			add(string(han[i : i+2]))
		}
		han = nil
	}
	flushLatin := func() { add(string(latin)); latin = nil }
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			flushLatin()
			han = append(han, r)
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			flushHan()
			latin = append(latin, r)
		} else {
			flushHan()
			flushLatin()
		}
	}
	flushHan()
	flushLatin()
	for _, pair := range [][2]string{{"镯子", "手镯"}, {"耳环", "耳饰"}, {"项链", "吊坠"}, {"玉石", "翡翠"}} {
		if strings.Contains(text, pair[0]) {
			add(pair[1])
		}
	}
	return out
}
