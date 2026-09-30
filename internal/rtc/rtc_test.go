package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"tailshare/internal/encode"
	"tailshare/internal/stream"
)

func vp8() encode.CodecInfo {
	return encode.CodecInfo{
		Codec:     encode.CodecVP8,
		MimeType:  "video/VP8",
		ClockRate: 90000,
		Ready:     true,
	}
}

// levels is the set of streams a test server offers, keyed by level, and the
// lookup it is handed: a level that is not in the map is not on offer, which is
// how a viewer asking for one the server does not have comes back.
type levels map[string]Stream

func (l levels) lookup(name string) Stream { return l[name] }

// oneLevel is the common case: a single level on offer, with the parts a test
// did not care about filled in.
func oneLevel(name string, s Stream) Config {
	if s.Frames == nil {
		s.Frames = stream.NewHub()
	}
	if s.Codec == nil {
		s.Codec = vp8
	}
	return Config{Streams: levels{name: s}.lookup}
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Streams == nil {
		cfg.Streams = levels{"1080p": {Frames: stream.NewHub(), Codec: vp8}}.lookup
	}
	s, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webrtc", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// exchange posts a real offer to a running server and returns the response.
func exchange(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestNewRequiresStreams(t *testing.T) {
	if _, err := New(t.Context(), Config{}); err == nil {
		t.Error("a server that can hand out no streams should not start")
	}
}

func TestSignallingRejectsOtherMethods(t *testing.T) {
	s := newTestServer(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "/webrtc", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want %q", got, http.MethodPost)
	}
}

func TestSignallingRejectsBadOffers(t *testing.T) {
	s := newTestServer(t, Config{})
	tests := []struct {
		name string
		body string
	}{
		{"not json", "<html>nope</html>"},
		{"not an offer", `{"type":"answer","sdp":"v=0"}`},
		{"no sdp", `{"type":"offer"}`},
		{"empty body", ``},
		// A level that is not on offer is refused rather than served as
		// something else: a viewer that asked for 360p and was handed the
		// 1080p stream, or the other way round, has no way to tell.
		{"unknown quality", `{"type":"offer","quality":"4320p","sdp":"v=0"}`},
		{"no quality", `{"type":"offer","sdp":"v=0"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, s, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body)
			}
		})
	}
	if n := s.ViewerCount(); n != 0 {
		t.Errorf("a refused offer left %d peers behind", n)
	}
}

// A resolution name is only ever compared against the names the streams are
// registered under, and a viewer that spells it differently still meant that
// stream. Spelling it differently must not turn into a viewer of a level the
// counts and the measurements do not know about.
func TestQualityIsMatchedWhateverItIsSpelled(t *testing.T) {
	s := newTestServer(t, Config{OnViewers: func(map[string]int) {}})
	body := `{"type":"offer","quality":" 1080P ","sdp":"v=0"}`
	// The offer is a stub in the sense that it will not negotiate, so all this
	// asserts is that the name got past the check that refuses unknown ones and
	// was not refused as a bad request.
	rec := post(t, s, body)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("a name that differs only in case and space was refused: %s", rec.Body)
	}
}

// h264WithSets is the description of an H.264 stream whose parameter sets the
// encoder has already seen, which is the only way a browser can decode it.
func h264WithSets() encode.CodecInfo {
	return encode.CodecInfo{
		Codec:     encode.CodecH264,
		MimeType:  "video/H264",
		ClockRate: 90000,
		SDPFmtp: "level-asymmetry-allowed=1;packetization-mode=1;" +
			"profile-level-id=64001F;sprop-parameter-sets=Z0IAKeNQFAe2AtwEBAaQeJEV,aM48gA==",
		Ready: true,
	}
}

// A browser's offer never carries sprop-parameter-sets, and pion builds the
// answer out of what the offer said, so the parameter sets have to be put back
// deliberately. Left to pion the answer describes a codec the browser cannot
// configure: the connection works, the packets arrive, and nothing is ever
// decoded. The viewer on the other end of this looks exactly like a browser in
// that respect, because it is built from pion's default codecs.
func TestAnswerCarriesTheParameterSetsBack(t *testing.T) {
	viewer := newTestViewer(t)
	t.Cleanup(viewer.close)
	offer := viewer.offer.SDP
	if !strings.Contains(offer, "H264/90000") {
		t.Fatalf("the viewer's offer has no H.264 in it, so this proves nothing:\n%s", offer)
	}
	if strings.Contains(offer, "sprop-parameter-sets") {
		t.Fatalf("the viewer's offer already carries the parameter sets, so it is not a browser:\n%s", offer)
	}

	srv := newTestServer(t, oneLevel("1080p", Stream{Codec: h264WithSets}))
	body, err := json.Marshal(viewer.offer)
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, srv, string(body))
	if resp.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", resp.Code, resp.Body)
	}
	var answer signal
	if err := json.Unmarshal(resp.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sprop-parameter-sets=Z0IAKeNQFAe2AtwEBAaQeJEV,aM48gA==",
		"profile-level-id=64001F",
		"packetization-mode=1",
	} {
		if !strings.Contains(answer.SDP, want) {
			t.Errorf("the answer does not describe the stream: no %q in\n%s", want, answer.SDP)
		}
	}
	// The payload type has to be one the browser offered, or it will not
	// recognise the codec at all, so the number cannot be one of our own.
	payloadType := answerSDPAttr(t, answer.SDP, "a=rtpmap:", " H264/90000")
	if !strings.Contains(offer, fmt.Sprintf("a=rtpmap:%s H264/90000", payloadType)) {
		t.Errorf("the answer used payload type %s for H.264, which the offer never offered:\n%s", payloadType, answer.SDP)
	}
}

// answerSDPAttr returns the value that follows prefix in the first line of sdp
// that contains suffix.
func answerSDPAttr(t *testing.T, sdp, prefix, suffix string) string {
	t.Helper()
	for line := range strings.Lines(sdp) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix)
		}
	}
	t.Fatalf("no line in the answer is %s*%s:\n%s", prefix, suffix, sdp)
	return ""
}

// TestAnswerNegotiatesTheFeedbackTheViewerNeeds covers the whole codec at once,
// because pion narrows whatever is registered down to what the offer asked for:
// a codec registered without any feedback ends up with an answer carrying no
// a=rtcp-fb lines at all, and the viewer can neither ask for a lost packet back
// nor ask for a keyframe. It is not specific to H.264, so VP8 is the better
// place to watch it.
func TestAnswerNegotiatesTheFeedbackTheViewerNeeds(t *testing.T) {
	viewer := newTestViewer(t)
	t.Cleanup(viewer.close)
	body, err := json.Marshal(viewer.offer)
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, newTestServer(t, oneLevel("1080p", Stream{})), string(body))
	if resp.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", resp.Code, resp.Body)
	}
	var answer signal
	if err := json.Unmarshal(resp.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nack", "nack pli", "ccm fir"} {
		if !strings.Contains(answer.SDP, want) {
			t.Errorf("the answer does not offer %q, so the viewer cannot ask for it:\n%s", want, answer.SDP)
		}
	}
}

func TestSignallingPreparesTheCodec(t *testing.T) {
	ready := encode.CodecInfo{Codec: encode.CodecH264, MimeType: "video/H264", ClockRate: 90000, Ready: true}
	// A viewer arriving before anything has been encoded asks the pipeline to
	// make the codec describable, and is answered once it is.
	calls := 0
	viewer := newTestViewer(t)
	t.Cleanup(viewer.close)
	srv := newTestServer(t, oneLevel("1080p", Stream{
		Codec: func() encode.CodecInfo {
			if calls == 0 {
				return encode.CodecInfo{Codec: encode.CodecH264, MimeType: "video/H264", ClockRate: 90000}
			}
			return ready
		},
		Prepare: func(context.Context) error {
			calls++
			return nil
		},
	}))
	body, err := json.Marshal(viewer.offer)
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, srv, string(body))
	if resp.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", resp.Code, resp.Body)
	}
	if calls != 1 {
		t.Errorf("Prepare was called %d times, want 1", calls)
	}
}

func TestSignallingGivesUpWhenTheCodecStaysUndescribable(t *testing.T) {
	srv := newTestServer(t, oneLevel("1080p", Stream{
		Codec:   func() encode.CodecInfo { return encode.CodecInfo{Codec: encode.CodecH264} },
		Prepare: func(context.Context) error { return errors.New("no keyframe") },
	}))
	if resp := post(t, srv, `{"type":"offer","quality":"1080p","sdp":"v=0"}`); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status is %d, want 503: %s", resp.Code, resp.Body)
	}
	if n := srv.ViewerCount(); n != 0 {
		t.Errorf("ViewerCount() = %d, want 0", n)
	}
}

// A codec that cannot be described yet is only worth waiting for if something
// can make it describable. A stream with a Prepare gets that call and is
// answered once it has; a stream without one has nothing to wait for, so the
// viewer is turned away straight away rather than left hanging.
func TestSignallingRefusesWhenNothingCanPrepareTheCodec(t *testing.T) {
	s := newTestServer(t, oneLevel("1080p", Stream{
		Codec: func() encode.CodecInfo {
			return encode.CodecInfo{Codec: encode.CodecH264, MimeType: "video/H264", ClockRate: 90000}
		},
	}))
	rec := post(t, s, `{"type":"offer","quality":"1080p","sdp":"v=0"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestServerReportsViewersOnceAtStartup(t *testing.T) {
	var seen []map[string]int
	cfg := oneLevel("1080p", Stream{})
	cfg.OnViewers = func(counts map[string]int) { seen = append(seen, counts) }
	s := newTestServer(t, cfg)
	// The report is a set of levels in use, and an empty room is an empty set:
	// there is no level to name until somebody has asked for one.
	if len(seen) != 1 || len(seen[0]) != 0 {
		t.Errorf("startup reported %v, want one empty report", seen)
	}
	if n := s.ViewerCount(); n != 0 {
		t.Errorf("ViewerCount = %d, want 0", n)
	}
}

// stubBWE stands in for a congestion controller with nothing to do, carrying
// the round trip and loss a probe would have seen along with the rate.
type stubBWE struct {
	bitrate int
	rtt     time.Duration
	loss    float64
}

func (stubBWE) AddStream(*interceptor.StreamInfo, interceptor.RTPWriter) interceptor.RTPWriter {
	return nil
}
func (stubBWE) WriteRTCP([]rtcp.Packet, interceptor.Attributes) error { return nil }
func (s stubBWE) GetTargetBitrate() int                               { return s.bitrate }
func (stubBWE) OnTargetBitrateChange(func(int))                       {}
func (stubBWE) GetStats() map[string]any                              { return nil }
func (stubBWE) Close() error                                          { return nil }

// stubPeers puts viewers on the server at a level, with the numbers a
// bandwidth estimator and a probe would have reported. They have no connection
// behind them, so the test has to take them off the list again before the
// server's own cleanup goes looking for something to close.
func stubPeers(t *testing.T, s *Server, level string, watching ...stubBWE) {
	t.Helper()
	peers := make([]*peer, 0, len(watching))
	// Written under the lock the server itself uses: New has already started a
	// monitor that walks this map every second, and a map written without it is
	// a race that -race reports and a crash waiting to happen.
	s.mu.Lock()
	for _, bwe := range watching {
		p := &peer{
			server:  s,
			quality: level,
			bwe:     bwe,
			probe:   &probe{rtt: bwe.rtt, loss: bwe.loss},
		}
		peers = append(peers, p)
		// Closed before the server ever gets hold of it. The monitor New
		// starts closes everything on the list when the test's context ends,
		// and that happens before the cleanup below can take these off it; a
		// stub has no connection behind it, so being closed for real would
		// reach for a cancel function and a peer connection that are not
		// there. Spending the once here is what leaves close to do nothing.
		p.once.Do(func() {})
		s.peers[p] = struct{}{}
	}
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		for _, p := range peers {
			delete(s.peers, p)
		}
		s.mu.Unlock()
	})
}

func TestSampleReportsTheTightestViewer(t *testing.T) {
	s := newTestServer(t, Config{})
	stubPeers(t, s, "1080p",
		stubBWE{bitrate: 3_000_000, rtt: 20 * time.Millisecond, loss: 0.01},
		stubBWE{bitrate: 400_000, rtt: 180 * time.Millisecond, loss: 0.07},
	)

	got := s.sample()
	if len(got) != 1 {
		t.Fatalf("sample with one level in use = %d reports, want 1", len(got))
	}
	if got[0].Peers != 2 {
		t.Errorf("Peers = %d, want 2", got[0].Peers)
	}
	if got[0].Bitrate != 400_000 {
		t.Errorf("Bitrate = %d, want the slowest viewer's 400000", got[0].Bitrate)
	}
	if got[0].RTT != 180*time.Millisecond {
		t.Errorf("RTT = %v, want the slowest viewer's 180ms", got[0].RTT)
	}
	if got[0].Loss != 0.07 {
		t.Errorf("Loss = %v, want the worst viewer's 0.07", got[0].Loss)
	}
}

// TestSampleReportsEachLevelSeparately is the reason there is a ladder at all:
// a viewer on a slow link at 360p says nothing about what the link can carry for
// the viewer watching at 1080p, and letting it pull the whole stream down would
// hand everybody else a worse picture to make up for somebody else's network.
func TestSampleReportsEachLevelSeparately(t *testing.T) {
	s := newTestServer(t, Config{})
	stubPeers(t, s, "360p", stubBWE{bitrate: 200_000, rtt: 300 * time.Millisecond, loss: 0.2})
	stubPeers(t, s, "1080p",
		stubBWE{bitrate: 5_000_000, rtt: 10 * time.Millisecond, loss: 0},
		stubBWE{bitrate: 2_000_000, rtt: 40 * time.Millisecond, loss: 0},
	)

	reports := make(map[string]Feedback)
	for _, f := range s.sample() {
		reports[f.Quality] = f
	}
	if len(reports) != 2 {
		t.Fatalf("sample reported %d levels, want 2: %+v", len(reports), reports)
	}
	if got := reports["360p"]; got.Peers != 1 || got.Bitrate != 200_000 || got.RTT != 300*time.Millisecond {
		t.Errorf("360p = %+v, want 1 peer at 200000 and 300ms away", got)
	}
	if got := reports["1080p"]; got.Peers != 2 || got.Bitrate != 2_000_000 || got.RTT != 40*time.Millisecond {
		t.Errorf("1080p = %+v, want 2 peers, the tightest of them 200000 and 40ms away", got)
	}
}

func TestSampleOfNothing(t *testing.T) {
	s := newTestServer(t, Config{})
	got := s.sample()
	// One report about the room rather than one per level, because the thing a
	// caller needs to hear is that everybody has gone, and a level nobody ever
	// asked for must not look the same as a level that has just emptied.
	if len(got) != 1 || got[0] != (Feedback{}) {
		t.Errorf("sample with no viewers = %+v, want one zero report", got)
	}
}

func TestGateWaitsForAKeyframe(t *testing.T) {
	var g gate
	if g.allow(stream.Frame{}) {
		t.Error("a viewer was let in before any keyframe")
	}
	if g.allow(stream.Frame{Keyframe: true}) != true {
		t.Error("the keyframe should have been let through")
	}
	if !g.allow(stream.Frame{}) {
		t.Error("frames after the first keyframe should go straight out")
	}
}

// TestKeyframeRequestsAreCountedOnce covers the other half of reading the
// reports: a viewer that has lost its way asks for a new keyframe, and a
// browser that has lost its way asks several times a second. Nothing can be
// done about it — the next keyframe is on its way either way — so the requests
// are counted once each and then ignored for a while rather than filling the
// log.
func TestKeyframeRequestsAreCountedOnce(t *testing.T) {
	s := newTestServer(t, Config{})
	p := &peer{server: s, id: "test"}

	picture, err := rtcp.Marshal([]rtcp.Packet{&rtcp.PictureLossIndication{
		MediaSSRC: 1,
		// The same packet the browser sends, marshalled through pion's own
		// codec, is what the reader hands to wantsKeyframe.
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.wantsKeyframe(picture) {
		t.Fatal("a keyframe request from a viewer that had not asked recently was ignored")
	}
	if p.wantsKeyframe(picture) {
		t.Error("a second request straight after was counted too")
	}
	// Once the wait is over the next request counts again, and a report that
	// asks for nothing is not a request at all.
	p.askedAt = time.Now().Add(-keyframeWait)
	receiver, err := rtcp.Marshal([]rtcp.Packet{&rtcp.ReceiverReport{}})
	if err != nil {
		t.Fatal(err)
	}
	if p.wantsKeyframe(receiver) {
		t.Error("a report with no request in it was counted as one")
	}
	if !p.wantsKeyframe(picture) {
		t.Error("a request after the wait was dropped")
	}
	if p.wantsKeyframe([]byte{0xff, 0xff, 0xff}) {
		t.Error("a report that is not a report at all was counted as a request")
	}
}

func TestCloseIsQuietTwice(t *testing.T) {
	var count atomic.Int64
	cfg := oneLevel("1080p", Stream{})
	cfg.OnViewers = func(map[string]int) { count.Add(1) }
	s := newTestServer(t, cfg)
	s.Close()
	s.Close()
	// The only report is the one New makes at startup. Nothing is connected in
	// this test, so closing takes no peer off the list and neither Close has a
	// change in the room to report: the count stays at that one startup call,
	// however many times the empty server is closed.
	if got := count.Load(); got != 1 {
		t.Errorf("OnViewers called %d times, want 1", got)
	}
}

// A server on its way down has no streams left to feed a viewer, so an offer
// that arrives after the shutdown is turned away rather than negotiated from
// start to finish and then dropped.
func TestSignallingTurnsAwayOffersAfterAClose(t *testing.T) {
	viewer := newTestViewer(t)
	t.Cleanup(viewer.close)
	srv := newTestServer(t, oneLevel("1080p", Stream{}))
	srv.Close()
	body, err := json.Marshal(viewer.offer)
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, srv, string(body))
	if resp.Code != http.StatusServiceUnavailable {
		t.Errorf("status is %d, want %d: %s", resp.Code, http.StatusServiceUnavailable, resp.Body)
	}
	if n := srv.ViewerCount(); n != 0 {
		t.Errorf("ViewerCount() = %d, want 0, no viewer is answered after a close", n)
	}
	// The context the monitor watches is done as well, so a closed server is
	// not still measuring a room behind the shutdown.
	if err := srv.ctx.Err(); err == nil {
		t.Error("the server's context is still live, so the monitor is still running after a close")
	}
}

// TestAnOfferInFlightOverACloseIsTurnedAway covers a handshake that was still
// going when the server was closed: its peer was not in the snapshot Close took
// and nothing else was ever going to close it, so it would have been left
// connected and counted as a viewer for good, with no stream left behind it.
func TestAnOfferInFlightOverACloseIsTurnedAway(t *testing.T) {
	viewer := newTestViewer(t)
	t.Cleanup(viewer.close)
	var describable atomic.Bool
	asked, ready := make(chan struct{}), make(chan struct{})
	srv := newTestServer(t, oneLevel("1080p", Stream{
		Codec: func() encode.CodecInfo {
			if describable.Load() {
				return h264WithSets()
			}
			return encode.CodecInfo{Codec: encode.CodecH264, MimeType: "video/H264", ClockRate: 90000}
		},
		// The wait for a codec to become describable is where the shutdown
		// lands. Nothing else in this exchange takes long enough to put one in
		// the middle of it, and a viewer that had to be fed a frame to be able
		// to wait would make the test a great deal longer.
		Prepare: func(context.Context) error {
			close(asked)
			<-ready
			describable.Store(true)
			return nil
		},
	}))
	body, err := json.Marshal(viewer.offer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webrtc", bytes.NewBuffer(body))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()
	<-asked
	srv.Close()
	close(ready)
	<-done
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status is %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	if n := srv.ViewerCount(); n != 0 {
		t.Errorf("ViewerCount() = %d, want 0, the viewer that arrived over the shutdown", n)
	}
}

// TestViewerReceivesTheStream connects a second pion stack as a viewer and
// checks that frames reach it over the wire.
func TestViewerReceivesTheStream(t *testing.T) {
	frames := stream.NewHub()
	joined := make(chan struct{})
	var once sync.Once
	feedback := make(chan Feedback, 8)
	cfg := oneLevel("1080p", Stream{Frames: frames})
	cfg.OnViewers = func(counts map[string]int) {
		if counts["1080p"] == 1 {
			once.Do(func() { close(joined) })
		}
	}
	cfg.OnFeedback = func(f Feedback) {
		select {
		case feedback <- f:
		default:
		}
	}
	s := newTestServer(t, cfg)
	ts := httptest.NewServer(s)
	defer ts.Close()

	client := newTestViewer(t)
	defer client.close()
	client.connect(t, ts.URL)

	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("the viewer never reached the server")
	}

	// The stream is fed from the broadcast, which hands the newest frame to
	// whoever joins, so one keyframe and a few more frames is all it takes.
	frames.PublishFrame(stream.Frame{
		Data:     []byte("coded frame"),
		Keyframe: true,
		Duration: 40 * time.Millisecond,
	})
	deadline := time.Now().Add(20 * time.Second)
	for client.packets.Load() == 0 && time.Now().Before(deadline) {
		frames.PublishFrame(stream.Frame{
			Data:     []byte("coded frame"),
			Duration: 40 * time.Millisecond,
		})
		time.Sleep(50 * time.Millisecond)
	}
	if got := client.packets.Load(); got == 0 {
		t.Fatal("no packets arrived at the viewer")
	}
	if client.bytes.Load() == 0 {
		t.Error("the packets that arrived were empty")
	}
	if got, _ := client.mime.Load().(string); got != "video/VP8" {
		t.Errorf("viewer negotiated %q, want video/VP8", got)
	}
	if n := s.ViewerCount(); n != 1 {
		t.Errorf("ViewerCount = %d, want 1", n)
	}

	// A viewer that is watching is what the encoder needs to hear about, and it
	// has to be about the level that viewer asked for.
	seen := false
	for stop := time.After(3 * time.Second); !seen; {
		select {
		case f := <-feedback:
			seen = f.Peers == 1 && f.Quality == "1080p"
		case <-stop:
			t.Fatal("no feedback reported the viewer")
		}
	}

	s.Close()
	if n := s.ViewerCount(); n != 0 {
		t.Errorf("ViewerCount after Close = %d, want 0", n)
	}
	if !client.closedWithin(10 * time.Second) {
		t.Error("the viewer's connection was not closed when the server went away")
	}
	// An empty room is what stops the capture, so the last word on the
	// viewers has to say so.
	empty := time.After(3 * time.Second)
	for {
		select {
		case f := <-feedback:
			if f.Peers == 0 && f.Quality == "" {
				return
			}
		case <-empty:
			t.Fatal("no feedback reported the empty room")
		}
	}
}

// TestViewersAtDifferentLevelsGetDifferentStreams is the claim the whole ladder
// rests on: the level a viewer asked for decides which broadcast it is fed
// from. Publishing to one level must not put a frame on another level's
// connection, or the two are the same stream with a different name on it.
func TestViewersAtDifferentLevelsGetDifferentStreams(t *testing.T) {
	small, large := stream.NewHub(), stream.NewHub()
	cfg := Config{Streams: levels{
		"360p":  {Frames: small, Codec: vp8},
		"1080p": {Frames: large, Codec: vp8},
	}.lookup}
	var mu sync.Mutex
	var watching map[string]int
	cfg.OnViewers = func(counts map[string]int) {
		mu.Lock()
		watching = counts
		mu.Unlock()
	}
	s := newTestServer(t, cfg)
	ts := httptest.NewServer(s)
	defer ts.Close()

	low := newTestViewer(t, "360p")
	defer low.close()
	high := newTestViewer(t, "1080p")
	defer high.close()
	low.connect(t, ts.URL)
	high.connect(t, ts.URL)

	// Both connections are registered by the time the answers are out, and the
	// viewer counts are what the service stands the encoders up from.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		mu.Lock()
		seen := watching["360p"] == 1 && watching["1080p"] == 1
		mu.Unlock()
		if seen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	counts := watching
	mu.Unlock()
	if counts["360p"] != 1 || counts["1080p"] != 1 {
		t.Fatalf("viewer counts are %v, want one at each level", counts)
	}

	// Keyframes and a few frames after, on the 360p broadcast alone.
	deadline := time.Now().Add(20 * time.Second)
	for low.packets.Load() == 0 && time.Now().Before(deadline) {
		small.PublishFrame(stream.Frame{
			Data:     []byte("coded frame"),
			Keyframe: true,
			Duration: 40 * time.Millisecond,
		})
		time.Sleep(50 * time.Millisecond)
	}
	if got := low.packets.Load(); got == 0 {
		t.Error("the viewer that asked for 360p received nothing")
	}
	if got := high.packets.Load(); got != 0 {
		t.Errorf("the viewer that asked for 1080p received %d packets from the 360p stream", got)
	}
}

// testViewer is the other end of a connection: a plain pion stack that offers
// to receive video and counts what turns up.
type testViewer struct {
	pc        *webrtc.PeerConnection
	offer     signal
	packets   atomic.Int64
	bytes     atomic.Int64
	mime      atomic.Value // string
	connected chan struct{}
	state     chan webrtc.PeerConnectionState
}

// newTestViewer makes a viewer asking for the given level, or for 1080p, which
// is what the single level test servers offer.
func newTestViewer(t *testing.T, quality ...string) *testViewer {
	t.Helper()
	level := "1080p"
	if len(quality) > 0 {
		level = quality[0]
	}
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("register codecs: %v", err)
	}
	registry := &interceptor.Registry{}
	if err := webrtc.ConfigureTWCCSender(mediaEngine, registry); err != nil {
		t.Fatalf("transport feedback: %v", err)
	}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		t.Fatalf("interceptors: %v", err)
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client peer connection: %v", err)
	}
	v := &testViewer{
		pc:        pc,
		connected: make(chan struct{}),
		state:     make(chan webrtc.PeerConnectionState, 8),
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatalf("recvonly: %v", err)
	}
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		v.mime.Store(track.Codec().MimeType)
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				v.packets.Add(1)
				v.bytes.Add(int64(len(pkt.Payload)))
			}
		}()
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		select {
		case v.state <- state:
		default:
		}
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-v.connected:
			default:
				close(v.connected)
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("local description: %v", err)
	}
	<-gathered
	v.offer = signal{Type: "offer", Quality: level, SDP: pc.LocalDescription().SDP}
	return v
}

// connect hands the offer to a server and takes the answer.
func (v *testViewer) connect(t *testing.T, url string) {
	t.Helper()
	body, err := json.Marshal(v.offer)
	if err != nil {
		t.Fatalf("encode offer: %v", err)
	}
	resp := exchange(t, url, string(body))
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("signalling: %s: %s", resp.Status, payload)
	}
	var answer signal
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.Type != "answer" {
		t.Fatalf("answer type = %q, want answer", answer.Type)
	}
	if err := v.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer.SDP,
	}); err != nil {
		t.Fatalf("remote description: %v", err)
	}
}

func (v *testViewer) closedWithin(d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case state := <-v.state:
			if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func (v *testViewer) close() { _ = v.pc.Close() }
