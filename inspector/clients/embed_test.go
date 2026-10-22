package clients

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedClients(t *testing.T) {
	us, err := fs.ReadFile(Files, "userscript/roll-inspector.user.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(us), "@match        https://*.__ROLL_DOMAIN__/*") {
		t.Fatal("the userscript template lost its domain placeholder")
	}
	raw, err := fs.ReadFile(Files, "extension/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version      int    `json:"manifest_version"`
		DevtoolsPage string `json:"devtools_page"`
		OptionsPage  string `json:"options_page"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{m.DevtoolsPage, m.OptionsPage, "panel.html", "panel.js", "panel.css", "lib/core.js"} {
		if _, err := fs.Stat(Files, "extension/"+f); err != nil {
			t.Errorf("extension/%s: %v", f, err)
		}
	}
}
