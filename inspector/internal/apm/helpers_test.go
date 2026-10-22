package apm

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "apm", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func allowAll(string) bool { return true }

func spanByName(tr Trace, name string) *Span {
	for i := range tr.Spans {
		if tr.Spans[i].Name == name {
			return &tr.Spans[i]
		}
	}
	return nil
}

func rootOf(tr Trace) *Span {
	for i := range tr.Spans {
		if tr.Spans[i].ParentID == "" {
			return &tr.Spans[i]
		}
	}
	return nil
}
