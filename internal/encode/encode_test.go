package encode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsBadOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"codec", Options{Codec: "av1"}},
		{"fps too low", Options{MaxFPS: -1}},
		{"fps too high", Options{MaxFPS: 61}},
		{"bitrate too low", Options{Bitrate: 1}},
		{"bitrate too high", Options{Bitrate: 1 << 30}},
		{"keyframe interval", Options{KeyframeInterval: 20}},
		{"width", Options{Width: 10}},
		{"negative height", Options{Height: -2}},
		{"odd height", Options{Height: 361}},
		{"width and height", Options{Width: 1280, Height: 720}},
		{"missing binary", Options{Binary: "definitely-not-ffmpeg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestScaleFilter(t *testing.T) {
	// The picture is scaled as part of encoding, so a viewer who asked for
	// 360p gets a stream that was encoded at 360 lines rather than a full size
	// one the browser shrank. -2 is the other dimension, rounded to an even
	// number, which is what 4:2:0 needs.
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"captured size", Options{}, "format=yuv420p"},
		{"by width", Options{Width: 1280}, "scale=1280:-2,format=yuv420p"},
		{"by height", Options{Height: 360}, "scale=-2:360,format=yuv420p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := New(tt.opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			args := e.argsLocked()
			filter := ""
			for i, arg := range args {
				if arg == "-vf" && i+1 < len(args) {
					filter = args[i+1]
				}
			}
			if filter != tt.want {
				t.Errorf("filter is %q, want %q", filter, tt.want)
			}
		})
	}
}

func TestParseCodec(t *testing.T) {
	for _, s := range []string{"auto", "vp8", "VP9", " h264 "} {
		if _, err := ParseCodec(s); err != nil {
			t.Errorf("ParseCodec(%q) = %v, want no error", s, err)
		}
	}
	if _, err := ParseCodec("h265"); err == nil {
		t.Error("ParseCodec(\"h265\") succeeded, want an error")
	}
}

