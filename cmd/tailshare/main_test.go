package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"tailshare/internal/preview"
	"tailshare/internal/quality"
	"tailshare/internal/rtc"
	"tailshare/internal/stream"
)

// stubGrab is a capture that is never asked for anything: these tests are about
// what happens to the encoders, and the frames would only get in the way.
type stubGrab struct{}

func (stubGrab) Name() string { return "stub" }

func (stubGrab) Capture(context.Context) ([]byte, error) {
	return testJPEG, nil
}

// testJPEG is a real picture, because the fallback decodes what it is handed and
// a bare header would only teach it to fail. Two grey pixels is the smallest
// JPEG that is one.
var testJPEG = tinyJPEG()

func tinyJPEG() []byte {
	var buf bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, 2, 2))
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 30}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// testPreview is the fallback the MJPEG endpoint is served from.
func testPreview(t *testing.T, jpeg *stream.Hub, fps int) *preview.Preview {
	t.Helper()
	p, err := preview.New(preview.Config{Source: jpeg, Width: 640, Quality: 45, FPS: fps})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ladderOf parses a -qualities value the way run() does, so a test that is about
// what the screen can show is not also a test of the parser.
func ladderOf(t *testing.T, spec string) quality.Ladder {
	t.Helper()
	ladder, err := quality.Parse(spec)
	if err != nil {
		t.Fatalf("parsing -qualities %q: %v", spec, err)
	}
	return ladder
}

func TestServedLadderDropsWhatTheScreenCannotShow(t *testing.T) {
	// A 1280x800 laptop has no 1080 lines in it, and offering one would be an
	// upscale: the same encode, a blurrier picture, more of the budget.
	fit := servedLadder(ladderOf(t, "360p,480p,720p,1080p"),
		quality.Capture{Width: 1280, Height: 800}, 0)
	if got := fit.Names(); got != "360p, 480p, 720p" {
		t.Errorf("ladder is %q, want 360p, 480p, 720p", got)
	}
}

func TestServedLadderDropsWhatWouldNotFitTheWidth(t *testing.T) {
	// -width says how wide a picture may be, and at 16:10 a 1080 line capture
	// scales to 1728 across, so 1440 keeps everything up to 720p.
	fit := servedLadder(ladderOf(t, "360p,720p,1080p"),
		quality.Capture{Width: 1728, Height: 1080}, 1440)
	if got := fit.Names(); got != "360p, 720p" {
		t.Errorf("ladder is %q, want 360p, 720p", got)
	}
}

func TestServedLadderAlwaysLeavesSomethingToWatch(t *testing.T) {
	// A screen too small for the whole ladder is still watchable, and the
	// smallest rung on offer beats a page with nothing to pick.
	fit := servedLadder(ladderOf(t, "720p,1080p"),
		quality.Capture{Width: 640, Height: 480}, 0)
	if got := fit.Names(); got != "720p" {
		t.Errorf("ladder is %q, want 720p", got)
	}
}

// A -width below every resolution on offer is a width nothing can be served
// inside, and the two ways of carrying on from there are a page offering nothing
// to watch and a page offering a picture the flag says was dropped. The cap is
// refused, and the refusal names the cap and the width the smallest resolution
// needs, since that is the number the person who set it has to change.
func TestAWidthCapNarrowerThanEveryResolutionIsRefused(t *testing.T) {
	ladder := ladderOf(t, "360p,720p,1080p")
	hd := quality.Capture{Width: 1920, Height: 1080}
	err := widthCapKept(ladder, hd, 400)
	if err == nil {
		t.Fatal("a 400 pixel cap was accepted on a 1920x1080 screen, where 360p is 640 across")
	}
	for _, want := range []string{"-width 400", "360p", "640"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is %q, want it to name %q", err, want)
		}
	}
	// A cap the smallest rung comes out inside is a cap that can be kept, at
	// exactly the width it needs as well as above it, and no cap is nothing to
	// keep either way.
	for _, width := range []int{0, 640, 700, 7680} {
		if err := widthCapKept(ladder, hd, width); err != nil {
			t.Errorf("-width %d was refused on a 1920x1080 screen: %v", width, err)
		}
	}
	// The shape of the capture is what decides it, so a cap nothing is known to
	// break is not refused by guesswork.
	if err := widthCapKept(ladder, quality.Capture{}, 400); err != nil {
		t.Errorf("a cap was refused for a capture of unknown shape: %v", err)
	}
}

