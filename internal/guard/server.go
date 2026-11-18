package guard

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

// maxBody bounds request bodies: a JIT config plus a name.
const maxBody = 128 << 10

// Handler serves the guard API. Every refusal is logged with DENY so it can be grepped from container logs.
func (g *Guard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, g.Info())
	})
	mux.HandleFunc("GET /v1/slots", func(w http.ResponseWriter, r *http.Request) {
		list := g.List
		if r.URL.Query().Get("stats") != "" {
			list = g.ListWithStats
		}
		s, err := list(r.Context())
		if err != nil {
			g.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("PUT /v1/slots/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := g.slotArg(w, r)
		if !ok {
			return
		}
		var req api.StartRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			g.fail(w, r, errors.Join(ErrBadRequest, err))
			return
		}
		if err := g.Start(r.Context(), n, req); err != nil {
			g.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("DELETE /v1/slots/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := g.slotArg(w, r)
		if !ok {
			return
		}
		if err := g.Stop(r.Context(), n); err != nil {
			g.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/slots/{n}/keep-cache", func(w http.ResponseWriter, r *http.Request) {
		n, ok := g.slotArg(w, r)
		if !ok {
			return
		}
		var req api.KeepCacheRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			g.fail(w, r, errors.Join(ErrBadRequest, err))
			return
		}
		if err := g.KeepCache(r.Context(), n, req.RunnerName); err != nil {
			g.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		g.log.Warn("DENY", "method", r.Method, "path", r.URL.Path, "reason", "no such endpoint")
		writeJSON(w, http.StatusNotFound, api.Error{Error: "no such endpoint"})
	})
	return mux
}

func (g *Guard) slotArg(w http.ResponseWriter, r *http.Request) (int, bool) {
	s := r.PathValue("n")
	n, err := strconv.Atoi(s)
	if err != nil || strings.TrimLeft(s, "0123456789") != "" {
		g.fail(w, r, errors.Join(ErrBadRequest, errors.New("slot must be a number")))
		return 0, false
	}
	return n, true
}

func (g *Guard) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrBadRequest):
		code = http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		code = http.StatusConflict
	case errors.Is(err, ErrNotReady):
		code = http.StatusServiceUnavailable
	}
	lvl, msg := slog.LevelError, "error"
	if code < 500 || code == http.StatusServiceUnavailable {
		lvl, msg = slog.LevelWarn, "DENY"
	}
	g.log.Log(r.Context(), lvl, msg, "method", r.Method, "path", r.URL.Path, "status", code, "reason", err.Error())
	writeJSON(w, code, api.Error{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
