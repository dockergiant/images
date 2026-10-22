package apm

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func NewOTLPIntake(s *Store, allow func(ip string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r, allow) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/v1/traces" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctype, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ctype != "application/json" && ctype != "application/x-protobuf" && ctype != "application/protobuf" {
			http.Error(w, "content type must be application/x-protobuf or application/json", http.StatusUnsupportedMediaType)
			return
		}
		// OTLP/JSON decodes through map[string]any at many times its size, and
		// nothing in RollDev posts it, so it gets a smaller cap than protobuf.
		limit := int64(maxBody)
		if ctype == "application/json" {
			limit = maxJSONBody
		}
		if !acquireSlot(r) {
			return
		}
		body, err := readBody(w, r, limit)
		if err != nil {
			releaseSlot()
			s.noteRejected("OTLP payload: " + err.Error())
			return
		}
		traces, err := decodeOTLP(body, ctype)
		releaseSlot()
		if err != nil {
			s.noteRejected("OTLP payload: " + err.Error())
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
		if ctype == "application/json" {
			writeJSON(w, http.StatusOK, "{}")
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	})
}

// TracesData has the same wire shape as ExportTraceServiceRequest (field 1, resource_spans),
// which keeps the collector package and its grpc dependencies out of the binary.
func decodeOTLP(body []byte, ctype string) ([]Trace, error) {
	var data *tracev1.TracesData
	var err error
	if ctype == "application/json" {
		data, err = parseOTLPJSON(body)
	} else if err = checkOTLP(body); err == nil {
		data = &tracev1.TracesData{}
		err = proto.Unmarshal(body, data)
	}
	if err != nil {
		return nil, fmt.Errorf("decode otlp: %w", err)
	}
	return otlpTraces(data), nil
}

// OTLP/JSON writes trace and span ids as hex, while protojson expects base64 for bytes fields.
func parseOTLPJSON(body []byte) (*tracev1.TracesData, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	hexIDsToBase64(doc)
	fixed, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	data := &tracev1.TracesData{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(fixed, data); err != nil {
		return nil, err
	}
	return data, nil
}

var idFields = map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true, "trace_id": true, "span_id": true, "parent_span_id": true}

func hexIDsToBase64(v any) {
	switch n := v.(type) {
	case map[string]any:
		for k, val := range n {
			if s, ok := val.(string); ok && idFields[k] {
				if b, err := hex.DecodeString(s); err == nil && s != "" {
					n[k] = base64.StdEncoding.EncodeToString(b)
				}
				continue
			}
			hexIDsToBase64(val)
		}
	case []any:
		for _, val := range n {
			hexIDsToBase64(val)
		}
	}
}

func otlpTraces(data *tracev1.TracesData) []Trace {
	groups := map[string][]Span{}
	var order []string
	for _, rs := range data.GetResourceSpans() {
		service := ""
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.GetKey() == "service.name" {
				service = anyString(kv.GetValue())
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				key := otlpKey(sp.GetTraceId())
				if _, seen := groups[key]; !seen {
					order = append(order, key)
				}
				groups[key] = append(groups[key], otlpSpan(key, service, sp))
			}
		}
	}
	out := make([]Trace, 0, len(order))
	for _, key := range order {
		if t, ok := assemble(groups[key]); ok {
			out = append(out, t)
		}
	}
	return out
}

func otlpKey(id []byte) string {
	h := hex.EncodeToString(id)
	if len(h) < 32 {
		h = strings.Repeat("0", 32-len(h)) + h
	}
	return h
}

func otlpSpan(key, service string, sp *tracev1.Span) Span {
	attrs := make(map[string]string, len(sp.GetAttributes())+2)
	for _, kv := range sp.GetAttributes() {
		attrs[kv.GetKey()] = cut(anyString(kv.GetValue()))
	}
	if k := otlpKind(sp.GetKind()); k != "" {
		attrs["otel.kind"] = k
	}
	if msg := sp.GetStatus().GetMessage(); msg != "" {
		attrs["otel.status_message"] = cut(msg)
	}
	start, end := sp.GetStartTimeUnixNano(), sp.GetEndTimeUnixNano()
	var dur time.Duration
	if end > start {
		dur = time.Duration(end - start)
	}
	resource := first(attrs, "db.statement", "db.query.text", "http.route")
	if resource == "" {
		resource = sp.GetName()
	}
	parent := ""
	if p := sp.GetParentSpanId(); len(p) > 0 && !allZero(p) {
		parent = hex.EncodeToString(p)
	}
	return Span{
		TraceID:  key,
		SpanID:   hex.EncodeToString(sp.GetSpanId()),
		ParentID: parent,
		Name:     sp.GetName(),
		Resource: cut(resource),
		Service:  service,
		Type:     otlpType(attrs),
		Start:    time.Unix(0, int64(start)),
		Duration: dur,
		Error:    sp.GetStatus().GetCode() == tracev1.Status_STATUS_CODE_ERROR,
		Attrs:    attrs,
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func otlpKind(k tracev1.Span_SpanKind) string {
	switch k {
	case tracev1.Span_SPAN_KIND_SERVER:
		return "server"
	case tracev1.Span_SPAN_KIND_CLIENT:
		return "client"
	case tracev1.Span_SPAN_KIND_PRODUCER:
		return "producer"
	case tracev1.Span_SPAN_KIND_CONSUMER:
		return "consumer"
	case tracev1.Span_SPAN_KIND_INTERNAL:
		return "internal"
	}
	return ""
}

func otlpType(a map[string]string) string {
	if sys := strings.ToLower(a["db.system"]); sys != "" {
		switch {
		case sys == "memcached":
			return "memcached"
		case cacheSystems[sys]:
			return "redis"
		case searchSystems[sys]:
			return sys
		}
		return "sql"
	}
	if a["messaging.system"] != "" {
		return "queue"
	}
	switch a["otel.kind"] {
	case "server":
		return "web"
	case "client":
		if first(a, "url.full", "http.url", "http.request.method", "http.method") != "" {
			return "http"
		}
	}
	return ""
}

func anyString(v *commonv1.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonv1.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *commonv1.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			parts = append(parts, anyString(e))
		}
		b, _ := json.Marshal(parts)
		return string(b)
	case *commonv1.AnyValue_KvlistValue:
		m := map[string]string{}
		for _, kv := range x.KvlistValue.GetValues() {
			m[kv.GetKey()] = anyString(kv.GetValue())
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	return ""
}
