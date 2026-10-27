package studio

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// This is a guard for explicit, supported constraints, not a general language
// understanding guarantee. Ambiguous multi-city briefs require clarification.
func ValidateBriefSegment(brief string, segment Segment) error {
	normalized := strings.Map(func(r rune) rune {
		if r == 'İ' || r == 'ı' {
			return 'i'
		}
		return unicode.ToLower(r)
	}, brief)
	cities := []struct{ word, city string }{{"istanbul", "İstanbul"}, {"ankara", "Ankara"}, {"izmir", "İzmir"}}
	found := ""
	for _, c := range cities {
		if regexp.MustCompile(`(^|[^\pL])` + c.word + `([^\pL]|$)`).MatchString(normalized) {
			if found != "" {
				return fmt.Errorf("Brief birden fazla şehir içeriyor; tek hedef şehri açıkça belirtin")
			}
			found = c.city
		}
	}
	if found != "" && found != segment.City {
		return fmt.Errorf("Brief/segment uyuşmazlığı: istenen şehir %s, önerilen %s", found, segment.City)
	}
	matches := regexp.MustCompile(`\b([0-9]+)\s*g[üu]n`).FindAllStringSubmatch(normalized, -1)
	var days int
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return fmt.Errorf("Brief gün aralığı geçersiz")
		}
		if days != 0 && days != n {
			return fmt.Errorf("Brief birden fazla gün aralığı içeriyor")
		}
		days = n
	}
	if len(matches) > 0 && days != segment.Days {
		return fmt.Errorf("Brief/segment uyuşmazlığı: istenen gün %d, önerilen %d", days, segment.Days)
	}
	return nil
}

// Providers must explicitly promise stable-key deduplication across retries.
// Receipt lookup alone is not proof that a not-found receipt cannot appear later.
type IdempotentProvider interface{ SupportsIdempotency() bool }

func safeToResend(p Provider) bool {
	s, ok := p.(IdempotentProvider)
	return ok && s.SupportsIdempotency()
}
