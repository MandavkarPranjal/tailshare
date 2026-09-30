// Package quality names the resolutions a viewer can ask for.
//
// The screen is encoded once per rung of the ladder rather than once for the
// screen, which is what makes the choice mean anything. Scaling is something
// ffmpeg does as part of encoding, so a 360p stream is not a small copy of the
// 1080p one: it is a genuinely cheaper picture, both on the wire and on the
// machine, and the encoder spends the bitrate on fewer pixels.
//
// The rungs are deliberately independent of each other. A viewer on a slow link
// who asked for 360p has no business holding down the picture somebody else is
// watching at 1080p, so each rung is steered by its own viewers alone. The cost
// of a rung is only paid while somebody is watching that rung.
package quality

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	// MinHeight and MaxHeight bound what a level may be, in lines. The bottom
	// is a picture small enough to be worth nothing and the top is taller than
	// any display worth sharing.
	MinHeight = 144
	MaxHeight = 4320

	// minBudget is the floor on what a level is given out of the configured
	// bitrate, in bits per second. Without it the arithmetic below would walk
	// the smallest rung down to a slideshow, and a small picture that cannot be
	// followed is not a cheaper picture, it is no picture.
	minBudget = 400_000
)

// Default is the ladder a viewer is offered unless the service is told
// otherwise: a phone on a bad link up to a monitor at its own size, and nothing
// in between that most viewers would ever pick.
var Default = Ladder{
	{Name: "360p", Height: 360},
	{Name: "480p", Height: 480},
	{Name: "720p", Height: 720},
	{Name: "1080p", Height: 1080},
}

// Level is one resolution on offer. The name is what the viewer page shows and
// what travels in the signalling request; the height is what the encoder is
// told to scale the picture to.
type Level struct {
	Name   string
	Height int
}

// String returns the name, so a level reads as itself wherever a name is
// wanted.
func (l Level) String() string { return l.Name }

// Ladder is the set of levels on offer, ordered from the smallest to the
// largest. That order is the one the viewer page offers them in and the one the
// bitrate is shared out in, so it is kept rather than left to the caller.
type Ladder []Level

// ParseLevel turns a name into a level. A name is a count of picture lines with
// a p on the end, which is how resolutions are written down and how the viewer
// page labels them.
//
// The name that comes back is the canonical one rather than the one that went
// in, so `+720p` and `0720p` are the same level as `720p` and are not a second
// one. A name is what duplicates are recognised by and what a viewer is sent
// back in the signalling request, so letting two spellings of one resolution
// through would start a second encoder for a picture that is already being made.
func ParseLevel(name string) (Level, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	lines, ok := strings.CutSuffix(name, "p")
	if !ok {
		return Level{}, fmt.Errorf("quality: %q is not a resolution such as 720p", name)
	}
	height, err := strconv.Atoi(lines)
	if err != nil {
		return Level{}, fmt.Errorf("quality: %q is not a resolution such as 720p", name)
	}
	if height < MinHeight || height > MaxHeight {
		return Level{}, fmt.Errorf("quality: %d lines is out of range %d-%d", height, MinHeight, MaxHeight)
	}
	// Every encoder here emits 4:2:0, which has no odd line and no odd column:
	// ffmpeg is asked for one anyway when the picture is scaled, and the picture
	// it produces is then off by a line. Catching the name here says which
	// number is wrong instead of letting it come back as a scaling error.
	if height%2 != 0 {
		return Level{}, fmt.Errorf("quality: %d lines is odd, which 4:2:0 video cannot have", height)
	}
	return Level{Name: strconv.Itoa(height) + "p", Height: height}, nil
}

// Parse turns a comma separated list of resolutions into a ladder. The order
// they are written in does not matter and duplicates are dropped, because the
// ladder is always ordered smallest first.
func Parse(spec string) (Ladder, error) {
	var ladder Ladder
	for _, name := range strings.Split(spec, ",") {
		if strings.TrimSpace(name) == "" {
			continue
		}
		level, err := ParseLevel(name)
		if err != nil {
			return nil, err
		}
		if _, seen := ladder.Find(level.Name); seen {
			continue
		}
		ladder = append(ladder, level)
	}
	if len(ladder) == 0 {
		return nil, errors.New("quality: no resolutions given")
	}
	slices.SortFunc(ladder, func(a, b Level) int { return a.Height - b.Height })
	return ladder, nil
}

// Top is the highest level, which is what a viewer that has not chosen is sent.
// The ladder tops out at the best picture on offer rather than asking the viewer
// to know in advance which one they want. An empty ladder has no top and yields
// the zero level, so a caller that has not been given a ladder can be told what
// it asked for and say so.
func (l Ladder) Top() Level {
	if len(l) == 0 {
		return Level{}
	}
	return l[len(l)-1]
}

// Find returns the level with that name, ignoring case and surrounding space.
func (l Ladder) Find(name string) (Level, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, level := range l {
		if level.Name == name {
			return level, true
		}
	}
	return Level{}, false
}

// Names lists the level names in order, for an error message.
func (l Ladder) Names() string {
	names := make([]string, len(l))
	for i, level := range l {
		names[i] = level.Name
	}
	return strings.Join(names, ", ")
}

// Capture is the size of the frames being captured.
type Capture struct {
	Width  int
	Height int
}

// Fit returns the ladder as it can actually be served from a capture of the
// given size: the levels that are no bigger than what is being captured, in the
// order they came in. maxWidth caps it the other way round, for a caller that
// has been given a width to stay inside; 0 means no cap.
//
// A level taller than the capture would be an upscale, which costs the machine
// as much to encode as a downscale and puts a blurrier version of the picture
// on the wire, so it is not offered. There is always one level left: a screen
// too small for the whole ladder is still watchable, and the smallest rung on
// offer beats a page with nothing to choose. An empty ladder is the one case
// with nothing to fall back on, and returns empty rather than reaching past the
// end of itself.
func (l Ladder) Fit(c Capture, maxWidth int) Ladder {
	fitted := make(Ladder, 0, len(l))
	for _, level := range l {
		if !level.fits(c, maxWidth) {
			continue
		}
		fitted = append(fitted, level)
	}
	if len(fitted) == 0 && len(l) > 0 {
		return l[:1]
	}
	return fitted
}

// fits reports whether a level is within both the capture and the width cap.
//
// The capture's shape matters: a 5:4 screen scaled to 1080 lines comes out
// wider than a 16:9 one does, so the height alone is not enough to tell whether
// a level is a downscale.
func (l Level) fits(c Capture, maxWidth int) bool {
	if c.Height > 0 && l.Height > c.Height {
		return false
	}
	if c.Width <= 0 || maxWidth <= 0 {
		return true
	}
	return float64(l.Height)*float64(c.Width)/float64(c.Height) <= float64(maxWidth)
}

// Budget is the bitrate a level gets out of a total meant for the top of the
// ladder, in bits per second.
//
// It goes with the number of pixels rather than with the height, because a
// level is a fraction of the lines and so a fraction of the picture to
// describe, and what a codec needs to describe a picture does not fall off with
// its height alone. Handing every level the same bitrate would spend as much on
// a 360p stream as on a 1080p one to get a worse picture, and spend it again
// for anybody who picked the small one.
func (l Ladder) Budget(level Level, bps int) int {
	top := l.Top()
	if bps < 1 || top.Height < 1 {
		return bps
	}
	pixels := float64(level.Height) * float64(level.Height) / float64(top.Height*top.Height)
	return max(int(float64(bps)*pixels), minBudget)
}
