package tui

import (
	"strings"
	"unicode"
)

// fuzzyScore returns -1 if every rune of q doesn't appear in s in order.
// Higher is better: consecutive runs and word starts score extra.
func fuzzyScore(q, s string) int {
	q, s = strings.ToLower(q), strings.ToLower(s)
	if q == "" {
		return 0
	}
	if i := strings.Index(s, q); i >= 0 {
		return 1000 - i // substring beats scattered matches
	}
	score, run, qi := 0, 0, 0
	qr := []rune(q)
	prev := ' '
	for _, r := range s {
		if qi < len(qr) && r == qr[qi] {
			qi++
			run++
			score += run
			if !unicode.IsLetter(prev) && !unicode.IsDigit(prev) {
				score += 5
			}
		} else {
			run = 0
		}
		prev = r
	}
	if qi < len(qr) {
		return -1
	}
	return score
}
