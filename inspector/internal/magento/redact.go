package magento

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
)

const redacted = "REDACTED"

// The spec anchors the pattern at the end of the name; it also matches at an
// inner underscore so that names such as apikey_live are caught.
var secretName = regexp.MustCompile(`(?i)(^|_)(password|pwd|passwd|secret|token|key|apikey|signature|cert)(_|$)`)

var secretExact = map[string]bool{"username": true, "user": true, "userid": true, "id_salt": true}

func isSecret(name string) bool {
	return secretName.MatchString(name) || secretExact[strings.ToLower(name)]
}

func Redact(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if isSecret(k) {
				out[k] = redacted
			} else {
				out[k] = Redact(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Redact(x)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, x := range t {
			if isSecret(k) {
				out[k] = redacted
			} else {
				out[k] = x
			}
		}
		return out
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer, reflect.Interface:
	default:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil
	}
	return Redact(generic)
}
