package encode

import (
	"os"
	"testing"
	"time"
)

func TestDumpParsedFrames(t *testing.T) {
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 20, Bitrate: 3_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer enc.Stop()
	jpeg, err := os.ReadFile("/tmp/opencode/frames/f002.jpg")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create("/tmp/opencode/parsed.vp8")
	defer f.Close()
	go func() {
		n := 0
		for fr := range enc.Frames() {
			n++
			t.Logf("frame %d key=%v len=%d head=%x", n, fr.Keyframe, len(fr.Data), fr.Data[:4])
			if n <= 3 {
				f.Write(fr.Data)
			}
			if n >= 3 {
				return
			}
		}
	}()
	for range 20 {
		if err := enc.Feed(t.Context(), jpeg); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
}
