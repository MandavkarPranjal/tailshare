package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tailshare/internal/stream"
)

var testJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01, 0xff, 0xd9}

func newTestServer(t *testing.T, identity IdentityFunc) (*httptest.Server, *stream.Hub) {
	t.Helper()
	hub := stream.NewHub()
	h, err := New(Config{Hub: hub, Identity: identity, Banner: "test banner"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, hub
}

func TestPage(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type is %q, want text/html", ct)
	}
	html := string(body)
	for _, want := range []string{"tailshare", "/stream", "test banner"} {
		if !strings.Contains(html, want) {
			t.Fatalf("page missing %q", want)
		}
	}
}

func TestStreamServesMJPEG(t *testing.T) {
	srv, hub := newTestServer(t, nil)
	hub.Publish(testJPEG)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "multipart/x-mixed-replace") || !strings.Contains(ct, "boundary=") {
		t.Fatalf("content type is %q", ct)
	}

	r := bufio.NewReader(resp.Body)
	// First line of the multipart body.
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "--"+boundary {
		t.Fatalf("first line is %q, want %q", line, "--"+boundary)
	}

	var contentType, contentLength string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		switch {
		case strings.HasPrefix(strings.ToLower(line), "content-type:"):
			contentType = line
		case strings.HasPrefix(strings.ToLower(line), "content-length:"):
			contentLength = line
		}
	}
	if !strings.Contains(contentType, "image/jpeg") {
		t.Fatalf("part content-type is %q", contentType)
	}
	if !strings.Contains(contentLength, "14") {
		t.Fatalf("part content-length is %q, want 14", contentLength)
	}

	payload := make([]byte, 14)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatal(err)
	}
	if payload[0] != 0xff || payload[1] != 0xd8 {
		t.Fatalf("part payload is %x, want JPEG SOI", payload[:2])
	}
	if payload[len(payload)-2] != 0xff || payload[len(payload)-1] != 0xd9 {
		t.Fatalf("part payload is %x, want JPEG EOI", payload[len(payload)-2:])
	}
}

func TestStreamLogsViewerIdentity(t *testing.T) {
	srv, hub := newTestServer(t, func(*http.Request) Identity {
		return Identity{ID: "alice@example.com"}
	})
	hub.Publish(testJPEG)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Reading anything proves the handler ran past connect logging.
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	resp, err := http.Post(srv.URL+"/stream", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status is %d, want 405", resp.StatusCode)
	}
}

func TestNotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status is %d, want 404", resp.StatusCode)
	}
}

func TestNewRequiresHub(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error when hub is missing")
	}
}
