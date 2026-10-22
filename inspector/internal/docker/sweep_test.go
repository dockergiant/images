package docker

import (
	"strings"
	"testing"
)

func TestSweepSpecIsFixed(t *testing.T) {
	a := sweepSpec("abc123")
	b := sweepSpec(`"; rm -rf / #`)
	if len(a.Cmd) != 3 || a.Cmd[0] != "sh" || a.Cmd[1] != "-c" || a.Cmd[2] != sweepScript {
		t.Fatalf("cmd %q", a.Cmd)
	}
	if strings.Join(a.Cmd, "\x00") != strings.Join(b.Cmd, "\x00") {
		t.Fatal("container id leaked into the command")
	}
	if b.Container != `"; rm -rf / #` || a.User != "root" || !a.Privileged || a.Stdin != nil || len(a.Env) != 0 {
		t.Fatalf("spec %+v", a)
	}
}

func TestSweepScriptContent(t *testing.T) {
	for _, bad := range []string{"%", "{{", "$1", "$@", "$*"} {
		if strings.Contains(sweepScript, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	for _, want := range []string{"/proc/[0-9]*", "environ", "grep -qx 'ROLL_INSPECTOR_EXEC=1'", "kill -TERM", "$$", "$PPID", "Z|X"} {
		if !strings.Contains(sweepScript, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if !strings.Contains(sweepScript, ExecMarker) {
		t.Error("script does not match the exec marker")
	}
}
