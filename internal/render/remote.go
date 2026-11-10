package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Handler is the renderer container's server (`artifex renderer`): POST
// /render with a Request answers with a Result, and GET /healthz says it
// runs. It takes requests only from Artifex, on a network of their own.
func Handler(r Renderer, log *zap.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /render", func(w http.ResponseWriter, req *http.Request) {
		var in Request
		if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request is not a render request"})
			return
		}
		start := time.Now()
		res, err := r.Render(req.Context(), in)
		if err != nil {
			status := http.StatusInternalServerError
			var inv *Invalid
			if errors.As(err, &inv) {
				status = http.StatusBadRequest
			} else {
				log.Warn("render failed", zap.Error(err))
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		log.Info("rendered", zap.Duration("took", time.Since(start)), zap.Int("width", res.Width), zap.Int("height", res.Height))
		writeJSON(w, http.StatusOK, res)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Remote is a Renderer in another container, reached at its address.
type Remote struct {
	URL    string
	Client *http.Client
}

func (r *Remote) Render(ctx context.Context, req Request) (Result, error) {
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.URL, "/")+"/render", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	c := r.Client
	if c == nil {
		c = &http.Client{Timeout: Timeout + 15*time.Second}
	}
	resp, err := c.Do(hr)
	if err != nil {
		return Result{}, fmt.Errorf("the renderer can't be reached: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if resp.StatusCode == http.StatusBadRequest {
			return Result{}, &Invalid{e.Error}
		}
		return Result{}, fmt.Errorf("the renderer failed: %d %s", resp.StatusCode, e.Error)
	}
	var res Result
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(&res); err != nil {
		return Result{}, fmt.Errorf("the renderer's answer is unreadable: %w", err)
	}
	return res, nil
}

// Proxy is the Upstream of a renderer container: it fetches Artifex's
// content and bridge from Artifex's address on their shared network. Only
// /c/ and /static/ reach it (run.serve).
func Proxy(upstream string) (http.Handler, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the upstream must be Artifex's http address, such as http://artifex:8090; got %q", upstream)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "Artifex can't be reached: "+err.Error(), http.StatusBadGateway)
	}
	return p, nil
}
