package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const nonceTTL = 30 * time.Second

type nonce struct {
	action  string
	binding string
	expires time.Time
}

type nonces struct {
	mu    sync.Mutex
	items map[string]nonce
	now   func() time.Time
}

func newNonces() *nonces {
	return &nonces{items: map[string]nonce{}, now: time.Now}
}

func (n *nonces) issue(action, binding string) (string, time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	for k, v := range n.items {
		if now.After(v.expires) {
			delete(n.items, k)
		}
	}
	token := randomToken()
	exp := now.Add(nonceTTL)
	n.items[token] = nonce{action: action, binding: binding, expires: exp}
	return token, exp
}

var (
	errNonceMissing = errors.New("this action needs a confirmation: request a nonce from /api/confirm first")
	errNonceUnknown = errors.New("confirmation unknown, already used or expired; confirm again")
	errNonceTarget  = errors.New("confirmation was given for a different action, target or parameters; confirm again")
)

func (n *nonces) use(token, action, binding string) error {
	if token == "" {
		return errNonceMissing
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.items[token]
	delete(n.items, token)
	if !ok || n.now().After(v.expires) {
		return errNonceUnknown
	}
	if v.action != action || v.binding != binding {
		return errNonceTarget
	}
	return nil
}

func bindingOf(target string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(target)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

func needsNonce(spec core.ActionSpec) bool {
	return spec.Destructive || (spec.Confirm != core.ConfirmNone && spec.Confirm != "")
}

type confirmRequest struct {
	Kind   core.Kind         `json:"kind"`
	Action string            `json:"action"`
	Target string            `json:"target"`
	Params map[string]string `json:"params"`
	Typed  string            `json:"typed"`
}

type confirmResponse struct {
	Nonce   string    `json:"nonce"`
	Expires time.Time `json:"expires"`
}

func (s *Server) findAction(kind core.Kind, name string) (core.ActionSpec, bool) {
	a, ok := s.adapters[kind]
	if !ok {
		return core.ActionSpec{}, false
	}
	for _, spec := range a.Actions() {
		if spec.Name == name {
			return spec, true
		}
	}
	return core.ActionSpec{}, false
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if !decodeBody(w, r, &req) {
		return
	}
	spec, ok := s.findAction(req.Kind, req.Action)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown action "+string(req.Kind)+"/"+req.Action)
		return
	}
	id := string(req.Kind) + "/" + spec.Name
	if spec.Confirm == core.ConfirmTyped && (req.Target == "" || req.Typed != req.Target) {
		s.cfg.Log.Warn("audit: confirm refused", "action", id, "target", req.Target, "reason", "typed mismatch")
		writeError(w, http.StatusUnprocessableEntity, "type "+req.Target+" exactly to confirm")
		return
	}
	token, exp := s.nonces.issue(id, bindingOf(req.Target, req.Params))
	s.cfg.Log.Info("audit: confirm issued", "action", id, "target", req.Target)
	writeJSON(w, http.StatusOK, confirmResponse{Nonce: token, Expires: exp})
}

type actionRequest struct {
	Target string            `json:"target"`
	Params map[string]string `json:"params"`
	Nonce  string            `json:"nonce"`
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	kind := core.Kind(r.PathValue("kind"))
	spec, ok := s.findAction(kind, r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown action "+string(kind)+"/"+r.PathValue("name"))
		return
	}
	var req actionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id := string(kind) + "/" + spec.Name
	if needsNonce(spec) {
		if err := s.nonces.use(req.Nonce, id, bindingOf(req.Target, req.Params)); err != nil {
			s.cfg.Log.Warn("audit: action refused", "action", id, "target", req.Target, "reason", err.Error())
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), actionLimit)
	defer cancel()
	res, err := spec.Run(ctx, core.ActionRequest{Target: req.Target, Params: req.Params})
	if err != nil {
		s.cfg.Log.Warn("audit: action failed", "action", id, "target", req.Target, "params", req.Params, "err", err.Error())
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.cfg.Log.Info("audit: action ran", "action", id, "target", req.Target, "params", req.Params, "result", res.Message)
	writeJSON(w, http.StatusOK, res)
}
