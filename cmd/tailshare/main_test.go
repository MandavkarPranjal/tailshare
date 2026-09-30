package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os/exec"
	"strings"
	"testing"
	"time"

	"tailshare/internal/preview"
	"tailshare/internal/quality"
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
