package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/hub"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/perf"
	"github.com/dockergiant/images/inspector/internal/timeline"
)

const (
	hubBuffer   = 1024
	maxBody     = 1 << 20
	infoTTL     = 10 * time.Second
	readTimeout = 15 * time.Second
	actionLimit = 5 * time.Minute
)

type Config struct {
	Env      string
	Domain   string
	Secret   string
	Version  string
	Docker   docker.API
	Adapters []core.Adapter
	Feeds    []Feed
	Magento  magento.Reader
	Hub      *hub.Hub
	Timeline *timeline.Timeline
	Store    *apm.Store
	Capture  *capture.Manager
	UI       fs.FS
	// Clients holds userscript/ and extension/, the reference clients of
	// /api/v1 that Inspector serves for installing.
	Clients fs.FS
	// Perf runs the Performance page: tracing switches, cost and benchmark.
	Perf *perf.Perf
	Log  *slog.Logger
}

// Feed is a source outside the adapters, such as the nginx access log, that
// the timeline follows and the drawer can subscribe to as <kind>:<name>.
type Feed struct {
	Kind core.Kind
	Spec core.StreamSpec
}

type cachedInfo struct {
	info core.Info
	at   time.Time
}

type Server struct {
	cfg      Config
	csrf     string
	origin   string
	host     string
	adapters map[core.Kind]core.Adapter
	gate     *gate
	nonces   *nonces
	ring     *ring
	now      func() time.Time

	infoMu sync.Mutex
	infos  map[core.Kind]cachedInfo

	relayOnce sync.Once

	wantMu  sync.Mutex
	want    map[string]int
	apiWant map[string]int
	stopped map[string]time.Time

	zipOnce sync.Once
	zipData []byte
	zipErr  error

	uiOnce sync.Once
	uiSum  string

	feeds    feeds
	recheck  time.Duration
	leaseFor time.Duration
}

func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Hub == nil {
		cfg.Hub = hub.New(hubBuffer)
	}
	if cfg.Secret == "" {
		cfg.Log.Error("api: no gate secret configured, refusing every request")
	}
	s := &Server{
		cfg:      cfg,
		csrf:     randomToken(),
		host:     "inspector." + cfg.Domain,
		origin:   "https://inspector." + cfg.Domain,
		adapters: map[core.Kind]core.Adapter{},
		nonces:   newNonces(),
		ring:     newRing(ringSize),
		now:      time.Now,
		infos:    map[core.Kind]cachedInfo{},
		want:     map[string]int{},
		apiWant:  map[string]int{},
		stopped:  map[string]time.Time{},
		feeds:    feeds{joined: map[string]*feed{}},
		recheck:  timelineRecheck,
		leaseFor: leaseFor,
	}
	s.gate = newGate(cfg.Docker, cfg.Env, cfg.Secret)
	for _, a := range cfg.Adapters {
		s.adapters[a.Kind()] = a
	}
	s.relayBenchmark()
	return s
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("api: no randomness for the csrf token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// uiHash is a short sha256 over the paths and bytes of the embedded UI.
func (s *Server) uiHash() string {
	s.uiOnce.Do(func() {
		if s.cfg.UI == nil {
			return
		}
		h := sha256.New()
		err := fs.WalkDir(s.cfg.UI, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(s.cfg.UI, path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%d\x00", path, len(data))
			h.Write(data)
			return nil
		})
		if err != nil {
			s.cfg.Log.Warn("api: cannot hash the embedded UI", "err", err)
			return
		}
		s.uiSum = hex.EncodeToString(h.Sum(nil))[:16]
	})
	return s.uiSum
}

func (s *Server) Handler() http.Handler {
	return s.gate.wrap(s.checkHeaders(s.routes()))
}

func (s *Server) info(ctx context.Context, a core.Adapter, refresh bool) core.Info {
	s.infoMu.Lock()
	c, ok := s.infos[a.Kind()]
	s.infoMu.Unlock()
	if ok && !refresh && s.now().Sub(c.at) < infoTTL {
		return c.info
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	info, err := a.Detect(ctx)
	info.Kind = a.Kind()
	if err != nil && info.Error == "" {
		info.Error = err.Error()
	}
	s.infoMu.Lock()
	s.infos[a.Kind()] = cachedInfo{info: info, at: s.now()}
	s.infoMu.Unlock()
	return info
}

func (s *Server) lan(ctx context.Context) (bool, string) {
	if s.cfg.Docker == nil {
		return true, "Traefik cannot be inspected, so Inspector refuses writes"
	}
	t, err := s.cfg.Docker.Traefik(ctx, false)
	if err != nil {
		return true, "Traefik cannot be inspected (" + err.Error() + "), so Inspector refuses writes"
	}
	if !t.Found {
		return true, "Traefik is not running, so Inspector cannot prove it listens on loopback only and refuses writes"
	}
	if t.LAN {
		return true, "Traefik listens beyond loopback (" + joinBindings(t.Bindings) + "), so anyone on the LAN can reach Inspector; writes are refused"
	}
	return false, ""
}

func joinBindings(b []string) string {
	if len(b) == 0 {
		return "no bindings found"
	}
	out := b[0]
	for _, x := range b[1:] {
		out += ", " + x
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
