package apm

import (
	"errors"
	"net/http"
	"strings"
)

const (
	maxPending      = 32
	maxPendingBytes = 32 << 20
)

var (
	ErrBadTraceID      = errors.New("trace id must be 32 lowercase hex characters")
	ErrProfileTooLarge = errors.New("profile larger than 5 MB")
)

func validKey(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func profileHandler(s *Store, w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id = strings.ToLower(id)
	if !validKey(id) {
		http.Error(w, ErrBadTraceID.Error(), http.StatusBadRequest)
		return
	}
	body, err := readBody(w, r, maxProfile)
	if err != nil {
		s.noteRejected("profile " + id + ": " + err.Error())
		return
	}
	if err := s.SetProfile(id, body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// The prepend posts at shutdown and the sidecar flushes the trace about a second later,
// so a profile normally arrives before its trace and waits here.
func (s *Store) park(id string, data []byte) {
	if old, ok := s.pending[id]; ok {
		s.pendingBytes -= int64(len(old))
		s.dropPendingOrder(id)
	}
	s.pending[id] = data
	s.pendingOrder = append(s.pendingOrder, id)
	s.pendingBytes += int64(len(data))
	for len(s.pendingOrder) > 0 && (len(s.pending) > maxPending || s.pendingBytes > maxPendingBytes) {
		oldest := s.pendingOrder[0]
		s.pendingOrder = s.pendingOrder[1:]
		s.pendingBytes -= int64(len(s.pending[oldest]))
		delete(s.pending, oldest)
	}
}

func (s *Store) unpark(id string) ([]byte, bool) {
	data, ok := s.pending[id]
	if !ok {
		return nil, false
	}
	delete(s.pending, id)
	s.pendingBytes -= int64(len(data))
	s.dropPendingOrder(id)
	return data, true
}

func (s *Store) dropPendingOrder(id string) {
	for i, p := range s.pendingOrder {
		if p == id {
			s.pendingOrder = append(s.pendingOrder[:i], s.pendingOrder[i+1:]...)
			return
		}
	}
}
