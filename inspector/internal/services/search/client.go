package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	opaqueID       = "roll-inspector"
	requestTimeout = 10 * time.Second
	maxResponse    = 64 << 20
)

type client struct {
	base string
	http *http.Client
}

type statusError struct {
	Status int
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("search engine answered %d: %s", e.Status, e.Body)
}

func isStatus(err error, codes ...int) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.Status == c {
			return true
		}
	}
	return false
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: requestTimeout}
}

func (c *client) do(ctx context.Context, method, path string, body any) ([]byte, http.Header, error) {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			rd = bytes.NewReader(b)
		case json.RawMessage:
			rd = bytes.NewReader(b)
		default:
			data, err := json.Marshal(body)
			if err != nil {
				return nil, nil, err
			}
			rd = bytes.NewReader(data)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("X-Opaque-Id", opaqueID)
	req.Header.Set("Accept", "application/json")
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, resp.Header, err
	}
	if resp.StatusCode >= 300 {
		return data, resp.Header, &statusError{Status: resp.StatusCode, Body: clip(strings.TrimSpace(string(data)), 512)}
	}
	return data, resp.Header, nil
}

func (c *client) getJSON(ctx context.Context, path string, out any) error {
	data, _, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
