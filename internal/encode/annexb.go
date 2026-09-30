package encode

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
)

// ffmpeg writes H.264 as a bare Annex B stream: a run of NAL units, each behind
// a start code, with no framing of any other kind. A viewer cannot be handed
// that, so the stream is split into access units, one coded picture each, with
// the sequence and picture parameter sets in front of the keyframes they
// belong to.
const (
	nalTypeSPS = 7
	nalTypePPS = 8
	nalTypeSEI = 6
	nalTypeAUD = 9
	nalTypeIDR = 5
	// startCode is the four byte form, which is what the H.264 RTP payload
	// format wants in front of a parameter set.
	startCode = "\x00\x00\x00\x01"
)

// annexBReader splits an Annex B stream into access units.
type annexBReader struct {
	src *bufio.Reader
	// started says a start code has been read and the NAL unit behind it has
	// not, so the search for the next one has to wait.
	started bool
	onSPS   func(sps, pps []byte)

	au     [][]byte // NAL units of the access unit being assembled
	slices int      // how many of them are slices
	param  [][]byte // parameter sets waiting for the next access unit
	sps    []byte
	pps    []byte
}

// newAnnexBReader returns a reader for a bare Annex B H.264 stream. onSPS is
// called with the parameter sets of every keyframe the reader sees.
func newAnnexBReader(r io.Reader, onSPS func(sps, pps []byte)) *annexBReader {
	return &annexBReader{src: bufio.NewReaderSize(r, 64<<10), onSPS: onSPS}
}

// Next returns the next access unit as an Annex B byte stream.
func (a *annexBReader) Next() (codedFrame, error) {
	for {
		nal, err := a.nextNAL()
		if err != nil {
			if err == io.EOF && a.slices > 0 {
				// The stream ended mid picture, so hand out the last access
				// unit rather than dropping it.
				return a.flush(), nil
			}
			return codedFrame{}, err
		}
		if len(nal) == 0 {
			// Nothing between two start codes.
			continue
		}
		switch nal[0] & 0x1f {
		case nalTypeSPS:
			a.sps = bytes.Clone(nal)
		case nalTypePPS:
			a.pps = bytes.Clone(nal)
			// A parameter set is only useful with its partner.
			if a.sps != nil {
				a.param = append(a.param[:0], a.sps, a.pps)
			}
		case nalTypeSEI, nalTypeAUD:
			// Metadata for the picture that comes next, which for a
			// recovery point matters to a viewer that just joined.
			a.param = append(a.param, nal)
		case 1, nalTypeIDR:
			// A new picture starts at the slice whose first macroblock is
			// number zero. The other slices of a multi slice frame start
			// further in.
			first, err := firstMacroblock(nal)
			if err != nil {
				return codedFrame{}, err
			}
			if first == 0 && a.slices > 0 {
				frame := a.flush()
				a.add(nal)
				a.slices++
				return frame, nil
			}
			a.add(nal)
			a.slices++
		default:
			a.add(nal)
		}
	}
}

// add puts a NAL unit at the end of the access unit, first draining any
// parameter sets that belong in front of it.
func (a *annexBReader) add(nal []byte) {
	if a.slices == 0 {
		a.au = append(a.au, a.param...)
		a.param = a.param[:0]
	}
	a.au = append(a.au, nal)
}

// flush returns the assembled access unit and starts a new one.
func (a *annexBReader) flush() codedFrame {
	au := a.au
	keyframe := false
	size := 0
	for _, nal := range au {
		if nal[0]&0x1f == nalTypeIDR {
			keyframe = true
		}
		size += len(nal) + len(startCode)
	}
	data := make([]byte, 0, size)
	for _, nal := range au {
		data = append(data, startCode...)
		data = append(data, nal...)
	}
	a.au, a.slices, a.param = a.au[:0], 0, a.param[:0]
	if keyframe && a.sps != nil && a.pps != nil && a.onSPS != nil {
		a.onSPS(a.sps, a.pps)
	}
	return codedFrame{data: data, keyframe: keyframe}
}

