package quality

import (
	"strings"
	"testing"
)

func TestDefaultLadderIsOrderedAndSpans360To1080(t *testing.T) {
	ladder := Default
	if got := ladder.Top(); got.Name != "1080p" {
		t.Fatalf("top of the default ladder is %s, want 1080p", got.Name)
	}
	if got := ladder.Top().Height; got != 1080 {
		t.Fatalf("top of the default ladder is %d lines, want 1080", got)
	}
	for i, want := range []int{360, 480, 720, 1080} {
		if ladder[i].Height != want {
			t.Errorf("ladder[%d] is %s, want %dp", i, ladder[i].Name, want)
		}
		if !strings.HasSuffix(ladder[i].Name, "p") {
			t.Errorf("ladder[%d] is named %q, want a count of lines with a p on the end", i, ladder[i].Name)
		}
	}
}

func TestParseLevel(t *testing.T) {
	level, err := ParseLevel(" 720P ")
	if err != nil {
		t.Fatalf("parsing 720p: %v", err)
	}
	if level.Name != "720p" || level.Height != 720 {
		t.Fatalf("parsed %+v, want 720p at 720 lines", level)
	}
	if got := level.String(); got != "720p" {
		t.Fatalf("Level.String is %q, want 720p", got)
	}
}

func TestParseLevelNamesTheLevelCanonically(t *testing.T) {
	// The count of lines is the level; how it was written is not. The name is
	// what duplicates are spotted by and what a viewer is sent back in the
	// signalling request, so a name has to come out the same however it went in.
	for _, name := range []string{"720p", " 720p ", "720P", "+720p", "0720p", "+0720P"} {
		level, err := ParseLevel(name)
		if err != nil {
			t.Errorf("parsing %q: %v", name, err)
			continue
		}
		if level.Name != "720p" || level.Height != 720 {
			t.Errorf("parsed %q as %+v, want 720p at 720 lines", name, level)
		}
	}
}

func TestParseLevelRejectsThingsThatAreNotResolutions(t *testing.T) {
	// 721p is a resolution somebody would reasonably ask for and 4:2:0 cannot
	// encode: the line is refused here, where the number is, rather than by the
	// encoder as a scaling failure.
	for _, name := range []string{"", "720", "p", "big", "1440x900", "0p", "-360p", "4331p", "721p", "1441p"} {
		if _, err := ParseLevel(name); err == nil {
			t.Errorf("ParseLevel(%q) was accepted, want an error", name)
		}
	}
}

func TestParseOrdersAndDeduplicates(t *testing.T) {
	ladder, err := Parse("1080p, 360p,720P, 480p,360p")
	if err != nil {
		t.Fatalf("parsing ladder: %v", err)
	}
	if got := ladder.Names(); got != "360p, 480p, 720p, 1080p" {
		t.Fatalf("parsed %q, want 360p, 480p, 720p, 1080p", got)
	}
	if got := ladder.Top().Name; got != "1080p" {
		t.Fatalf("top of the parsed ladder is %s, want 1080p", got)
	}
}

func TestParseDropsRespellingsOfOneResolution(t *testing.T) {
	// Every rung is an ffmpeg of its own, fed the full size capture, so a second
	// spelling of a rung that is already there is not a duplicate in a list: it
	// is a second encoder making a picture that is already being made, under a
	// name the viewer page would offer as a separate choice.
	ladder, err := Parse("720p, +720p, 0720p, 0720P")
	if err != nil {
		t.Fatalf("parsing ladder: %v", err)
	}
	if len(ladder) != 1 || ladder.Names() != "720p" {
		t.Fatalf("parsed %q into %d rungs, want 720p alone", ladder.Names(), len(ladder))
	}
}

func TestParseRejectsEmptyAndUnknown(t *testing.T) {
	for _, spec := range []string{"", "  ", ",", "360p,nope", "nope"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) was accepted, want an error", spec)
		}
	}
}

func TestFind(t *testing.T) {
	if _, ok := Default.Find("720P"); !ok {
		t.Error("Find did not match 720P, want a case-insensitive match")
	}
	if level, ok := Default.Find(" 1080p "); !ok || level.Height != 1080 {
		t.Errorf("Find returned %+v, %v, want 1080p and true", level, ok)
	}
	if level, ok := Default.Find("144p"); ok {
		t.Errorf("Find matched %s, which is not on the ladder", level.Name)
	}
}

func TestFitDropsLevelsTallerThanTheCapture(t *testing.T) {
	hd := Capture{Width: 1920, Height: 1080}
	if got := Default.Fit(hd, 0).Names(); got != "360p, 480p, 720p, 1080p" {
		t.Errorf("fitting to a 1080p capture gave %q, want the whole ladder", got)
	}
	small := Capture{Width: 1280, Height: 720}
	if got := Default.Fit(small, 0).Names(); got != "360p, 480p, 720p" {
		t.Errorf("fitting to a 720p capture gave %q, want 360p, 480p, 720p", got)
	}
	tiny := Capture{Width: 640, Height: 360}
	if got := Default.Fit(tiny, 0).Names(); got != "360p" {
		t.Errorf("fitting to a 360p capture gave %q, want 360p", got)
	}
}

