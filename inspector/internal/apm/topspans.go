package apm

import (
	"cmp"
	"slices"
)

// TopSpans keeps the n spans with the most self time, in their trace order,
// so a client that cannot take a 7,000-span trace still gets the costly part.
// n <= 0 keeps every span.
func TopSpans(t Trace, n int) []Span {
	if n <= 0 || len(t.Spans) <= n {
		return append([]Span{}, t.Spans...)
	}
	self, _ := selfTimes(t.Spans)
	order := make([]int, len(t.Spans))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(self[b], self[a]) })
	keep := order[:n]
	slices.Sort(keep)
	out := make([]Span, n)
	for i, j := range keep {
		out[i] = t.Spans[j]
	}
	return out
}
