package encode

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// annexBStream builds an Annex B byte stream out of NAL units, alternating
// between the four and the three byte form of the start code.
func annexBStream(nals ...[]byte) []byte {
	var buf bytes.Buffer
	for i, nal := range nals {
		if i%2 == 0 {
			buf.WriteString(startCode)
		} else {
			buf.WriteString("\x00\x00\x01")
		}
		buf.Write(nal)
	}
	return buf.Bytes()
}

// annexBUnit joins NAL units the way the reader hands them out, with the four
// byte form of the start code in front of every one of them.
func annexBUnit(nals ...[]byte) []byte {
	var buf bytes.Buffer
	for _, nal := range nals {
		buf.WriteString(startCode)
		buf.Write(nal)
	}
	return buf.Bytes()
}

// slice builds a slice NAL unit of the given type whose
// first_macroblock_in_slice is first.
func slice(typ byte, first int) []byte {
	// The field is an Exp-Golomb code of first + 1: as many zeros as the code
	// is wide, then the code itself.
	code := first + 1
	var bits []byte
	for i := code; i > 1; i >>= 1 {
		bits = append(bits, '0')
	}
	bits = append(bits, '1')
	for i := code; i > 1; i >>= 1 {
		bits = append(bits, byte('0'+i&1))
	}
	out := []byte{typ}
	var cur byte
	n := 0
	for _, b := range bits {
		cur <<= 1
		if b == '1' {
			cur |= 1
		}
		if n++; n == 8 {
			out = append(out, cur)
			cur, n = 0, 0
		}
	}
	if n > 0 {
		out = append(out, cur<<(8-n))
	}
	return out
}

func TestAnnexBReader(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0x00, 0x00, 0x03, 0x00, 0x04}
	pps := []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}
	sei := []byte{0x06, 0x05, 0x01, 0x02, 0x03, 0x04, 0x80}
	idr := slice(0x65, 0)
	second := slice(0x41, 12) // another slice of the same picture
	p1 := slice(0x41, 0)
	stream := annexBStream(sps, pps, sei, idr, second, p1)

	var gotSPS, gotPPS []byte
	r := newAnnexBReader(bytes.NewReader(stream), func(s, p []byte) {
		gotSPS, gotPPS = bytes.Clone(s), bytes.Clone(p)
	})
	var frames []codedFrame
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		frames = append(frames, f)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d access units, want 2", len(frames))
	}
	if !frames[0].keyframe {
		t.Error("the first access unit is not a keyframe, want one")
	}
	if frames[1].keyframe {
		t.Error("the second access unit is a keyframe, want an inter frame")
	}
	// The keyframe carries the parameter sets and the metadata of its picture,
	// in front of the slices; the inter frame is one slice and nothing else.
	if want := annexBUnit(sps, pps, sei, idr, second); !bytes.Equal(frames[0].data, want) {
		t.Errorf("first access unit = %x, want %x", frames[0].data, want)
	}
	if want := annexBUnit(p1); !bytes.Equal(frames[1].data, want) {
		t.Errorf("second access unit = %x, want %x", frames[1].data, want)
	}
	if !bytes.Equal(gotSPS, sps) || !bytes.Equal(gotPPS, pps) {
		t.Errorf("parameter sets = %x, %x; want %x, %x", gotSPS, gotPPS, sps, pps)
	}
	if fmtp := parameterSetsFmtp(gotSPS, gotPPS); !strings.Contains(fmtp, "sprop-parameter-sets=") ||
		!strings.Contains(fmtp, "profile-level-id=64001F") {
		t.Errorf("parameterSetsFmtp = %q, want the profile and the parameter sets", fmtp)
	}
}

// TestAnnexBReaderStartsMidStream covers a stream that begins in the middle of a
// picture, which is what a viewer joining between keyframes gets. Two pictures
// of slices the reader cannot tell apart still have to come out as one access
// unit each, and neither may be taken for a keyframe.
func TestAnnexBReaderStartsMidStream(t *testing.T) {
	first := slice(0x41, 0)
	rest := slice(0x41, 12)
	r := newAnnexBReader(bytes.NewReader(annexBStream(first, rest)), nil)
	f, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.keyframe {
		t.Error("a slice without parameter sets was taken for a keyframe")
	}
	if want := annexBUnit(first, rest); !bytes.Equal(f.data, want) {
		t.Errorf("access unit = %x, want both slices of the one picture", f.data)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("Next past the end = %v, want EOF", err)
	}
}

// TestEncoderDeliversWhileStreaming is the regression test for ffmpeg holding
// the whole stream back: with probing left at its default, nothing came out
// until the input ended, so a live screen share showed frames seconds old.
//
// ffmpeg hands out a frame as it reads the one behind it, so a stream that has
// been fed a single frame has nothing to show yet. Two are enough.
func TestEncoderDeliversWhileStreaming(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	enc, err := New(Options{Codec: CodecVP8, MaxFPS: 10, Bitrate: 500_000, KeyframeInterval: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer enc.Stop()
	for i := range 2 {
		if err := enc.Feed(t.Context(), testJPEG(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case f := <-enc.Frames():
		if !f.Keyframe || len(f.Data) == 0 {
			t.Errorf("first frame = keyframe %v, %d bytes; want a keyframe with data", f.Keyframe, len(f.Data))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no frame came out while the stream was still open")
	}
}
