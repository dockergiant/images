package apm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	maxBody    = 16 << 20
	maxProfile = 5 << 20
	// The intake servers set no ReadTimeout, and a body read holds a decode slot.
	bodyTimeout = 30 * time.Second
)

type ddSpan struct {
	TraceID  uint64             `msgpack:"trace_id"`
	SpanID   uint64             `msgpack:"span_id"`
	ParentID uint64             `msgpack:"parent_id,omitempty"`
	Start    int64              `msgpack:"start"`
	Duration int64              `msgpack:"duration"`
	Name     string             `msgpack:"name"`
	Resource string             `msgpack:"resource"`
	Service  string             `msgpack:"service"`
	Type     string             `msgpack:"type"`
	Meta     map[string]string  `msgpack:"meta,omitempty"`
	Metrics  map[string]float64 `msgpack:"metrics,omitempty"`
	Error    int32              `msgpack:"error,omitempty"`
}

func NewDDIntake(s *Store, allow func(ip string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r, allow) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		p := r.URL.Path
		switch {
		case p == "/info":
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, http.StatusOK, `{"endpoints":["/v0.4/traces"],"client_drop_p0s":false}`)
		case p == "/v0.4/traces":
			if r.Method != http.MethodPut && r.Method != http.MethodPost {
				w.Header().Set("Allow", "PUT, POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !acquireSlot(r) {
				return
			}
			body, err := readBody(w, r, maxBody)
			if err != nil {
				releaseSlot()
				s.noteRejected("trace payload: " + err.Error())
				return
			}
			traces, err := decodeDD(body)
			releaseSlot()
			if err != nil {
				s.noteRejected("trace payload: " + err.Error())
				http.Error(w, err.Error(), decodeStatus(err))
				return
			}
			kept := 0
			for _, t := range traces {
				if s.Add(t) {
					kept++
				}
			}
			// Bytes count for the traces the store kept, split by trace count, so
			// Inspector's own skipped requests do not inflate the payload per trace.
			if len(traces) > 0 {
				s.noteBody(len(body)*kept/len(traces), kept)
			}
			writeJSON(w, http.StatusOK, `{"rate_by_service":{}}`)
		case strings.HasPrefix(p, "/profiles/"):
			profileHandler(s, w, r, strings.TrimPrefix(p, "/profiles/"))
		case strings.HasPrefix(p, "/telemetry/proxy/"):
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxBody))
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	})
}

func decodeDD(body []byte) ([]Trace, error) {
	if err := checkDD(body); err != nil {
		return nil, fmt.Errorf("decode v0.4 payload: %w", err)
	}
	var chunks [][]ddSpan
	if err := msgpack.Unmarshal(body, &chunks); err != nil {
		return nil, fmt.Errorf("decode v0.4 payload: %w", err)
	}
	out := make([]Trace, 0, len(chunks))
	for _, chunk := range chunks {
		if t, ok := ddTrace(chunk); ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func ddTrace(chunk []ddSpan) (Trace, bool) {
	if len(chunk) == 0 {
		return Trace{}, false
	}
	root := chunk[0]
	for _, s := range chunk {
		if s.ParentID == 0 {
			root = s
			break
		}
	}
	tid := root.Meta["_dd.p.tid"]
	if tid == "" {
		for _, s := range chunk {
			if v := s.Meta["_dd.p.tid"]; v != "" {
				tid = v
				break
			}
		}
	}
	key := traceKey(tid, root.TraceID)
	spans := make([]Span, len(chunk))
	for i, d := range chunk {
		spans[i] = Span{
			TraceID:  key,
			SpanID:   spanHex(d.SpanID),
			ParentID: spanHex(d.ParentID),
			Name:     d.Name,
			Resource: cut(d.Resource),
			Service:  d.Service,
			Type:     d.Type,
			Start:    time.Unix(0, d.Start),
			Duration: time.Duration(d.Duration),
			Error:    d.Error != 0,
			Attrs:    ddAttrs(d),
		}
	}
	return assemble(spans)
}

func ddAttrs(d ddSpan) map[string]string {
	a := make(map[string]string, len(d.Meta)+len(d.Metrics))
	for k, v := range d.Meta {
		// Code origin frames are a dozen keys per span that Inspector never shows.
		if strings.HasPrefix(k, "_dd.code_origin.") {
			continue
		}
		a[k] = cut(v)
	}
	for k, v := range d.Metrics {
		if _, ok := a[k]; !ok {
			a[k] = strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return a
}

func traceKey(tid string, traceID uint64) string {
	tid = strings.ToLower(tid)
	if len(tid) < 16 {
		tid = strings.Repeat("0", 16-len(tid)) + tid
	}
	return tid + fmt.Sprintf("%016x", traceID)
}

func spanHex(id uint64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprintf("%016x", id)
}

func allowed(r *http.Request, allow func(ip string) bool) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return allow != nil && allow(host)
}

// readBody answers a body it cannot read itself; the error says why.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return nil, fmt.Errorf("body of %d bytes is over the %d byte limit", r.ContentLength, limit)
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return nil, fmt.Errorf("body is over the %d byte limit", limit)
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, fmt.Errorf("reading the body: %w", err)
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}
