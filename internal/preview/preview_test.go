package preview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"tailshare/internal/stream"
)

// jpegOf encodes img as a capture would arrive, so the tests go through the same
// decode the endpoint does rather than around it.
func jpegOf(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encode the source frame: %v", err)
	}
	return buf.Bytes()
}

// ramp is a picture with a different colour in every column, which is what makes
// a nearest neighbour downscale visible: it takes one of the colours rather than
// the average of the ones it skipped.
func ramp(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: 40, B: 200, A: 0xff})
		}
	}
	return img
}

func decoded(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

// The point of the preview is what it costs on the wire, so the first thing to
// check is that the bytes actually go down and stay a picture.
func TestThePreviewIsCheaperThanTheCapture(t *testing.T) {
	src := jpegOf(t, ramp(1920, 1080), 80)
	p, err := New(Config{Source: stream.NewHub(), Width: 640, Quality: 45, FPS: 10})
	if err != nil {
		t.Fatal(err)
	}
	data, err := p.frame(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= len(src)/4 {
		t.Errorf("preview is %d bytes against a %d byte capture, want under a quarter",
			len(data), len(src))
	}
	img := decoded(t, data)
	if got := img.Bounds().Size(); got.X != 640 || got.Y != 360 {
		t.Errorf("preview is %v, want 640x360", got)
	}
}

func TestPreviewDefaultsAreTheCheapOnes(t *testing.T) {
	p, err := New(Config{Source: stream.NewHub()})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Width != 0 || p.cfg.Quality != 45 || p.cfg.FPS != 10 {
		t.Errorf("defaults are width %d quality %d fps %d, want 0, 45 and 10",
			p.cfg.Width, p.cfg.Quality, p.cfg.FPS)
	}
}

func TestPreviewRejectsNonsense(t *testing.T) {
	for _, cfg := range []Config{
		{Width: 1},
		{Source: stream.NewHub(), Width: -1},
		{Source: stream.NewHub(), Quality: 101},
		{Source: stream.NewHub(), Quality: -1},
		{Source: stream.NewHub(), FPS: 61},
		{Source: stream.NewHub(), FPS: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) was accepted, want an error", cfg)
		}
	}
}

// Averaging is the whole reason this exists: a preview that picks one source
// pixel per destination pixel is a quarter resolution of confetti as far as text
// is concerned, which is the one thing it must not be.
func TestShrinkingAveragesRatherThanPicks(t *testing.T) {
	img := shrink(ramp(8, 4), 4)
	if got := img.Bounds().Size(); got.X != 4 || got.Y != 2 {
		t.Fatalf("shrunk to %v, want 4x2", got)
	}
	// Each destination pixel covers two source columns, so the red plane is the
	// mean of the two it spans rather than either of them. The mean is taken on
	// the eight bit samples, which is where the encoder's own conversion puts
	// the picture too.
	r, _, _, _ := img.At(0, 0).RGBA()
	if want := uint32((0 + 31) / 2 * 0x101); r != want {
		t.Errorf("red is %d, want the average %d of the two source columns", r, want)
	}
	// Nothing may be left black: an uncovered pixel is a hole in the picture.
	for y := range 2 {
		for x := range 4 {
			if r, _, _, _ := img.At(x, y).RGBA(); r == 0 {
				t.Errorf("pixel %d,%d was never written", x, y)
			}
		}
	}
}

func TestShrinkingNeverUpscales(t *testing.T) {
	img := shrink(ramp(4, 4), 640)
	if got := img.Bounds().Size(); got.X != 4 || got.Y != 4 {
		t.Errorf("a 4x4 source came out as %v", got)
	}
}

func TestShrinkingKeepsTheAspectRatio(t *testing.T) {
	for _, c := range []struct{ w, h, want int }{
		{1920, 1080, 562},
		{1280, 800, 625},
		{1024, 1024, 1000},
	} {
		img := shrink(ramp(c.w, c.h), 1000)
		if got := img.Bounds().Size(); got.X != 1000 || got.Y != c.want {
			t.Errorf("%dx%d shrunk to %v, want 1000x%d", c.w, c.h, got, c.want)
		}
	}
}

// One pixel wide is the degenerate case that a floor exists for: the span of a
// destination pixel has to be at least one source pixel or the average divides
// by nothing.
func TestShrinkingToNothing(t *testing.T) {
	img := shrink(ramp(4, 4), 1)
	if got := img.Bounds().Size(); got.X != 1 || got.Y != 1 {
		t.Errorf("shrunk to %v, want 1x1", got)
	}
}

func TestSpansCoverEverySourcePixelAtLeastOnce(t *testing.T) {
	for _, c := range []struct{ n, m int }{{1, 1}, {1, 400}, {400, 1}, {3, 3}, {10, 4}, {4, 10}} {
		s := spans(c.n, c.m)
		if len(s) != c.n {
			t.Fatalf("spans(%d, %d) made %d spans", c.n, c.m, len(s))
		}
		if s[0].lo != 0 {
			t.Errorf("spans(%d, %d) starts at %d, want 0", c.n, c.m, s[0].lo)
		}
		for i, sp := range s {
			if sp.hi <= sp.lo {
				t.Errorf("spans(%d, %d)[%d] is empty: %+v", c.n, c.m, i, sp)
			}
			if i > 0 && sp.lo < s[i-1].lo {
				t.Errorf("spans(%d, %d)[%d] goes backwards: %+v after %+v", c.n, c.m, i, sp, s[i-1])
			}
		}
		if last := s[len(s)-1]; last.hi != c.m {
			t.Errorf("spans(%d, %d) stops at %d, want %d", c.n, c.m, last.hi, c.m)
		}
	}
}

// A reduction is a partition: every source pixel belongs to exactly one
// destination pixel, or the picture is being sampled rather than averaged.
func TestReducingPartitionsTheSource(t *testing.T) {
	s := spans(10, 100)
	for i, sp := range s {
		if sp.lo != i*10 || sp.hi != (i+1)*10 {
			t.Fatalf("spans(10, 100)[%d] is %+v, want %d-%d", i, sp, i*10, (i+1)*10)
		}
	}
}

// The rate is the other half of the cost, and it is the half that has to hold
// when the machine is capturing faster than the endpoint can serve. Thirty
// captures at 50 fps is 600ms of screen, and a preview at 10 fps is six
// pictures of it however fast the captures arrive.
func TestThePreviewRateIsACeilingNotAQueue(t *testing.T) {
	for _, c := range []struct {
		fps  int
		step time.Duration
		want int
	}{
		{10, 20 * time.Millisecond, 6},  // 600ms of screen, one preview per 100ms
		{10, 5 * time.Millisecond, 2},   // 150ms of screen, and one preview left over
		{30, 10 * time.Millisecond, 8},  // 300ms of screen, and the 33ms a preview apart rounds down
		{1, 100 * time.Millisecond, 3},  // 3s of screen, one preview per second
		{60, 20 * time.Millisecond, 30}, // never the bottleneck
	} {
		p, err := New(Config{Source: stream.NewHub(), FPS: c.fps})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		got := 0
		for range 30 {
			now = now.Add(c.step)
			if p.due(now) {
				got++
			}
		}
		if got != c.want {
			t.Errorf("at %d fps, %d captures every %v published %d, want %d",
				c.fps, 30, c.step, got, c.want)
		}
	}
}

// The first frame is not held back waiting for a rate to establish itself: a
// viewer who has just connected should not stare at an empty page for a
// tenth of a second because nothing has been published yet.
func TestTheFirstFrameGoesStraightOut(t *testing.T) {
	p, err := New(Config{Source: stream.NewHub(), FPS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !p.due(time.Now()) {
		t.Error("the first frame was held back")
	}
}

func TestRunServesTheViewerThatIsThere(t *testing.T) {
	src := stream.NewHub()
	p, err := New(Config{Source: src, Width: 320, FPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	ch, unsub := p.Hub().Subscribe(ctx)
	defer unsub()

	frame := jpegOf(t, ramp(320, 180), 60)
	src.Publish(frame)
	select {
	case got := <-ch:
		img := decoded(t, got.Data)
		if got.Seq == 0 || got.Time.IsZero() {
			t.Errorf("the frame was published as %+v, want it stamped and numbered", got)
		}
		if size := img.Bounds().Size(); size.X != 320 || size.Y != 180 {
			t.Errorf("the preview is %v, want the 320x180 that went in", size)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the viewer was never sent anything")
	}
}

// A preview that decoded frames nobody was looking at would spend a core of the
// sharer's machine on a stream with no viewers, which is the one thing this is
// not allowed to cost.
func TestRunCostsNothingWithoutViewers(t *testing.T) {
	src := stream.NewHub()
	p, err := New(Config{Source: src, Width: 320, FPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	frame := jpegOf(t, ramp(320, 180), 60)
	for range 20 {
		src.Publish(frame)
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := p.Hub().Latest(); ok {
		t.Error("a preview was published with nobody watching")
	}
}

func TestFrameSurvivesRubbish(t *testing.T) {
	p, err := New(Config{Source: stream.NewHub()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.frame([]byte("not a jpeg")); err == nil {
		t.Error("a capture that is not a JPEG was accepted")
	}
}

func TestStringSaysWhatIsOnOffer(t *testing.T) {
	p, err := New(Config{Source: stream.NewHub(), Width: 640, FPS: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.String(), "mjpeg preview 10 fps at 640 px wide, quality 45"; got != want {
		t.Errorf("String() is %q, want %q", got, want)
	}
	full, err := New(Config{Source: stream.NewHub(), Width: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got := full.String(); got != "mjpeg preview 10 fps at the captured size, quality 45" {
		t.Errorf("String() is %q", got)
	}
}
