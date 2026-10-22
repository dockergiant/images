package search

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "search", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureHeaders(t *testing.T, name string) http.Header {
	t.Helper()
	h := http.Header{}
	for _, line := range strings.Split(string(fixture(t, name)), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return h
}

func fixtureStores(t *testing.T, name string) map[int]string {
	t.Helper()
	var raw map[string]string
	if err := json.Unmarshal(fixture(t, name), &raw); err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for k, v := range raw {
		id, err := strconv.Atoi(k)
		if err != nil {
			t.Fatal(err)
		}
		out[id] = v
	}
	return out
}

type dockerLine struct {
	Time time.Time
	Line string
}

func fixtureLogLines(t *testing.T, name string) []dockerLine {
	t.Helper()
	var out []dockerLine
	sc := bufio.NewScanner(bytes.NewReader(fixture(t, name)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ts, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		tm, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, dockerLine{Time: tm, Line: rest})
	}
	return out
}