func TestFitKeepsAtLeastOneLevel(t *testing.T) {
	// A capture too small for the smallest rung, or a width cap too narrow to
	// hold it, would leave nothing to watch. The smallest rung is always offered
	// instead, since a page with no choice is worse than a small picture.
	flat := Default.Fit(Capture{Width: 320, Height: 200}, 0)
	if len(flat) != 1 || flat[0].Name != "360p" {
		t.Errorf("fitting to a 200 line capture gave %q, want 360p alone", flat.Names())
	}
	narrow := Default.Fit(Capture{Width: 1920, Height: 1080}, 400)
	if len(narrow) != 1 || narrow[0].Name != "360p" {
		t.Errorf("fitting to a 400 pixel width gave %q, want 360p alone", narrow.Names())
	}
}

// An empty ladder is the one case where the fallback to the smallest rung has
// nothing to fall back on. Every other entry point here takes an empty ladder as
// an answer it can give, and so does this one.
func TestFitOnAnEmptyLadderOffersNothing(t *testing.T) {
	var empty Ladder
	for _, fitted := range []Ladder{
		empty.Fit(Capture{Width: 1920, Height: 1080}, 0),
		empty.Fit(Capture{}, 0),
		Ladder{}.Fit(Capture{Width: 640, Height: 200}, 400),
	} {
		if len(fitted) != 0 {
			t.Errorf("fitting an empty ladder gave %q, want nothing", fitted.Names())
		}
	}
}

func TestFitRespectsWidthCapByShapeNotHeight(t *testing.T) {
	// The same cap, on the same level, either keeps it or drops it depending on
	// how wide the screen is: 1080 lines of a 4:3 screen is 1440 pixels, and
	// 1080 lines of a 16:10 one is 1920. So the cap cannot be a comparison on
	// the height alone.
	cap := 1450
	square := Default.Fit(Capture{Width: 1440, Height: 1080}, cap)
	if got := square.Names(); got != Default.Names() {
		t.Errorf("a 4:3 capture capped to %d wide gave %q, want the whole ladder", cap, got)
	}
	wide := Default.Fit(Capture{Width: 2560, Height: 1440}, cap)
	if got := wide.Names(); got != "360p, 480p, 720p" {
		t.Errorf("a 16:10 capture capped to %d wide gave %q, want 360p, 480p, 720p", cap, got)
	}
	// The height of the capture is unknown, so nothing is dropped.
	if got := Default.Fit(Capture{}, 0).Names(); got != Default.Names() {
		t.Errorf("fitting to an unknown capture gave %q, want the whole ladder", got)
	}
	if got := Default.Fit(Capture{Width: 1920, Height: 1080}, 0).Names(); got != Default.Names() {
		t.Errorf("a 0 width cap dropped levels, gave %q", got)
	}
	if flat := Default.Fit(Capture{}, 400); len(flat) != len(Default) {
		t.Errorf("an unknown capture with a width cap gave %q, want the whole ladder", flat.Names())
	}
}

func TestBudgetSplitsTheBitrateByPixels(t *testing.T) {
	ladder, err := Parse("360p,720p,1080p")
	if err != nil {
		t.Fatalf("parsing ladder: %v", err)
	}
	for _, tc := range []struct {
		name string
		want int
	}{
		// 1080p is the top, so it keeps the whole configured budget; the rest fall
		// with the square of the height, which is what the number of pixels does.
		// 720 lines is 4/9 of the lines and so 4/9 of the picture, not 2/3.
		{"1080p", 8_000_000},
		{"720p", 3_555_555},
		{"360p", 888_888},
	} {
		level, ok := ladder.Find(tc.name)
		if !ok {
			t.Fatalf("%s is not on the ladder", tc.name)
		}
		if got := ladder.Budget(level, 8_000_000); got != tc.want {
			t.Errorf("budget for %s is %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestBudgetHasAFloorAndSurvivesNonsense(t *testing.T) {
	ladder, err := Parse("144p,1080p")
	if err != nil {
		t.Fatalf("parsing ladder: %v", err)
	}
	small, _ := ladder.Find("144p")
	// 144 lines is a fiftieth of the pixels, which is well under the floor.
	if got := ladder.Budget(small, 8_000_000); got != minBudget {
		t.Errorf("budget for 144p is %d, want the %d floor", got, minBudget)
	}
	top := ladder.Top()
	if got := ladder.Budget(top, 0); got != 0 {
		t.Errorf("budget with no bitrate is %d, want 0", got)
	}
	if got := (Ladder{}).Budget(Level{Name: "720p", Height: 720}, 8_000_000); got != 8_000_000 {
		t.Errorf("budget on an empty ladder is %d, want the bitrate unchanged", got)
	}
}
