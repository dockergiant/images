package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	maxArg       = 256
	maxLine      = 1 << 20
	foldWindow   = 5 * time.Millisecond
	inspectorTag = "roll-inspector"
)

var errMonitorLine = errors.New("not a MONITOR line")

func ParseMonitor(line string) (core.RedisCmd, error) {
	var c core.RedisCmd
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		return c, errMonitorLine
	}
	ts, err := parseStamp(line[:sp])
	if err != nil {
		return c, err
	}
	rest := line[sp+1:]
	if !strings.HasPrefix(rest, "[") {
		return c, errMonitorLine
	}
	head := rest
	if q := strings.IndexByte(rest, '"'); q >= 0 {
		head = rest[:q]
	}
	end := strings.LastIndex(head, "] ")
	if end < 0 {
		return c, errMonitorLine
	}
	dbText, addr, ok := strings.Cut(rest[1:end], " ")
	if !ok {
		return c, errMonitorLine
	}
	db, err := strconv.Atoi(dbText)
	if err != nil {
		return c, errMonitorLine
	}
	args := parseArgs(rest[end+2:])
	if len(args) == 0 {
		return c, errMonitorLine
	}
	c.Time = ts
	c.DB = db
	c.Addr = addr
	c.IP, c.Port = splitAddr(addr)
	c.Cmd = strings.ToUpper(args[0])
	if len(args) > 1 {
		c.Args = args[1:]
	}
	return c, nil
}

func parseStamp(s string) (time.Time, error) {
	secText, usecText, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secText, 10, 64)
	if err != nil {
		return time.Time{}, errMonitorLine
	}
	var usec int64
	if usecText != "" {
		if len(usecText) > 6 {
			usecText = usecText[:6]
		}
		usecText += strings.Repeat("0", 6-len(usecText))
		usec, err = strconv.ParseInt(usecText, 10, 64)
		if err != nil {
			return time.Time{}, errMonitorLine
		}
	}
	return time.Unix(sec, usec*1000).UTC(), nil
}

func splitAddr(addr string) (string, int) {
	if addr == "lua" || strings.HasPrefix(addr, "unix:") {
		return "", 0
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(portText)
	return host, port
}

// parseArgs undoes sdscatrepr quoting. Arguments that are not printable UTF-8
// after unescaping keep their escaped form, because the JSON encoder would
// otherwise replace their bytes.
func parseArgs(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		if s[i] == ' ' {
			i++
			continue
		}
		if s[i] != '"' {
			break
		}
		i++
		var b strings.Builder
		start := i
		for i < len(s) && s[i] != '"' {
			if s[i] != '\\' || i+1 >= len(s) {
				b.WriteByte(s[i])
				i++
				continue
			}
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'x':
				if i+3 < len(s) {
					if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
						b.WriteByte(byte(v))
						i += 4
						continue
					}
				}
				b.WriteString(`\x`)
			default:
				b.WriteByte(s[i+1])
			}
			i += 2
		}
		raw := b.String()
		if printable(raw) {
			out = append(out, raw)
		} else {
			out = append(out, s[start:i])
		}
		i++
	}
	return out
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

func skip(c core.RedisCmd, ownIP string) bool {
	if ownIP != "" && c.IP == ownIP {
		return true
	}
	if c.Cmd == "PING" && c.IP == "127.0.0.1" {
		return true
	}
	return c.Cmd == "ECHO" && len(c.Args) > 0 && strings.HasPrefix(c.Args[0], "phpredis:")
}

func truncateArgs(c core.RedisCmd) core.RedisCmd {
	var args []string
	for i, a := range c.Args {
		if i == 0 || len(a) <= maxArg {
			continue
		}
		if args == nil {
			args = append([]string(nil), c.Args...)
		}
		cut := maxArg
		for cut > 0 && !utf8.RuneStart(a[cut]) {
			cut--
		}
		args[i] = fmt.Sprintf("%s… (%d bytes)", a[:cut], len(a))
	}
	if args != nil {
		c.Args = args
	}
	return c
}

var foldable = map[string]bool{"SET": true, "SETEX": true, "SADD": true, "DEL": true, "EXPIRE": true, "UNLINK": true}

type burst struct {
	cmd  core.RedisCmd
	last time.Time
	n    int
}

func (b *burst) done() core.RedisCmd {
	c := b.cmd
	if b.n > 1 {
		c.Folded = b.n
	}
	return c
}

