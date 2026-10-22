package apm

import "strings"

// normalizeSQL turns a statement into its shape: string and number literals
// and IN (...) or VALUES lists become ?, whitespace is squeezed, and quoted
// identifiers stay as written. It is a single scan because a trace holds
// thousands of statements and regexp passes cost several times as much.
func normalizeSQL(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	space := false
	for i := 0; i < len(q); {
		c := q[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' {
			space = b.Len() > 0
			i++
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		switch {
		case c == '\'' || c == '"':
			i = stringEnd(q, i)
			b.WriteByte('?')
		case c == '`':
			j := strings.IndexByte(q[i+1:], '`')
			if j < 0 {
				j = len(q)
			} else {
				j += i + 2
			}
			b.WriteString(q[i:j])
			i = j
		case isDigit(c):
			if j, ok := numberEnd(q, i); ok {
				b.WriteByte('?')
				i = j
				break
			}
			j := wordEnd(q, i)
			b.WriteString(q[i:j])
			i = j
		case isWord(c):
			j := wordEnd(q, i)
			b.WriteString(q[i:j])
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := b.String()
	if strings.IndexByte(out, '?') < 0 {
		return out
	}
	return collapseRows(collapseLists(out))
}

func stringEnd(q string, i int) int {
	quote := q[i]
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\\':
			j++
		case quote:
			if j+1 < len(q) && q[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(q)
}

// numberEnd accepts 12, 1.5 and 0xff, but not 1abc or 0xZZ, which are words.
func numberEnd(q string, i int) (int, bool) {
	j := i
	if q[j] == '0' && j+1 < len(q) && (q[j+1] == 'x' || q[j+1] == 'X') {
		k := j + 2
		for k < len(q) && isHex(q[k]) {
			k++
		}
		if k == j+2 || (k < len(q) && isWord(q[k])) {
			return 0, false
		}
		return k, true
	}
	for j < len(q) && isDigit(q[j]) {
		j++
	}
	if j+1 < len(q) && q[j] == '.' && isDigit(q[j+1]) {
		j++
		for j < len(q) && isDigit(q[j]) {
			j++
		}
	}
	if j < len(q) && isWord(q[j]) {
		return 0, false
	}
	return j, true
}

func wordEnd(q string, i int) int {
	j := i + 1
	for j < len(q) && isWord(q[j]) {
		j++
	}
	return j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }

func isAlnum(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isWord(c byte) bool { return isAlnum(c) || c == '_' || c == '$' || c >= 0x80 }

// collapseLists turns ( ?, ?, ? ) into (?).
func collapseLists(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '(' {
			if j := listEnd(s, i); j > 0 {
				b.WriteString("(?)")
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func listEnd(s string, i int) int {
	j := skipSpace(s, i+1)
	if j >= len(s) || s[j] != '?' {
		return -1
	}
	j++
	for {
		j = skipSpace(s, j)
		if j >= len(s) {
			return -1
		}
		switch s[j] {
		case ')':
			return j + 1
		case ',':
			j = skipSpace(s, j+1)
			if j >= len(s) || s[j] != '?' {
				return -1
			}
			j++
		default:
			return -1
		}
	}
}

// collapseRows turns VALUES (?), (?), (?) into VALUES (?).
func collapseRows(s string) string {
	if !strings.Contains(s, "(?)") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], "(?)") {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString("(?)")
		i += 3
		for {
			j := skipSpace(s, i)
			if j >= len(s) || s[j] != ',' {
				break
			}
			j = skipSpace(s, j+1)
			if !strings.HasPrefix(s[j:], "(?)") {
				break
			}
			i = j + 3
		}
	}
	return b.String()
}

func skipSpace(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

// cacheCall is the command and its first key. Values never leave the trace
// detail: a setEx carries the whole cached block.
func cacheCall(s Span) string {
	raw := s.Attrs["redis.raw_command"]
	if raw == "" {
		raw = s.Attrs["db.statement"]
	}
	if raw == "" {
		if s.Resource != "" {
			return s.Resource
		}
		return s.Name
	}
	cmd, rest := nextField(raw)
	key, _ := nextField(rest)
	if key == "" {
		return cmd
	}
	return cmd + " " + key
}

func nextField(s string) (string, string) {
	s = strings.TrimLeft(s, " \t\r\n")
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// normalizeCache keeps the command and the shape of its key: the Magento
// cache id prefix (69d_), hex ids and digits become *.
func normalizeCache(call string) string {
	cmd, rest := nextField(call)
	if i := strings.LastIndexByte(cmd, '.'); i > 0 && hasLower(cmd[:i]) {
		cmd = cmd[i+1:]
	}
	key, _ := nextField(rest)
	if key == "" {
		return cmd
	}
	return cmd + " " + keyShape(key)
}

func cacheCommand(pattern string) string {
	cmd, _ := nextField(pattern)
	return cmd
}

func hasLower(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			return true
		}
	}
	return false
}

func keyShape(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); {
		if !isAlnum(key[i]) {
			b.WriteByte(key[i])
			i++
			continue
		}
		j := i
		for j < len(key) && isAlnum(key[j]) {
			j++
		}
		run := key[i:j]
		switch {
		case hexID(run), idPrefix(key, i, j):
			b.WriteByte('*')
		default:
			digits := false
			for k := 0; k < len(run); k++ {
				if isDigit(run[k]) {
					if !digits {
						b.WriteByte('*')
					}
					digits = true
					continue
				}
				digits = false
				b.WriteByte(run[k])
			}
		}
		i = j
	}
	return b.String()
}

// hexID leaves words such as FACADE alone: a run counts as an id when it has
// a digit, or is too long to be a word.
func hexID(run string) bool {
	digit := false
	for i := 0; i < len(run); i++ {
		if !isHex(run[i]) {
			return false
		}
		if isDigit(run[i]) {
			digit = true
		}
	}
	return digit || len(run) >= 16
}

// Magento prefixes every cache id with three hex characters of an md5 and an
// underscore, which can be all letters (abc_).
func idPrefix(key string, i, j int) bool {
	if j-i != 3 || j >= len(key) || key[j] != '_' || (i > 0 && key[i-1] != ':') {
		return false
	}
	for k := i; k < j; k++ {
		if !isDigit(key[k]) && (key[k] < 'a' || key[k] > 'f') {
			return false
		}
	}
	return true
}

func kindOf(s Span) string {
	c := s.Component
	if c == "" {
		c = classify(s)
	}
	switch c {
	case "sql", "cache", "search", "http":
		return c
	case "magento":
		switch {
		case strings.HasPrefix(s.Name, "magento.plugin"):
			return "plugin"
		case s.Name == "magento.event.execute":
			return "observer"
		case strings.HasPrefix(s.Name, "magento.event"):
			return "event"
		case strings.HasPrefix(s.Name, "magento.block"):
			return "block"
		case strings.HasPrefix(s.Name, "magento.template"):
			return "template"
		}
	}
	return "php"
}

func sqlText(s Span) string {
	if st := first(s.Attrs, "db.statement", "db.query.text"); st != "" {
		return st
	}
	return s.Resource
}

func requestURL(s Span) string {
	u := s.Resource
	if u == "" || !strings.Contains(u, "://") {
		if v := first(s.Attrs, "http.url", "url.full"); v != "" {
			u = v
		}
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// display is the resource the UI shows for a span of this kind.
func display(s Span, kind string) string {
	switch kind {
	case "sql":
		return sqlText(s)
	case "cache":
		return cacheCall(s)
	}
	if s.Resource == "" {
		return s.Name
	}
	return s.Resource
}
