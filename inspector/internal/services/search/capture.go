package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var errNothingToCapture = errors.New("no index to capture: create the Magento indices first with roll magento indexer:reindex")

const (
	markerIndex     = ".roll-inspector"
	captureKey      = "search.capture"
	captureMax      = 30 * time.Minute
	thresholdPrefix = "index.search.slowlog.threshold."
)

// The warn tier is the one that reaches the console whatever level the
// slowlog loggers are configured at, so a 0 ms threshold there logs every query.
var captureSettings = []string{
	"index.search.slowlog.threshold.query.warn",
	"index.search.slowlog.threshold.fetch.warn",
}

type marker struct {
	Index   string         `json:"index"`
	Prior   map[string]any `json:"prior"`
	Started time.Time      `json:"started"`
}

func indexPath(name string) string {
	return "/" + url.PathEscape(name)
}

func markerPath(index string) string {
	return "/" + markerIndex + "/_doc/" + url.PathEscape(index) + "?refresh=true"
}

func (c *client) catalog(ctx context.Context) ([]CatIndex, []CatAlias, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/_cat/indices?format=json&bytes=b&time=ms&h=health,status,index,uuid,pri,rep,docs.count,docs.deleted,store.size,creation.date", nil)
	if err != nil {
		return nil, nil, err
	}
	ix, err := ParseCatIndices(data)
	if err != nil {
		return nil, nil, err
	}
	data, _, err = c.do(ctx, http.MethodGet, "/_cat/aliases?format=json", nil)
	if err != nil {
		return nil, nil, err
	}
	al, err := ParseCatAliases(data)
	return ix, al, err
}

func (c *client) thresholds(ctx context.Context, index string) (map[string]any, error) {
	var resp map[string]struct {
		Settings map[string]any `json:"settings"`
	}
	if err := c.getJSON(ctx, indexPath(index)+"/_settings/"+thresholdPrefix+"*?flat_settings=true", &resp); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, v := range resp {
		for k, s := range v.Settings {
			if strings.HasPrefix(k, thresholdPrefix) {
				out[k] = s
			}
		}
	}
	for _, k := range captureSettings {
		if _, ok := out[k]; !ok {
			out[k] = nil
		}
	}
	return out, nil
}

func (c *client) putSettings(ctx context.Context, index string, settings map[string]any) error {
	_, _, err := c.do(ctx, http.MethodPut, indexPath(index)+"/_settings", settings)
	return err
}

func (c *client) ensureMarkerIndex(ctx context.Context) error {
	settings := map[string]any{"index.number_of_shards": 1, "index.number_of_replicas": 0, "index.hidden": true}
	_, _, err := c.do(ctx, http.MethodPut, "/"+markerIndex, map[string]any{"settings": settings})
	if isStatus(err, http.StatusBadRequest) && strings.Contains(err.Error(), "index.hidden") {
		delete(settings, "index.hidden")
		_, _, err = c.do(ctx, http.MethodPut, "/"+markerIndex, map[string]any{"settings": settings})
	}
	if isStatus(err, http.StatusBadRequest) && strings.Contains(err.Error(), "resource_already_exists") {
		return nil
	}
	return err
}

func (c *client) getMarker(ctx context.Context, index string) (marker, bool, error) {
	var resp struct {
		Found  bool   `json:"found"`
		Source marker `json:"_source"`
	}
	err := c.getJSON(ctx, "/"+markerIndex+"/_doc/"+url.PathEscape(index), &resp)
	if isStatus(err, http.StatusNotFound) {
		return marker{}, false, nil
	}
	if err != nil || !resp.Found {
		return marker{}, false, err
	}
	if resp.Source.Index == "" {
		resp.Source.Index = index
	}
	return resp.Source, true, nil
}

func (c *client) putMarker(ctx context.Context, m marker) error {
	if err := c.ensureMarkerIndex(ctx); err != nil {
		return fmt.Errorf("create %s: %w", markerIndex, err)
	}
	_, _, err := c.do(ctx, http.MethodPut, markerPath(m.Index), m)
	return err
}

