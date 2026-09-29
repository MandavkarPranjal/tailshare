// Package web serves the live screen viewer.
package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"log"
	"net/http"

	"tailshare/internal/stream"
)

//go:embed templates/index.html
var indexHTML string

// Identity describes the caller of a request.
type Identity struct {
	ID string
}

// IdentityFunc resolves a request to an identity. It may be nil.
type IdentityFunc func(*http.Request) Identity

// Config wires up the handler.
type Config struct {
	Hub      *stream.Hub
	Identity IdentityFunc
	// Banner is shown on the viewer page footer.
	Banner string
}

// Handler serves the viewer page and the MJPEG stream.
type Handler struct {
	hub      *stream.Hub
	identity IdentityFunc
	banner   string
	page     *template.Template
}

// New builds the HTTP handler.
func New(cfg Config) (*Handler, error) {
	if cfg.Hub == nil {
		return nil, fmt.Errorf("web: hub is required")
	}
	page, err := template.New("index").Parse(indexHTML)
	if err != nil {
		return nil, fmt.Errorf("web: parse template: %w", err)
	}
	return &Handler{hub: cfg.Hub, identity: cfg.Identity, banner: cfg.Banner, page: page}, nil
}

// ServeHTTP routes / (viewer page) and /stream (MJPEG).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		h.servePage(w, r)
	case "/stream":
		h.serveStream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) servePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.page.Execute(w, struct{ Banner string }{Banner: h.banner}); err != nil {
		log.Printf("web: render page: %v", err)
	}
}

const boundary = "tailshareframe"

// serveStream writes frames as multipart/x-mixed-replace JPEG parts.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	id := "unknown"
	if h.identity != nil {
		id = h.identity(r).ID
	}
	log.Printf("viewer connected: %s (%s)", id, r.RemoteAddr)
	defer log.Printf("viewer disconnected: %s (%s)", id, r.RemoteAddr)

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ch, cancel := h.hub.Subscribe(r.Context())
	defer cancel()

	var lastSeq uint64
	for f := range ch {
		if f.Seq == lastSeq {
			continue
		}
		lastSeq = f.Seq
		if _, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",
			boundary, len(f.Data)); err != nil {
			return
		}
		if _, err := w.Write(f.Data); err != nil {
			return
		}
		if _, err := w.Write([]byte("\r\n")); err != nil {
			return
		}
		fl.Flush()
	}
}
