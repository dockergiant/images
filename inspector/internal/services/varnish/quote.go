package varnish

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	errControlChar = errors.New("value contains a control character")
	errInvalidUTF8 = errors.New("value is not valid UTF-8")
)

func QuoteCLI(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errInvalidUTF8
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", errControlChar
		}
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String(), nil
}
