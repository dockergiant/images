package docker

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestParseLogLine(t *testing.T) {
	l := parseLogLine(2, "2026-10-20T19:04:05.123456789Z 172.20.0.5 - - \"GET / HTTP/1.1\" 200")
	want := time.Date(2026, 10, 20, 19, 4, 5, 123456789, time.UTC)
	if !l.Time.Equal(want) || l.Stream != 2 || l.Line != "172.20.0.5 - - \"GET / HTTP/1.1\" 200" {
		t.Fatalf("got %+v", l)
	}
	l = parseLogLine(1, "no timestamp here")
	if !l.Time.IsZero() || l.Line != "no timestamp here" {
		t.Fatalf("got %+v", l)
	}
	l = parseLogLine(1, "2026-10-20T19:04:05Z")
	if l.Time.IsZero() || l.Line != "" {
		t.Fatalf("bare timestamp %+v", l)
	}
}

func TestDemuxLogsKeepsOrderAndSplitsLines(t *testing.T) {
	var src bytes.Buffer
	src.Write(frame(1, "2026-10-20T19:00:00.000000001Z out one\n2026-10-20T19:00:00.000000002Z out "))
	src.Write(frame(2, "2026-10-20T19:00:00.000000003Z err one\n"))
	src.Write(frame(1, "two\n"))
	src.Write(frame(1, "2026-10-20T19:00:00.000000004Z tail-without-newline"))
	var got []LogLine
	if err := demuxLogs(&src, false, func(l LogLine) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		stream int
		line   string
		nanos  int
	}{{1, "out one", 1}, {2, "err one", 3}, {1, "out two", 2}, {1, "tail-without-newline", 4}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].Stream != w.stream || got[i].Line != w.line || got[i].Time.Nanosecond() != w.nanos {
			t.Errorf("%d: got %+v want %+v", i, got[i], w)
		}
	}
}

func TestDemuxLogsTTY(t *testing.T) {
	src := bytes.NewBufferString("2026-10-20T19:00:00Z a\r\n2026-10-20T19:00:01Z b\n")
	var got []LogLine
	if err := demuxLogs(src, true, func(l LogLine) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Line != "a" || got[1].Line != "b" || got[0].Stream != int(stdcopy.Stdout) {
		t.Fatalf("got %+v", got)
	}
}