// The rungs a width cap rules out go with the cap, all of them: what is left is
// the empty ladder that widthCapKept refuses, rather than one rung that breaks it.
func TestServedLadderLeavesNothingWhenNoRungFitsTheWidth(t *testing.T) {
	fit := servedLadder(ladderOf(t, "360p,720p"), quality.Capture{Width: 1920, Height: 1080}, 400)
	if len(fit) != 0 {
		t.Errorf("ladder is %q under a 400 pixel cap, want nothing at all", fit.Names())
	}
}

// An empty ladder is not a page with no choice, it is a page with no picture, so
// the encoders are not built from one and it is said rather than indexed into.
func TestAnEmptyLadderIsRefusedBeforeAnyEncoderIsBuilt(t *testing.T) {
	pipe := &pipeline{grab: stubGrab{}}
	_, _, err := pipe.startVideo(context.Background(), config{qualities: "360p"}, nil)
	if err == nil {
		t.Fatal("an empty ladder was built from")
	}
	if !strings.Contains(err.Error(), "no resolution") {
		t.Errorf("error is %q, want it to say nothing is on offer", err)
	}
}

func TestServedLadderRejectsWhatIsNotAResolution(t *testing.T) {
	for _, spec := range []string{"", "nope", "1080p,nope", "721p"} {
		if _, err := quality.Parse(spec); err == nil {
			t.Errorf("-qualities %q was accepted, want an error", spec)
		}
	}
}

// A mistake in the ladder has to be found before the display is touched: a
// capture, a tsnet login and a first keyframe are a lot to go through to be told
// that 721 lines is not a resolution.
func TestABadLadderIsCaughtBeforeAnythingIsStarted(t *testing.T) {
	err := run(context.Background(), config{
		mode:           "local",
		fps:            30,
		qualities:      "721p",
		previewFPS:     10,
		previewQuality: 45,
	})
	if err == nil {
		t.Fatal("an odd height was accepted")
	}
	if !strings.Contains(err.Error(), "odd") {
		t.Errorf("error is %q, want the complaint about an odd number of lines", err)
	}
}

// The ladder is only worth anything if a rung nobody is watching costs nothing,
// so this is the part that is easy to get wrong: a viewer on one resolution
// must start that rung and leave the other three stopped.
func TestOnlyTheRungSomebodyAskedForRuns(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jpeg := stream.NewHub()
	pipe := &pipeline{
		grab:     stubGrab{},
		jpeg:     jpeg,
		preview:  testPreview(t, jpeg, 10),
		mjpegFPS: 10,
		capFPS:   60,
	}
	go pipe.preview.Run(ctx)
	ladder := servedLadder(ladderOf(t, "360p,720p,1080p"),
		quality.Capture{Width: 1920, Height: 1080}, 0)
	cfg := config{
		codec:     "vp8",
		maxFPS:    30,
		minFPS:    5,
		bitrate:   4000,
		keyint:    2,
		fps:       60,
		quality:   60,
		qualities: "360p,720p,1080p",
	}
	if _, _, err := pipe.startVideo(ctx, cfg, ladder); err != nil {
		t.Fatal(err)
	}
	defer pipe.stop()
	if len(pipe.levels) != 3 {
		t.Fatalf("built %d rungs, want 3", len(pipe.levels))
	}
	// The budget is the whole share and the top rung is the best picture, so the
	// top of the ladder gets all of it and the small ones get the fraction of it
	// that their pixels are worth.
	if got, want := pipe.rung("1080p").enc.Bitrate(), 4_000_000; got != want {
		t.Errorf("1080p was given %d bit/s of the budget, want %d", got, want)
	}
	if got, want := pipe.rung("360p").enc.Bitrate(), 444_444; got != want {
		t.Errorf("360p was given %d bit/s of the budget, want %d", got, want)
	}

	pipe.apply(map[string]int{"720p": 1})
	if !pipe.rung("720p").enc.Running() {
		t.Error("the rung the viewer asked for is not running")
	}
	for _, name := range []string{"360p", "1080p"} {
		if pipe.rung(name).enc.Running() {
			t.Errorf("%s is running with nobody watching it", name)
		}
	}
	// The captures are shared, so one viewer on one rung is what decides how
	// fast they are all taken. The fallback is a subscriber of the captures and
	// the rungs are too, and none of them is a viewer: counting them would pin
	// the capture rate at the ceiling for as long as the process ran. The
	// subscriptions happen on other goroutines, so the count settles first.
	waitFor(t, func() bool { return jpeg.ViewerCount() == len(pipe.levels)+1 })
	pipe.apply(map[string]int{"720p": 1})
	if got := int(pipe.fps.Load()); got != 30 {
		t.Errorf("capture rate is %d, want the 30 fps the rung asked for", got)
	}

	// A viewer of the endpoint is served the reduced copy at its own fixed rate,
	// and here that is slower than the rung, so the rung still decides.
	_, cancelMJPEG := pipe.preview.Hub().Subscribe(ctx)
	pipe.apply(map[string]int{"720p": 1})
	if got := int(pipe.fps.Load()); got != 30 {
		t.Errorf("capture rate is %d with a fallback viewer, want the 30 fps the rung asked for", got)
	}
	cancelMJPEG()
	pipe.apply(map[string]int{"720p": 1})
	if got := int(pipe.fps.Load()); got != 30 {
		t.Errorf("capture rate is %d once the fallback viewer left, want 30", got)
	}
}

