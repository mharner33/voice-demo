package stt

import (
	"strings"
	"unicode"
)

// WordErrorRate compares a hypothesis against a reference transcript, as the
// edit distance between them in words divided by the reference's length.
//
// This is the right comparison for a real recognizer, and exact string match
// is the wrong one: a recognizer that hears "four seven two nine" as "4729" or
// drops a "the" has not failed, and a test that demanded equality would fail
// on every API improvement. A threshold on this figure tests what the demo
// actually needs — that the transcript is close enough for the agent to act on.
//
// The rate can exceed 1 when the hypothesis invents more words than the
// reference contains. An empty reference gives 0 against an empty hypothesis
// and 1 against anything else, since there is nothing to divide by.
func WordErrorRate(reference, hypothesis string) float64 {
	ref := normalizeWords(reference)
	hyp := normalizeWords(hypothesis)

	if len(ref) == 0 {
		if len(hyp) == 0 {
			return 0
		}
		return 1
	}
	return float64(editDistance(ref, hyp)) / float64(len(ref))
}

// normalizeWords lowercases and splits on anything that is not a letter, a
// digit, or an intra-word apostrophe. Punctuation and capitalization are the
// recognizer's formatting choices, not transcription errors, so counting them
// would measure the wrong thing.
func normalizeWords(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// editDistance is the Levenshtein distance between two word sequences, which
// counts substitutions, deletions and insertions — the three error types a
// word error rate is defined over.
func editDistance(a, b []string) int {
	// Only the previous row is needed, so the table is two rows rather than
	// len(a)+1 of them.
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution or match
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