func TestCodecInfo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	vp8, err := New(Options{Codec: CodecVP8, MaxFPS: 10, Bitrate: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	info := vp8.Codec()
	if info.MimeType != "video/VP8" || info.ClockRate != 90000 {
		t.Errorf("Codec() = %+v, want video/VP8 at 90000", info)
	}
	if info.SDPFmtp != "" {
		t.Errorf("VP8 SDPFmtp = %q, want empty", info.SDPFmtp)
	}
	if !info.Ready {
		t.Error("VP8 is not ready, it should be ready right away")
	}

	vp9, err := New(Options{Codec: CodecVP9, MaxFPS: 10, Bitrate: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if got := vp9.Codec().SDPFmtp; got != "profile-id=0" {
		t.Errorf("VP9 SDPFmtp = %q, want profile-id=0", got)
	}

	// H.264 cannot describe itself until a keyframe has been encoded.
	h264, err := New(Options{Codec: CodecH264, MaxFPS: 10, Bitrate: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if info := h264.Codec(); info.Ready || info.SDPFmtp != "" {
		t.Errorf("Codec() = %+v, want an H.264 codec that is not ready yet", info)
	}

	// Asking for no codec in particular gets the one the default is.
	unset, err := New(Options{MaxFPS: 10, Bitrate: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if info := unset.Codec(); info.Codec != CodecH264 {
		t.Errorf("Codec() = %+v, want the default codec %q", info, CodecH264)
	}
	auto, err := New(Options{Codec: CodecAuto, MaxFPS: 10, Bitrate: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if info := auto.Codec(); info.Codec != CodecH264 {
		t.Errorf("Codec() = %+v, want auto to pick %q", info, CodecH264)
	}
}

// TestFeedingWhileTheEncoderStops covers the collision between a frame on its
// way in and the encoder being reconfigured or shut down: the write fails
// because the pipe is closed on purpose, which is not a fault of the frame.
func TestFeedingWhileTheEncoderStops(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 30, Bitrate: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan error, 64)
	stop := make(chan struct{})
	go func() {
		defer close(frames)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := enc.Feed(t.Context(), testJPEG(t, 1)); err != nil {
				frames <- err
				return
			}
		}
	}()
	for range 20 {
		if err := enc.SetBitrate(t.Context(), 500_000); err != nil {
			t.Fatal(err)
		}
		if err := enc.SetBitrate(t.Context(), 1_000_000); err != nil {
			t.Fatal(err)
		}
	}
	enc.Stop()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	for err := range frames {
		if err != nil && !errors.Is(err, ErrStopped) {
			t.Fatalf("Feed during a restart: %v", err)
		}
	}
}

// testJPEG returns a small JPEG that differs from the last one, so the encoder
// has something to work with.
func testJPEG(t *testing.T, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{R: uint8(x*4 + n*20), G: uint8(y * 5), B: uint8(n * 30), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestEncoderRoundTrip runs the real encoder, which is the only way to be sure
// the arguments and the demuxers agree with ffmpeg.
func TestEncoderRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	for _, codec := range []Codec{CodecVP8, CodecVP9, CodecH264} {
		t.Run(string(codec), func(t *testing.T) {
			enc, err := New(Options{Codec: codec, MaxFPS: 10, Bitrate: 500_000, KeyframeInterval: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := enc.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer enc.Stop()
			// Collect while feeding: the encoder drops frames rather than
			// queue them up, so a reader that only starts at the end of the
			// stream sees less than was encoded.
			got := make(chan []Frame, 1)
			go func() {
				var frames []Frame
				for len(frames) < 4 {
					select {
					case f := <-enc.Frames():
						frames = append(frames, f)
					case <-time.After(20 * time.Second):
						got <- frames
						return
					}
				}
				got <- frames
			}()

			for i := range 6 {
				if err := enc.Feed(t.Context(), testJPEG(t, i)); err != nil {
					t.Fatalf("Feed %d: %v", i, err)
				}
			}
			enc.Stop()
			if err := enc.Err(); err != nil {
				t.Fatalf("Err() = %v, want nil", err)
			}

			frames := <-got
			if len(frames) < 2 {
				t.Fatalf("got %d frames, want at least 2", len(frames))
			}
			if !frames[0].Keyframe {
				t.Error("the first frame is not a keyframe, want one")
			}
			var last time.Time
			for i, f := range frames {
				if len(f.Data) == 0 {
					t.Errorf("frame %d is empty", i)
				}
				if f.Duration <= 0 {
					t.Errorf("frame %d has duration %v, want a positive one", i, f.Duration)
				}
				if f.Time.Before(last) {
					t.Errorf("frame %d is at %v, before the one before it at %v", i, f.Time, last)
				}
				last = f.Time
			}
			// A keyframe is where the H.264 parameter sets come from, so this
			// is also the point at which the codec becomes describable.
			if info := enc.Codec(); !info.Ready {
				t.Errorf("Codec() = %+v, want a ready codec after a keyframe", info)
			} else if codec == CodecH264 && !strings.Contains(info.SDPFmtp, "sprop-parameter-sets=") {
				t.Errorf("SDPFmtp = %q, want the parameter sets", info.SDPFmtp)
			}
		})
	}
}

func TestSetBitrate(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 10, Bitrate: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if got := enc.Bitrate(); got != 1_000_000 {
		t.Errorf("Bitrate() = %d, want the options it was built with", got)
	}
	if err := enc.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer enc.Stop()
	// The rate the process was started with never moves, so that a slower
	// capture costs a lower total rate and nothing else.
	if err := enc.SetBitrate(t.Context(), 250_000); err != nil {
		t.Fatal(err)
	}
	if got := enc.Bitrate(); got != 250_000 {
		t.Errorf("Bitrate() = %d, want the new bitrate", got)
	}
	if got := enc.Rate(); got != 10 {
		t.Errorf("Rate() = %d, want the 10 the process was started with", got)
	}
	for _, bps := range []int{0, 1, 49_999, 50_000_001} {
		if err := enc.SetBitrate(t.Context(), bps); err == nil {
			t.Errorf("SetBitrate(%d) succeeded, want an error", bps)
		}
	}
	if !enc.Running() {
		t.Error("the encoder stopped after a rejected bitrate")
	}
}

// TestSetBitrateIsFreeWhenItChangesNothing is the regression test for the
// encoder restarts that used to happen on every frame rate change: asking for
// the bitrate already in use must leave the process alone.
func TestSetBitrateIsFreeWhenItChangesNothing(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 10, Bitrate: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Stop()
	// The bitrate an encoder is built with still has to start it, since
	// nothing has been started yet.
	if err := enc.SetBitrate(t.Context(), 1_000_000); err != nil {
		t.Fatal(err)
	}
	if !enc.Running() {
		t.Error("SetBitrate at the configured bitrate did not start the encoder")
	}
	// The same bitrate again is now a no-op rather than a restart.
	before := enc.SinceKeyframe()
	if err := enc.SetBitrate(t.Context(), 1_000_000); err != nil {
		t.Fatal(err)
	}
	if !enc.Running() {
		t.Error("a repeated SetBitrate stopped the encoder")
	}
	if got := enc.SinceKeyframe(); got > before {
		t.Errorf("SinceKeyframe() went from %v to %v, want a restart to have reset it", before, got)
	}
	// And after a Stop it has to start again.
	enc.Stop()
	if err := enc.SetBitrate(t.Context(), 1_000_000); err != nil {
		t.Fatal(err)
	}
	if !enc.Running() {
		t.Error("SetBitrate did not restart a stopped encoder")
	}
}

// TestKeyframesFollowTheWallClock covers a capture running well below the rate
// the encoder was told about: every frame gets the same share of the bitrate, so
// the stream is slower, and a keyframe every KeyframeInterval seconds has to
// arrive on the wall clock rather than every KeyframeInterval frames.
func TestKeyframesFollowTheWallClock(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	const (
		interval = 60 * time.Millisecond // what the capture loop manages
		frames   = 40                    // two and a bit keyframe intervals
	)
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 30, Bitrate: 1_000_000, KeyframeInterval: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer enc.Stop()
	jpeg := testJPEGSize(t, 256, 192)
	// Room for every frame, so the collector never blocks and the encoder does
	// not drop anything it is not meant to.
	out := make(chan Frame, 2*frames)
	go func() {
		for f := range enc.Frames() {
			out <- f
		}
	}()
	for range frames + 2 { // ffmpeg hands a frame out as it reads the next
		if err := enc.Feed(t.Context(), jpeg); err != nil {
			t.Fatal(err)
		}
		time.Sleep(interval)
	}
	var got []Frame
	for len(got) < frames {
		select {
		case f := <-out:
			got = append(got, f)
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of %d frames came out", len(got), frames)
		}
	}
	if !got[0].Keyframe {
		t.Error("the first frame is not a keyframe, want one")
	}
	// A second keyframe one interval of wall clock later is the whole point: a
	// frame count would have made it wait 30 frames, which is 1.8s.
	var second time.Duration
	for i, f := range got[1:] {
		if f.Keyframe {
			second = got[i+1].Time.Sub(got[0].Time)
			break
		}
	}
	if second == 0 {
		t.Error("no second keyframe within the frames fed, want one after a second")
	} else if second < 500*time.Millisecond || second > 1500*time.Millisecond {
		t.Errorf("second keyframe at %v, want about a second", second)
	}
	// The clock handed to viewers has to describe the rate actually delivered,
	// not the rate the process was started with, or the jitter buffer fills.
	median := median(durations(got[5:]))
	if median < 30*time.Millisecond || median > 150*time.Millisecond {
		t.Errorf("durations around %v, want about %v", median, interval)
	}
}

// durations returns the durations of these frames, for the clock check above.
func durations(fs []Frame) []time.Duration {
	out := make([]time.Duration, len(fs))
	for i, f := range fs {
		out[i] = f.Duration
	}
	return out
}

// TestClockSurvivesARestart covers what a restarting ffmpeg does to the clock a
// viewer builds its jitter buffer from. A new process begins its own clock at
// the wall clock, so without carrying the old one over, every restart moves the
// media clock forward by however long the restart took. The viewer sees a gap
// of that length, holds frames to cover it, and the latency never comes back
// down. A bitrate change is the only thing that restarts ffmpeg, so this is
// exactly what a viewer on a slow link would feel.
func TestClockSurvivesARestart(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 30, Bitrate: 1_000_000, KeyframeInterval: 1})
	if err != nil {
		t.Fatal(err)
	}
	jpeg := testJPEGSize(t, 256, 192)
	out := make(chan Frame, 200)
	go func() {
		for f := range enc.Frames() {
			out <- f
		}
	}()
	defer enc.Stop()

	// run feeds the encoder for a while and hands back the last frame out of
	// it, along with how far its clock is from the wall clock.
	run := func(seconds time.Duration) (Frame, time.Duration) {
		t.Helper()
		deadline := time.Now().Add(seconds)
		var last Frame
		for time.Now().Before(deadline) {
			if err := enc.Feed(t.Context(), jpeg); err != nil && !errors.Is(err, ErrStopped) {
				t.Fatal(err)
			}
			select {
			case last = <-out:
			case <-time.After(200 * time.Millisecond):
			}
			time.Sleep(30 * time.Millisecond)
		}
		return last, time.Since(last.Time)
	}

	// A restart takes about half a second to encode anything at all, so three
	// of them would leave the clock a second and a half ahead if they each
	// counted. One frame of slack either way is all a healthy clock is ever
	// off by.
	const slack = 60 * time.Millisecond
	bitrate := 1_000_000
	if err := enc.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	last, ahead := run(2 * time.Second)
	if last.Data == nil {
		t.Fatal("no frames came out of the first encoder")
	}
	if ahead < -slack || ahead > slack {
		t.Errorf("clock is %v ahead of the wall clock, want less than %v", ahead, slack)
	}
	for i := 0; i < 3; i++ {
		bitrate += 500_000
		if err := enc.SetBitrate(t.Context(), bitrate); err != nil {
			t.Fatal(err)
		}
		var drift time.Duration
		last, drift = run(2 * time.Second)
		if last.Data == nil {
			t.Fatalf("no frames came out of the encoder after %d restarts", i+1)
		}
		if drift < -slack || drift > slack {
			t.Errorf("after %d restarts the clock is %v behind the wall clock, want less than %v",
				i+1, -drift, slack)
		}
	}
}

// testJPEGSize returns a JPEG of the given size, filled with noise so that it
// does not compress to nothing.
func testJPEGSize(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x*7 + y), G: uint8(y * 11), B: uint8(x ^ y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEncoderFailsWhenStopped(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 5, Bitrate: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Feed(t.Context(), testJPEG(t, 0)); err == nil {
		t.Error("Feed on an encoder that never started succeeded, want an error")
	}
	if err := enc.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	enc.Stop()
	if err := enc.Feed(t.Context(), testJPEG(t, 0)); err == nil {
		t.Error("Feed after Stop succeeded, want an error")
	}
}

// ivfStream builds an IVF stream around the given frames.
func ivfStream(fourcc string, denom, num uint32, frames [][]byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("DKIF")
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(ivfHeaderSize))
	buf.WriteString(fourcc)
	binary.Write(&buf, binary.LittleEndian, uint16(320))
	binary.Write(&buf, binary.LittleEndian, uint16(240))
	binary.Write(&buf, binary.LittleEndian, denom)
	binary.Write(&buf, binary.LittleEndian, num)
	binary.Write(&buf, binary.LittleEndian, uint32(len(frames)))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	for i, f := range frames {
		binary.Write(&buf, binary.LittleEndian, uint32(len(f)))
		binary.Write(&buf, binary.LittleEndian, uint64(i*20))
		buf.Write(f)
	}
	return buf.Bytes()
}

func TestIVFReader(t *testing.T) {
	// Bit 0 of the first byte tells VP8 and VP9 keyframes apart.
	stream := ivfStream("VP80", 60, 1, [][]byte{
		{0x00, 0x11}, // keyframe
		{0x81, 0x22}, // inter frame
		{0x00, 0x33}, // keyframe
	})
	r, err := newIVFReader(bytes.NewReader(stream), "VP80")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		key  bool
		data []byte
	}{
		{true, []byte{0x00, 0x11}},
		{false, []byte{0x81, 0x22}},
		{true, []byte{0x00, 0x33}},
	}
	for i, w := range want {
		f, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.keyframe != w.key || !bytes.Equal(f.data, w.data) {
			t.Errorf("frame %d = key %v, data %x; want key %v, data %x", i, f.keyframe, f.data, w.key, w.data)
		}
	}
	if _, err := r.Next(); err == nil {
		t.Error("Next past the end succeeded, want an error")
	}
}

func TestIVFReaderRejects(t *testing.T) {
	tests := []struct {
		name   string
		stream []byte
		fourcc string
	}{
		{"not ivf", append(bytes.Repeat([]byte{0x7f}, ivfHeaderSize), 0x01), "VP80"},
		{"wrong codec", ivfStream("VP90", 60, 1, nil), "VP80"},
		{"zero timebase", ivfStream("VP80", 0, 1, nil), "VP80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newIVFReader(bytes.NewReader(tt.stream), tt.fourcc); err == nil {
				t.Error("newIVFReader succeeded, want an error")
			}
		})
	}
}

func TestKeyframeDetection(t *testing.T) {
	tests := []struct {
		name string
		fn   func([]byte) bool
		data []byte
		want bool
	}{
		{"vp8 key", vp8Keyframe, []byte{0x00}, true},
		{"vp8 inter", vp8Keyframe, []byte{0x81}, false},
		{"vp8 empty", vp8Keyframe, nil, false},
		{"vp9 key", vp9Keyframe, []byte{0b1000_0010}, true},
		{"vp9 inter", vp9Keyframe, []byte{0b1000_0110}, false},
		{"vp9 shown", vp9Keyframe, []byte{0b1000_1010}, false},
		{"vp9 not vp9", vp9Keyframe, []byte{0b0000_0000}, false},
		{"vp9 empty", vp9Keyframe, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(tt.data); got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}