// -max-fps is not a way to get more frames than the captures supply, and the
// number the encoder is told is not a cosmetic one: it is what every frame's
// share of the bitrate is worked out from, so a rung told 60 while the captures
// run at 24 spends its bitrate over twice the time it is sending.
func TestTheEncoderIsToldTheRateTheCapturesCanDeliver(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jpeg := stream.NewHub()
	pipe := &pipeline{
		grab:     stubGrab{},
		jpeg:     jpeg,
		preview:  testPreview(t, jpeg, 10),
		mjpegFPS: 10,
		capFPS:   24,
	}
	cfg := config{
		codec:     "vp8",
		maxFPS:    60,
		minFPS:    5,
		bitrate:   4000,
		keyint:    2,
		fps:       24,
		quality:   60,
		qualities: "720p",
	}
	ladder := servedLadder(ladderOf(t, "720p"), quality.Capture{Width: 1920, Height: 1080}, 0)
	if _, _, err := pipe.startVideo(ctx, cfg, ladder); err != nil {
		t.Fatal(err)
	}
	defer pipe.stop()

	rung := pipe.rung("720p")
	if got := rung.enc.Rate(); got != 24 {
		t.Errorf("the encoder was told %d fps, want the 24 the captures are taken at", got)
	}
	if fps, _ := rung.rate.Target(); fps != 24 {
		t.Errorf("the controller aims at %d fps, want the same 24", fps)
	}
}

// The signalling server turns a viewer away for a level that is not on offer,
// and that refusal only reaches production if this end hands back nothing for
// such a name. Handing the top rung over instead would count the viewer under a
// level the control loop has never heard of, so the rung actually being fed
// would see nobody watching it and stop its encoder.
func TestAnUnknownRungIsRefusedRatherThanServedAsAnother(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jpeg := stream.NewHub()
	pipe := &pipeline{
		grab:     stubGrab{},
		jpeg:     jpeg,
		preview:  testPreview(t, jpeg, 10),
		mjpegFPS: 10,
		capFPS:   60,
	}
	ladder := servedLadder(ladderOf(t, "360p,720p,1080p"),
		quality.Capture{Width: 1920, Height: 1080}, 0)
	cfg := config{
		codec:     "vp8",
		maxFPS:    30,
		minFPS:    5,
		bitrate:   4000,
		keyint:    2,
		fps:       60,
		quality:   60,
		qualities: "360p,720p,1080p",
	}
	if _, _, err := pipe.startVideo(ctx, cfg, ladder); err != nil {
		t.Fatal(err)
	}
	defer pipe.stop()

	// The names are the ones the signalling server passes: it folds case and
	// space away before asking, so these are already the bare spelling. The
	// empty one is an offer with no quality in it at all.
	for _, name := range []string{"", "4320p", "721p"} {
		if got := pipe.stream(name); got.Frames != nil || got.Codec != nil {
			t.Errorf("stream(%q) was served, want the zero stream that means not on offer", name)
		}
	}
	if got := pipe.stream("720p"); got.Frames == nil || got.Codec == nil {
		t.Error("a rung on offer came back as the zero stream, so every offer would be refused")
	}

	// And through the signalling server itself, with this wiring in place of
	// the one its own tests use: the refusal is the package's, but reaching it
	// is this function's job.
	srv, err := rtc.New(ctx, rtc.Config{Streams: pipe.stream})
	if err != nil {
		t.Fatalf("rtc.New: %v", err)
	}
	defer srv.Close()
	body := `{"type":"offer","quality":"4320p","sdp":"v=0"}`
	req := httptest.NewRequest(http.MethodPost, "/webrtc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a viewer asking for 4320p got status %d, want %d: %s",
			rec.Code, http.StatusBadRequest, rec.Body)
	}
}

