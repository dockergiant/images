package varnish

import "testing"

func TestQuoteCLI(t *testing.T) {
	cases := []struct {
		in, want string
		fail     bool
	}{
		{in: `\.html$`, want: `"\\.html$"`},
		{in: `a b`, want: `"a b"`},
		{in: `say "hi"`, want: `"say \"hi\""`},
		{in: `back\slash "and" quote`, want: `"back\\slash \"and\" quote"`},
		{in: `^/catalog/(product|category)/`, want: `"^/catalog/(product|category)/"`},
		{in: ``, want: `""`},
		{in: "line\nbreak", fail: true},
		{in: "carriage\rreturn", fail: true},
		{in: "nul\x00byte", fail: true},
		{in: "tab\there", fail: true},
		{in: "del\x7f", fail: true},
		{in: "c1\u0085", fail: true},
		{in: "bad\xff", fail: true},
	}
	for _, c := range cases {
		got, err := QuoteCLI(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("QuoteCLI(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("QuoteCLI(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}