// nextNAL returns the next NAL unit with its start code removed.
func (a *annexBReader) nextNAL() ([]byte, error) {
	for {
		if !a.started {
			// Find the start code in front of the next NAL unit. Start codes
			// are three or four bytes, so two zeros and then a one find one.
			zeros := 0
			for {
				b, err := a.src.ReadByte()
				if err != nil {
					return nil, err
				}
				if b == 0 {
					zeros++
					continue
				}
				if b == 1 && zeros >= 2 {
					break
				}
				zeros = 0
			}
			a.started = true
		}
		// Read up to the start code of the next NAL unit. Zero bytes are held
		// back until it is clear they are not part of a start code, which is
		// what keeps the 00 00 03 of an escaped sequence from ending the NAL
		// unit early.
		nal := make([]byte, 0, 1024)
		zeros := 0
		for {
			b, err := a.src.ReadByte()
			if err != nil {
				if err == io.EOF && len(nal) > 0 {
					// The last NAL unit of the stream is not followed by a
					// start code.
					return trimZeros(nal), nil
				}
				return nil, err
			}
			if b == 0 {
				zeros++
				continue
			}
			if b == 1 && zeros >= 2 {
				break
			}
			for range zeros {
				nal = append(nal, 0)
			}
			zeros = 0
			nal = append(nal, b)
		}
		if len(nal) > 0 {
			// The start code of the next NAL unit has been read, so the next
			// call goes straight to the NAL unit behind it.
			return trimZeros(nal), nil
		}
		// Two start codes in a row, so there was nothing in between: start
		// looking for the next one.
		a.started = false
		// Two start codes in a row, so there was nothing in between.
	}
}

// trimZeros drops the zero bytes a start code is allowed to be padded with.
func trimZeros(nal []byte) []byte {
	for len(nal) > 0 && nal[len(nal)-1] == 0 {
		nal = nal[:len(nal)-1]
	}
	return nal
}

// firstMacroblock returns the first_macroblock_in_slice field of a slice.
func firstMacroblock(nal []byte) (int, error) {
	if len(nal) < 2 {
		return 0, fmt.Errorf("encode: truncated H.264 slice header")
	}
	v, err := (&bitReader{data: nal[1:]}).readUE()
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// parameterSetsFmtp builds the fmtp line a browser needs to be told the H.264
// sequence and picture parameter sets out of band.
func parameterSetsFmtp(sps, pps []byte) string {
	if len(sps) < 4 || len(pps) == 0 {
		return ""
	}
	// RFC 7742: the profile-level-id is the first three bytes of the sequence
	// parameter set, that is the profile, the constraint flags and the level.
	profile := fmt.Sprintf("%02X%02X%02X", sps[1], sps[2], sps[3])
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile +
		";sprop-parameter-sets=" + base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps)
}

// bitReader reads single bits, skipping the emulation prevention bytes that
// stop a start code from appearing inside a NAL unit.
type bitReader struct {
	data  []byte
	pos   int // next byte to read
	bit   uint
	zeros int
}

func (b *bitReader) readBit() (uint, error) {
	if b.pos >= len(b.data) {
		return 0, io.ErrUnexpectedEOF
	}
	bit := (b.data[b.pos] >> (7 - b.bit)) & 1
	b.bit++
	if b.bit == 8 {
		b.bit, b.pos = 0, b.pos+1
	}
	if bit == 0 {
		b.zeros++
		if b.zeros == 2 && b.pos < len(b.data) && b.data[b.pos] == 0x03 {
			b.pos, b.zeros = b.pos+1, 0
		}
	} else {
		b.zeros = 0
	}
	return uint(bit), nil
}

// readUE reads an unsigned Exp-Golomb value, the encoding slice headers use.
func (b *bitReader) readUE() (uint, error) {
	zeros := 0
	for {
		bit, err := b.readBit()
		if err != nil {
			return 0, err
		}
		if bit == 1 {
			break
		}
		if zeros++; zeros > 31 {
			return 0, fmt.Errorf("encode: H.264 slice header is malformed")
		}
	}
	value := uint(0)
	for range zeros {
		bit, err := b.readBit()
		if err != nil {
			return 0, err
		}
		value = value<<1 | bit
	}
	return 1<<zeros - 1 + value, nil
}
