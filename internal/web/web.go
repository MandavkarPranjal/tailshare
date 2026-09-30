// Package web serves the live screen viewer.
package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"log"
	"net/http"

	"tailshare/internal/quality"
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
	// WebRTC answers the signalling exchange the viewer starts with. When it
	// is nil the page never offers a WebRTC stream and the viewer uses the
	// MJPEG endpoint on its own.
	WebRTC http.Handler
	// Qualities are the resolutions the viewer may pick between, smallest
	// first. With none on offer the page shows no picker and the viewer takes
	// whatever the service sends, which is also what the picker would have
	// selected if it had been there.
	Qualities quality.Ladder
}

// Handler serves the viewer page and the MJPEG stream.
type Handler struct {
	hub       *stream.Hub
	identity  IdentityFunc
	banner    string
	webrtc    http.Handler
	qualities quality.Ladder
	page      *template.Template
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
	return &Handler{
		hub:       cfg.Hub,
		identity:  cfg.Identity,
		banner:    cfg.Banner,
		webrtc:    cfg.WebRTC,
		qualities: cfg.Qualities,
		page:      page,
	}, nil
}

// ServeHTTP routes / (viewer page), /stream (MJPEG) and /webrtc (signalling).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		h.servePage(w, r)
	case "/stream":
		h.serveStream(w, r)
	case "/webrtc":
		if h.webrtc == nil {
			http.NotFound(w, r)
			return
		}
		h.webrtc.ServeHTTP(w, r)
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
	// The chosen resolution rides in the URL rather than in a cookie so that a
	// link can carry it and so that a viewer who has picked one gets the same
	// page back on a reload instead of being reset to the top of the ladder.
	// An unknown name is not an error: the viewer still gets a page, on the best
	// rung on offer, which is what they would have got without asking.
	selected := h.qualities.Top()
	if wanted := r.URL.Query().Get("quality"); wanted != "" {
		if level, ok := h.qualities.Find(wanted); ok {
			selected = level
		}
	}
	data := struct {
		Banner    string
		WebRTC    bool
		Qualities quality.Ladder
		Selected  quality.Level
	}{Banner: h.banner, WebRTC: h.webrtc != nil, Qualities: h.qualities, Selected: selected}
	if err := h.page.Execute(w, data); err != nil {
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
