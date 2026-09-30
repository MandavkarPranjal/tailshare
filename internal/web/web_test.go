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

	"tailshare/internal/quality"
	"tailshare/internal/stream"
)

var testJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01, 0xff, 0xd9}

func newTestServer(t *testing.T, identity IdentityFunc) (*httptest.Server, *stream.Hub) {
	t.Helper()
	return newTestServerWith(t, identity, nil)
}

func newTestServerWith(t *testing.T, identity IdentityFunc, webrtc http.Handler) (*httptest.Server, *stream.Hub) {
	t.Helper()
	hub := stream.NewHub()
	h, err := New(Config{Hub: hub, Identity: identity, Banner: "test banner", WebRTC: webrtc})
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

// stubSignalling stands in for the rtc server.
type stubSignalling struct {
	served string
	offer  string
}

func (s *stubSignalling) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.served = r.URL.Path
	body, _ := io.ReadAll(r.Body)
	s.offer = string(body)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"type":"answer","sdp":"v=0"}`)
}

func TestWebRTCSignallingIsRouted(t *testing.T) {
	stub := &stubSignalling{}
	srv, _ := newTestServerWith(t, nil, stub)
	resp, err := http.Post(srv.URL+"/webrtc", "application/json", strings.NewReader(`{"type":"offer","sdp":"v=0"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status is %d, want 200", resp.StatusCode)
	}
	if stub.served != "/webrtc" {
		t.Fatalf("handler saw path %q, want /webrtc", stub.served)
	}
	if !strings.Contains(stub.offer, `"offer"`) {
		t.Fatalf("handler saw body %q", stub.offer)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"answer"`) {
		t.Fatalf("answer not passed through: %s", body)
	}
}

func TestWebRTCAbsentWithoutAHandler(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/webrtc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status is %d, want 404", resp.StatusCode)
	}
}

func TestPageTellsTheViewerWhetherWebRTCIsThere(t *testing.T) {
	page := func(t *testing.T, webrtc http.Handler) string {
		t.Helper()
		srv, _ := newTestServerWith(t, nil, webrtc)
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	if with := page(t, &stubSignalling{}); !strings.Contains(with, `<body data-webrtc`) {
		t.Fatal("page does not advertise webrtc although a handler is configured")
	}
	if without := page(t, nil); strings.Contains(without, `<body data-webrtc`) {
		t.Fatal("page advertises webrtc without a handler")
	}
}

func TestNewRequiresHub(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error when hub is missing")
	}
}

// qualityPage renders the viewer page with a ladder on offer and returns it.
func qualityPage(t *testing.T, webrtc http.Handler, ladder quality.Ladder, query string) string {
	t.Helper()
	h, err := New(Config{Hub: stream.NewHub(), Banner: "test banner", WebRTC: webrtc, Qualities: ladder})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestPageOffersTheLadder(t *testing.T) {
	html := qualityPage(t, &stubSignalling{}, quality.Default, "")
	for _, want := range []string{`id="quality"`, `value="360p"`, `value="480p"`, `value="720p"`, `value="1080p"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	// A viewer who has not chosen gets the best picture on offer.
	if !strings.Contains(html, `<option value="1080p" selected>`) {
		t.Fatal("page does not select the top of the ladder")
	}
}

func TestPageSelectsTheQualityInTheURL(t *testing.T) {
	html := qualityPage(t, &stubSignalling{}, quality.Default, "?quality=720p")
	if !strings.Contains(html, `<option value="720p" selected>`) {
		t.Fatal("page ignores the quality asked for in the URL")
	}
	// The script has to be told too: it is the one that puts the name in the
	// signalling request.
	if !strings.Contains(html, `data-quality="720p"`) {
		t.Fatal("page does not tell the page which rung to ask for")
	}
}

func TestPageFallsBackToTheTopForAnUnknownQuality(t *testing.T) {
	html := qualityPage(t, &stubSignalling{}, quality.Default, "?quality=4321p")
	if !strings.Contains(html, `<option value="1080p" selected>`) {
		t.Fatal("page does not fall back to the best rung on offer")
	}
}

func TestPageHidesThePickerWhenThereIsNothingToPick(t *testing.T) {
	one := quality.Ladder{{Name: "720p", Height: 720}}
	for name, page := range map[string]string{
		"no levels on offer":  qualityPage(t, &stubSignalling{}, nil, ""),
		"only one level":      qualityPage(t, &stubSignalling{}, one, ""),
		"no webrtc to switch": qualityPage(t, nil, quality.Default, ""),
	} {
		if strings.Contains(page, `id="quality"`) {
			t.Fatalf("page shows a picker with %s", name)
		}
	}
}
