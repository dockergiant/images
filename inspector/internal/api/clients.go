package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	domainPlaceholder = "__ROLL_DOMAIN__"
	zipRoot           = "roll-inspector"
)

// A fixed time keeps the zip byte-identical across builds and restarts.
var zipTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

var tokensRe = regexp.MustCompile(`:root\{[^}]*--ground:[^}]*\}`)

func (s *Server) handleUserscript(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Clients == nil {
		writeHint(w, http.StatusNotFound, "this Inspector build carries no userscript", "rebuild the image")
		return
	}
	src, err := fs.ReadFile(s.cfg.Clients, "userscript/roll-inspector.user.js")
	if err != nil {
		writeHint(w, http.StatusNotFound, "this Inspector build carries no userscript", "rebuild the image")
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(bytes.ReplaceAll(src, []byte(domainPlaceholder), []byte(s.cfg.Domain)))
}

func (s *Server) handleExtension(w http.ResponseWriter, r *http.Request) {
	s.zipOnce.Do(func() { s.zipData, s.zipErr = buildExtensionZip(s.cfg.Clients, s.cfg.UI) })
	if s.zipErr != nil {
		writeHint(w, http.StatusServiceUnavailable, "the extension zip cannot be built: "+s.zipErr.Error(), "rebuild the image with scripts/ui and the Dockerfile")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="roll-inspector.zip"`)
	_, _ = w.Write(s.zipData)
}

// buildExtensionZip packs the extension with the UI's fonts and colour
// tokens, taken from the embedded UI build, so the panel looks like the UI
// without a second copy of either in git.
func buildExtensionZip(clients, ui fs.FS) ([]byte, error) {
	if clients == nil || ui == nil {
		return nil, fmt.Errorf("no embedded files")
	}
	files := map[string][]byte{}
	err := fs.WalkDir(clients, "extension", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(clients, p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, "extension/")] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("extension files: %w", err)
	}
	if _, ok := files["manifest.json"]; !ok {
		return nil, fmt.Errorf("extension/manifest.json is missing")
	}
	fonts, err := fs.Glob(ui, "fonts/*.woff2")
	if err != nil || len(fonts) == 0 {
		return nil, fmt.Errorf("the UI build has no fonts/*.woff2")
	}
	for _, f := range fonts {
		data, err := fs.ReadFile(ui, f)
		if err != nil {
			return nil, err
		}
		files["fonts/"+path.Base(f)] = data
	}
	css, err := fs.ReadFile(ui, "app.css")
	if err != nil {
		return nil, fmt.Errorf("the UI build has no app.css: %w", err)
	}
	tokens := tokensRe.Find(css)
	if tokens == nil {
		return nil, fmt.Errorf("app.css has no :root block with the design tokens")
	}
	files["tokens.css"] = append(append([]byte{}, tokens...), '\n')

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		h := &zip.FileHeader{Name: zipRoot + "/" + n, Method: zip.Deflate, Modified: zipTime}
		h.SetMode(0o644)
		f, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
