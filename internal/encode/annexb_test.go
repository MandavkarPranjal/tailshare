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

// bitCount reads a unit to the end and reports how many bits came out of it,
// which is what tells a unit with an emulation prevention byte in it from the
// same unit without one: the bytes in between are not bits anybody wanted.
func bitCount(data []byte) int {
	b := &bitReader{data: data}
	bits := 0
	for {
		if _, err := b.readBit(); err != nil {
			return bits
		}
		bits++
	}
}

// A 0x03 between two zero bytes is the encoder keeping a start code out of the
// unit, so the bits behind it are the ones in the byte that follows it. A 0x03
// with anything else in front of it is a byte of the unit like any other, and
// dropping that one takes a byte of the middle of a header out of the picture.
func TestEmulationPreventionBytesAreDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want int
	}{
		{"escaped", []byte{0x00, 0x00, 0x03, 0x80}, 24},
		{"escaped after a longer run of zeros", []byte{0x00, 0x00, 0x00, 0x03, 0x80}, 32},
		{"a 0x03 with nothing in front of it", []byte{0x03, 0x80}, 16},
		{"a 0x03 after one zero", []byte{0x00, 0x03, 0x80}, 24},
		{"a 0x03 after something else", []byte{0x80, 0x00, 0x03}, 24},
	} {
		if got := bitCount(tc.data); got != tc.want {
			t.Errorf("%s: read %d bits, want %d", tc.name, got, tc.want)
		}
	}
}

// first_macroblock_in_slice is the field the access units are split on, and the
// bytes a slice header is read out of can hold a 0x03 either as a byte of its
// own or as the encoder's escape. Read as one, the byte is dropped part way
// through and every bit behind it is read out of step, which is a picture split
// in the wrong place. The escaped case needs a field wide enough for the escape
// to fall inside it, hence the ridiculous macroblock count: what is under test
// is the arithmetic, not a header anybody would be sent.
func TestASliceHeaderIsReadThrough03Bytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// 0x03 0x00 is the header of a slice starting at macroblock 95
		// (000000 1 100000), and 0x00 0x00 0x80 0x00 0x00 0x00 with the 0x03
		// put back into it is the header of one starting at 16777215.
		nal  []byte
		want int
	}{
		{"a 0x03 of its own", []byte{0x41, 0x03, 0x00, 0x11, 0x00}, 95},
		{"an escaped 0x03", []byte{0x41, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00, 0x03, 0x00}, 16777215},
	} {
		got, err := firstMacroblock(tc.nal)
		if err != nil {
			t.Errorf("%s: firstMacroblock(%x): %v", tc.name, tc.nal, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: first macroblock is %d, want %d", tc.name, got, tc.want)
		}
	}
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

// TestMetadataAheadOfTheParameterSetsSurvives covers the order ffmpeg writes:
// the access unit delimiter goes in front of the parameter sets, and the
// recovery point behind them. The parameter sets join the queue for the picture
// rather than taking it over, so the delimiter is still in the access unit the
// viewer is handed.
func TestMetadataAheadOfTheParameterSetsSurvives(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0x00, 0x00, 0x03, 0x00, 0x04}
	pps := []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}
	aud := []byte{0x09, 0x10}
	sei := []byte{0x06, 0x05, 0x01, 0x02, 0x03, 0x04, 0x80}
	idr := slice(0x65, 0)
	r := newAnnexBReader(bytes.NewReader(annexBStream(aud, sps, pps, sei, idr)), nil)
	f, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if want := annexBUnit(aud, sps, pps, sei, idr); !bytes.Equal(f.data, want) {
		t.Errorf("access unit = %x, want %x, the delimiter in front of the parameter sets", f.data, want)
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