func (c *client) deleteMarker(ctx context.Context, index string) error {
	_, _, err := c.do(ctx, http.MethodDelete, markerPath(index), nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func restoreIndex(ctx context.Context, c *client, m marker) error {
	prior := map[string]any{}
	for k, v := range m.Prior {
		prior[k] = v
	}
	for _, k := range captureSettings {
		if _, ok := prior[k]; !ok {
			prior[k] = nil
		}
	}
	if err := c.putSettings(ctx, m.Index, prior); err != nil && !isStatus(err, http.StatusNotFound) {
		return fmt.Errorf("restore %s: %w", m.Index, err)
	}
	return c.deleteMarker(ctx, m.Index)
}

func reconcile(ctx context.Context, c *client) (int, error) {
	var resp struct {
		Hits struct {
			Hits []struct {
				ID     string `json:"_id"`
				Source marker `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	err := c.getJSON(ctx, "/"+markerIndex+"/_search?size=1000", &resp)
	if isStatus(err, http.StatusNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, h := range resp.Hits.Hits {
		m := h.Source
		if m.Index == "" {
			m.Index = h.ID
		}
		if err := restoreIndex(ctx, c, m); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// mu is held across the HTTP calls so a sync and a restore never interleave:
// a capture applied after the restore started would stay at 0 ms.
type captureSession struct {
	c       *client
	now     func() time.Time
	started time.Time

	mu       sync.Mutex
	done     bool
	captured []string
	markers  map[string]marker
}

func newCaptureSession(c *client, now func() time.Time) *captureSession {
	return &captureSession{c: c, now: now, started: now(), markers: map[string]marker{}}
}

func (s *captureSession) indices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.captured...)
}

func (s *captureSession) begin(ctx context.Context) error {
	if err := s.sync(ctx); err != nil {
		return err
	}
	if len(s.indices()) == 0 {
		return errNothingToCapture
	}
	return nil
}

func (s *captureSession) sync(ctx context.Context) error {
	ix, al, err := s.c.catalog(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	have := map[string]bool{}
	for _, x := range s.captured {
		have[x] = true
	}
	var errs []error
	for _, target := range captureTargets(ix, al) {
		if have[target] {
			continue
		}
		if err := s.apply(ctx, target); err != nil {
			errs = append(errs, fmt.Errorf("capture %s: %w", target, err))
			continue
		}
		s.captured = append(s.captured, target)
	}
	return errors.Join(errs...)
}

func (s *captureSession) apply(ctx context.Context, index string) error {
	m, found, err := s.c.getMarker(ctx, index)
	if err != nil {
		return err
	}
	if !found {
		prior, err := s.c.thresholds(ctx, index)
		if err != nil {
			return err
		}
		m = marker{Index: index, Prior: prior, Started: s.now().UTC()}
		if err := s.c.putMarker(ctx, m); err != nil {
			return err
		}
	}
	s.markers[index] = m
	zero := map[string]any{}
	for _, k := range captureSettings {
		zero[k] = "0ms"
	}
	if err := s.c.putSettings(ctx, index, zero); err != nil {
		if !found {
			// The server may have applied the PUT before the client gave up, and a
			// bare marker delete would then lose the only copy of the prior values.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
			defer cancel()
			if rerr := restoreIndex(rctx, s.c, m); rerr != nil {
				return errors.Join(err, rerr)
			}
		}
		return err
	}
	return nil
}

func (s *captureSession) restore(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	var errs []error
	for _, index := range s.captured {
		m, found, err := s.c.getMarker(ctx, index)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found {
			// reconcile already restored this index and removed its marker; an
			// empty marker would reset thresholds the developer had set.
			if m, found = s.markers[index]; !found {
				continue
			}
		}
		if err := restoreIndex(ctx, s.c, m); err != nil {
			errs = append(errs, err)
		}
	}
	// An apply cut short by the end of the session can leave a marker for an
	// index that never reached s.captured.
	if _, err := reconcile(ctx, s.c); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
