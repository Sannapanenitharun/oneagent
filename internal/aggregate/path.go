package aggregate

import (
	"strings"
)

// Path normalization exists for one reason: cardinality. A request path is
// user-controlled, so /api/orders/1, /api/orders/2 ... /api/orders/900000 are
// 900,000 distinct contexts if taken literally. Aggregating on raw paths would
// use more memory and produce more output than the per-request envelopes it is
// meant to replace, which is the exact failure the whole layer exists to avoid.
//
// The rules below are deliberately conservative — they collapse things that
// are unambiguously identifiers and leave everything else alone. Over-
// normalizing loses real endpoints; under-normalizing is caught by the context
// cap in aggregate.go, which is the backstop.

const (
	// maxSegments bounds how deep a normalized path can be. Paths deeper than
	// this are almost always generated (object storage keys, nested proxies).
	maxSegments = 12
	// maxSegmentLen bounds a single segment in the output, so one absurd
	// segment cannot dominate a label value.
	maxSegmentLen = 64
)

// NormalizePath reduces a request path to a bounded-cardinality template.
func NormalizePath(p string) string {
	if p == "" {
		return "/"
	}

	// A query string is pure cardinality: it never identifies the endpoint.
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "/"
	}

	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	truncated := false
	for _, s := range segs {
		if s == "" {
			continue
		}
		if len(out) >= maxSegments {
			truncated = true
			break
		}
		out = append(out, normalizeSegment(s))
	}

	if len(out) == 0 {
		return "/"
	}
	res := "/" + strings.Join(out, "/")
	if truncated {
		res += "/..."
	}
	return res
}

func normalizeSegment(s string) string {
	switch {
	case isAllDigits(s):
		return "{id}"
	case isUUID(s):
		return "{uuid}"
	case isLongHex(s):
		return "{hash}"
	}
	if len(s) > maxSegmentLen {
		return s[:maxSegmentLen] + "…"
	}
	return s
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isUUID matches the canonical 8-4-4-4-12 hyphenated form. Anything else is
// left alone rather than guessed at.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

// isLongHex catches git SHAs, session tokens, content hashes and the like. The
// 16-character floor keeps ordinary words made only of a-f characters (for
// example "added", "faced", "decade") from being collapsed.
func isLongHex(s string) bool {
	if len(s) < 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
