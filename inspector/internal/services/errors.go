package services

import "net/http"

// ErrorBody is the {error, hint} body every adapter route answers with, so a
// client of /api/v1 that shows the hint has something to show.
func ErrorBody(status int, msg string) map[string]string {
	return map[string]string{"error": msg, "hint": Hint(status)}
}

func Hint(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "check the query parameters; /api/v1/openapi.json lists what each route takes"
	case http.StatusUnauthorized:
		return "Inspector cannot log in to the service; check its credentials in the env"
	case http.StatusNotFound:
		return "it is gone or never existed; list it again"
	case http.StatusServiceUnavailable:
		return "the service is not running in this env; roll env ps shows what runs"
	}
	return "the service did not answer as expected; its /health route says what is wrong"
}