// The ladder is not the only thing the encoders are built from, and the flags
// that decide what they are built with are read in startVideo: after the
// display has been probed and after setupTailnet. A mistake in one of them is
// the same complaint at the same price, and with -webrtc off nothing would read
// them at all, so they are checked here, before anything is started.
func TestTheWebRTCFlagsAreCaughtBeforeAnythingIsStarted(t *testing.T) {
	base := config{
		mode:           "local",
		fps:            30,
		qualities:      "360p",
		previewFPS:     10,
		previewQuality: 45,
		codec:          "auto",
		maxFPS:         60,
		minFPS:         5,
		bitrate:        4000,
		keyint:         2,
	}
	tests := []struct {
		name  string
		wrong func(*config)
		want  string
	}{
		{"codec", func(c *config) { c.codec = "vp10" }, "unknown codec"},
		{"max-fps above the range", func(c *config) { c.maxFPS = 61 }, "-max-fps must be between 1 and 60"},
		{"max-fps below the range", func(c *config) { c.maxFPS = 0 }, "-max-fps must be between 1 and 60"},
		{"min-fps above the range", func(c *config) { c.minFPS = 600 }, "-min-fps must be between 1 and 60"},
		{"bitrate above what an encoder accepts", func(c *config) { c.bitrate = 60_000 }, "-bitrate must be between 0 and 50000"},
		{"keyint above the range", func(c *config) { c.keyint = 11 }, "-keyint must be between 0 and 10"},
		// -fps is the ceiling on the frame rate, whatever -max-fps says, so a
		// floor above it is the same failure the controller would report once
		// the capture and the login had already been paid for.
		{"min-fps above -fps", func(c *config) { c.minFPS = 40 }, "-min-fps must not be above"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.wrong(&cfg)
			err := run(context.Background(), cfg)
			if err == nil {
				t.Fatal("no error, want the flag complained about before anything is started")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error is %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// The endpoint is served a reduced copy of the captures at its own rate, and that
// rate is all it should ever ask of the capture loop: it is a placeholder, and a
// placeholder is not worth a capture rate. This is the case with no rung in it
// at all, where nothing else is asking for frames.
func TestTheEndpointRunsTheCapturesAtItsOwnRate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jpeg := stream.NewHub()
	pipe := &pipeline{
		grab:     stubGrab{},
		jpeg:     jpeg,
		preview:  testPreview(t, jpeg, 10),
		mjpegFPS: 10,
		capFPS:   30,
	}

	pipe.apply(nil)
	if got := int(pipe.fps.Load()); got != 0 {
		t.Errorf("capture rate is %d with nobody watching, want 0", got)
	}

	_, cancelViewer := pipe.preview.Hub().Subscribe(ctx)
	pipe.apply(nil)
	if got := int(pipe.fps.Load()); got != 10 {
		t.Errorf("capture rate is %d for an endpoint viewer, want the 10 fps the endpoint is served at", got)
	}
	cancelViewer()
	pipe.apply(nil)
	if got := int(pipe.fps.Load()); got != 0 {
		t.Errorf("capture rate is %d once the viewer left, want 0", got)
	}
}

// The counts handed to the control loop are the whole count and not a change to
// it, so only the newest one is worth reading. A viewer that connects and goes
// again between two reads reports twice into a one-slot queue, and if the second
// report is the one dropped the rung keeps encoding and the captures keep
// running with an empty room: the room going empty is the last thing that will
// ever be said about it, so nothing corrects it afterwards.
func TestOnlyTheNewestViewerCountIsLeftWaiting(t *testing.T) {
	pipe := &pipeline{viewers: make(chan map[string]int, 1)}
	pipe.reportViewers(map[string]int{"720p": 1})
	pipe.reportViewers(map[string]int{"360p": 2})
	pipe.reportViewers(map[string]int{})
	if got := <-pipe.viewers; len(got) != 0 {
		t.Errorf("the control loop was left %v, want the empty room the last viewer left", got)
	}
}

// Viewers come and go on whichever goroutine finished its handshake or its
// connection, so more than one report can be on its way at once. None of them may
// be lost silently or left blocked on the one slot: the newest has to end up
// waiting, and every reporter has to be through.
func TestEveryoneGetsTheirViewerCountReported(t *testing.T) {
	pipe := &pipeline{viewers: make(chan map[string]int, 1)}
	const reporters = 32
	done := make(chan struct{}, reporters)
	for i := range reporters {
		go func() {
			pipe.reportViewers(map[string]int{"720p": i})
			done <- struct{}{}
		}()
	}
	for range reporters {
		<-done
	}
	if got := <-pipe.viewers; got["720p"] < 0 || got["720p"] >= reporters {
		t.Errorf("the control loop was left with %v, which is not a count anybody reported", got)
	}
	if len(pipe.viewers) != 0 {
		t.Error("a report is still waiting after the newest one landed")
	}
}

// H.264 cannot be described until the encoder has put out a keyframe, so a
// viewer arriving while nothing else is being watched waits inside prepare for
// frames that are only taken for rungs somebody is watching. That viewer
// cannot be reported as watching until it has an answer, so the wait itself
// has to ask for the captures: otherwise prepare sits out its whole timeout
// waiting for a keyframe that can never arrive and turns the viewer away,
// while the same page a moment later, with the fallback endpoint open, would
// have got through.
func TestPreparingAKeyframeTakesTheCapturesItNeeds(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jpeg := stream.NewHub()
	pipe := &pipeline{
		grab:     stubGrab{},
		jpeg:     jpeg,
		preview:  testPreview(t, jpeg, 10),
		mjpegFPS: 10,
		capFPS:   60,
		wake:     make(chan struct{}),
		capErr:   &errLogger{what: "capture"},
	}
	ladder := servedLadder(ladderOf(t, "720p"), quality.Capture{Width: 1920, Height: 1080}, 0)
	cfg := config{
		codec:     "h264",
		maxFPS:    30,
		minFPS:    5,
		bitrate:   4000,
		keyint:    2,
		fps:       60,
		quality:   60,
		qualities: "720p",
	}
	if _, _, err := pipe.startVideo(ctx, cfg, ladder); err != nil {
		t.Fatal(err)
	}
	defer pipe.stop()

	// Nobody is watching, so nothing is being captured - and a viewer that has
	// not been counted yet looks exactly like that.
	pipe.apply(nil)
	if got := int(pipe.fps.Load()); got != 0 {
		t.Fatalf("capture rate is %d with nobody watching, want 0", got)
	}
	if ready := pipe.rung("720p").enc.Codec().Ready; ready {
		t.Fatal("the codec describes itself before any keyframe, so nothing would be prepared")
	}

	go pipe.controlLoop(ctx)
	go captureLoop(ctx, pipe)

	prepared := make(chan error, 1)
	go func() { prepared <- pipe.stream("720p").Prepare(ctx) }()

	// The captures are running for the viewer that is only waiting to be
	// answered, and stay running for as long as it waits.
	waitFor(t, func() bool { return int(pipe.fps.Load()) > 0 })
	if err := <-prepared; err != nil {
		t.Fatalf("preparing the codec: %v", err)
	}
}

// waitFor gives a condition a moment to become true and fails the test if it
// never does, which is what a test needs when the thing it is waiting for is
// another goroutine starting up.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