type folder struct {
	pending map[string]*burst
}

func newFolder() *folder {
	return &folder{pending: map[string]*burst{}}
}

func (f *folder) Add(c core.RedisCmd) []core.RedisCmd {
	out := f.Flush(c.Time)
	b := f.pending[c.Addr]
	if !foldable[c.Cmd] {
		if b != nil {
			out = append(out, b.done())
			delete(f.pending, c.Addr)
		}
		return append(out, c)
	}
	if b != nil && c.Time.Sub(b.last) <= foldWindow {
		b.n++
		b.last = c.Time
		return out
	}
	if b != nil {
		out = append(out, b.done())
	}
	f.pending[c.Addr] = &burst{cmd: c, last: c.Time, n: 1}
	return out
}

func (f *folder) Flush(now time.Time) []core.RedisCmd {
	var out []core.RedisCmd
	for addr, b := range f.pending {
		if now.Sub(b.last) > foldWindow {
			out = append(out, b.done())
			delete(f.pending, addr)
		}
	}
	sortByTime(out)
	return out
}

func (f *folder) FlushAll() []core.RedisCmd {
	var out []core.RedisCmd
	for addr, b := range f.pending {
		out = append(out, b.done())
		delete(f.pending, addr)
	}
	sortByTime(out)
	return out
}

func sortByTime(cmds []core.RedisCmd) {
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Time.Before(cmds[j].Time) })
}

type monitorConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialMonitor(ctx context.Context, addr string) (*monitorConn, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	m := &monitorConn{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, resp("CLIENT", "SETNAME", inspectorTag)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := m.readLine(); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := io.WriteString(conn, resp("MONITOR")); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := m.readLine()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if line != "+OK" {
		conn.Close()
		return nil, fmt.Errorf("MONITOR refused: %s", strings.TrimPrefix(line, "-"))
	}
	_ = conn.SetDeadline(time.Time{})
	return m, nil
}

func resp(args ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b.String()
}

func (m *monitorConn) ownIP() string {
	if a, ok := m.conn.LocalAddr().(*net.TCPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// readLine caps a line at maxLine and drops the rest, so a multi-megabyte SET
// seen through MONITOR cannot grow the buffer without bound.
func (m *monitorConn) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := m.r.ReadSlice('\n')
		if len(buf) < maxLine {
			room := maxLine - len(buf)
			if len(chunk) < room {
				room = len(chunk)
			}
			buf = append(buf, chunk[:room]...)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
	return strings.TrimRight(string(buf), "\r\n"), nil
}

func (m *monitorConn) Close() error {
	return m.conn.Close()
}

func glob(pat, s string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			for len(pat) > 0 && pat[0] == '*' {
				pat = pat[1:]
			}
			if pat == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if glob(pat, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			_, n := utf8.DecodeRuneInString(s)
			s, pat = s[n:], pat[1:]
		case '[':
			if s == "" {
				return false
			}
			r, n := utf8.DecodeRuneInString(s)
			ok, rest, valid := matchClass(pat[1:], r)
			if !valid {
				if s[0] != '[' {
					return false
				}
				s, pat = s[1:], pat[1:]
				continue
			}
			if !ok {
				return false
			}
			s, pat = s[n:], rest
		case '\\':
			if len(pat) > 1 {
				pat = pat[1:]
			}
			fallthrough
		default:
			if s == "" || s[0] != pat[0] {
				return false
			}
			s, pat = s[1:], pat[1:]
		}
	}
	return s == ""
}

func matchClass(pat string, r rune) (bool, string, bool) {
	negate := false
	if strings.HasPrefix(pat, "^") {
		negate = true
		pat = pat[1:]
	}
	matched := false
	first := true
	for {
		if pat == "" {
			return false, "", false
		}
		if pat[0] == ']' && !first {
			return matched != negate, pat[1:], true
		}
		first = false
		if pat[0] == '\\' && len(pat) > 1 {
			pat = pat[1:]
		}
		lo, n := utf8.DecodeRuneInString(pat)
		pat = pat[n:]
		hi := lo
		if len(pat) > 1 && pat[0] == '-' && pat[1] != ']' {
			pat = pat[1:]
			if pat[0] == '\\' && len(pat) > 1 {
				pat = pat[1:]
			}
			hi, n = utf8.DecodeRuneInString(pat)
			pat = pat[n:]
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		if r >= lo && r <= hi {
			matched = true
		}
	}
}
