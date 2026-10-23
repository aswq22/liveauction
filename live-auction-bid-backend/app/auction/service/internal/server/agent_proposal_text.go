package server

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var proposalCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(^|[^a-z0-9])sk-(?:proj-|svcacct-)?[a-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)(^|[^a-z0-9_])ghp_[a-z0-9]{20,}`),
	regexp.MustCompile(`(?i)(^|[^a-z0-9_])github_pat_[a-z0-9_]{20,}`),
	regexp.MustCompile(`(?i)(^|[^a-z0-9])xox[a-z0-9]*-[a-z0-9-]{10,}`),
	regexp.MustCompile(`(^|[^A-Za-z0-9_-])AIza[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`(^|[^A-Z0-9])(AKIA|ASIA)[A-Z0-9]{16}($|[^A-Z0-9])`),
	regexp.MustCompile(`(?i)(^|[^a-z0-9_])sk_live_[a-z0-9]{16,}`),
	regexp.MustCompile(`(^|[^A-Za-z0-9_-])[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}($|[^A-Za-z0-9_-])`),
	regexp.MustCompile(`(?i)-----BEGIN[\p{Z}\s]+[A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])bearer[\p{Z}\s]+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])basic[\p{Z}\s]+[a-z0-9+/]{12,}={0,2}`),
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]{1,20}://[^\p{Z}\s/?#@]{3,}@`),
	regexp.MustCompile(`(?i)(^|[\p{Z}\s(])[^:\p{Z}\s/@]{1,64}:[^@\p{Z}\s/]{1,128}@[a-z0-9.-]+`),
}

var proposalSensitiveLabels = []string{
	"password", "passwd", "pwd", "clientsecret", "accesstoken", "refreshtoken",
	"apikey", "accesskey", "secretkey", "privatekey", "secret", "token",
	"credential", "credentials", "authorization", "dsn", "databaseurl", "databaseuri", "connectionstring",
}

const maxProposalSensitiveLabelRunes = 32

func validateProposalText(value *string, maxBytes int) bool {
	if value == nil || !utf8.ValidString(*value) || hasUnsafeProposalTextRune(*value) {
		return false
	}
	original := strings.TrimSpace(*value)
	if original == "" || len(original) > maxBytes {
		return false
	}
	normalized := strings.TrimSpace(norm.NFKC.String(original))
	if normalized == "" || len(normalized) > maxBytes || !utf8.ValidString(normalized) || hasUnsafeProposalTextRune(normalized) {
		return false
	}
	if containsProposalSensitiveAssignment(normalized) {
		return false
	}
	for _, pattern := range proposalCredentialPatterns {
		if pattern.MatchString(normalized) {
			return false
		}
	}
	if containsHighEntropyCredentialRun(normalized) {
		return false
	}
	*value = original
	return true
}

func hasUnsafeProposalTextRune(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) || isExplicitInvisibleRune(char) {
			return true
		}
	}
	return false
}

func isExplicitInvisibleRune(char rune) bool {
	switch {
	case char == '\u034f', char == '\u2800', char == '\u3164', char == '\uffa0':
		return true
	case char >= '\u115f' && char <= '\u1160':
		return true
	case char >= '\u17b4' && char <= '\u17b5':
		return true
	case char >= '\u180b' && char <= '\u180f':
		return true
	case char >= '\ufe00' && char <= '\ufe0f':
		return true
	case char >= '\U000e0100' && char <= '\U000e01ef':
		return true
	default:
		return false
	}
}

func containsProposalSensitiveAssignment(value string) bool {
	var candidate [maxProposalSensitiveLabelRunes]rune
	length := 0
	write := 0
	pendingSensitiveValue := false
	for _, char := range value {
		if pendingSensitiveValue {
			if !unicode.IsSpace(char) {
				return true
			}
			continue
		}
		if char == '=' || char == ':' {
			if proposalSensitiveLabelSuffix(candidate[:], length, write) {
				pendingSensitiveValue = true
			}
			continue
		}
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			candidate[write] = unicode.ToLower(char)
			write = (write + 1) % len(candidate)
			if length < len(candidate) {
				length++
			}
		}
	}
	return false
}

func proposalSensitiveLabelSuffix(candidate []rune, length, write int) bool {
	for _, label := range proposalSensitiveLabels {
		if len(label) > length {
			continue
		}
		start := (write - len(label) + len(candidate)) % len(candidate)
		matched := true
		for index := 0; index < len(label); index++ {
			if candidate[(start+index)%len(candidate)] != rune(label[index]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func containsHighEntropyCredentialRun(value string) bool {
	candidates := strings.FieldsFunc(value, func(char rune) bool {
		return !isCredentialRunRune(char)
	})
	for _, candidate := range candidates {
		candidate = strings.Trim(candidate, ".~_=-")
		if len(candidate) < 32 || !isASCII(candidate) {
			continue
		}
		if len(candidate) >= 40 && isHexCredentialRun(candidate) {
			return true
		}
		classes := credentialRunClasses(candidate)
		entropy := shannonEntropy(candidate)
		if len(candidate) >= 40 && classes >= 3 && entropy >= 3.8 {
			return true
		}
		if len(candidate) >= 48 && classes >= 2 && entropy >= 4.2 {
			return true
		}
	}
	return false
}

func isCredentialRunRune(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
		char == '+' || char == '/' || char == '_' || char == '-' || char == '=' || char == '.' || char == '~'
}

func isASCII(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func isHexCredentialRun(value string) bool {
	for index := 0; index < len(value); index++ {
		char := value[index]
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func credentialRunClasses(value string) int {
	var lower, upper, digit, symbol bool
	for index := 0; index < len(value); index++ {
		switch char := value[index]; {
		case char >= 'a' && char <= 'z':
			lower = true
		case char >= 'A' && char <= 'Z':
			upper = true
		case char >= '0' && char <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	classes := 0
	for _, present := range []bool{lower, upper, digit, symbol} {
		if present {
			classes++
		}
	}
	return classes
}

func shannonEntropy(value string) float64 {
	var frequency [256]int
	for index := 0; index < len(value); index++ {
		frequency[value[index]]++
	}
	entropy := 0.0
	length := float64(len(value))
	for _, count := range frequency {
		if count == 0 {
			continue
		}
		probability := float64(count) / length
		entropy -= probability * math.Log2(probability)
	}
	return entropy
}
