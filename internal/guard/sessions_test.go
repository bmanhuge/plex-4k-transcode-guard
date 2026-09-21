package guard

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mixedSessions(t *testing.T) map[string]Session {
	t.Helper()
	parsed, err := ParseSessions(fixture(t, "sessions_mixed.xml"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Session{}
	for _, s := range parsed.Items {
		byKey[s.SessionKey] = s
	}
	return byKey
}

func TestParseSessionsMixed(t *testing.T) {
	parsed, err := ParseSessions(fixture(t, "sessions_mixed.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 9 || parsed.Videos != 8 {
		t.Fatalf("counts: items=%d videos=%d", len(parsed.Items), parsed.Videos)
	}
	s := parsed.Items[0]
	if s.Kind != "Video" || s.SessionKey != "7" || s.RatingKey != "769612" || s.Title != "Example Movie UHD" || s.User != "viewer-a" || s.Live {
		t.Fatalf("item 0 basics: %+v", s)
	}
	if s.SessionID != "e6gmj1bjf7jlbz7hu5cqcags" {
		t.Fatalf("session id %q", s.SessionID)
	}
	if s.Transcode == nil || s.Transcode.VideoDecision != "transcode" || s.Transcode.AudioDecision != "transcode" || s.Transcode.Width != 1280 || s.Transcode.Height != 720 || s.Transcode.Protocol != "dash" {
		t.Fatalf("transcode: %+v", s.Transcode)
	}
	m, ok := s.SelectedMedia()
	if !ok || m.ID != "1399271" || m.Width != 1280 || m.Height != 720 || m.VideoResolution != "720p" {
		t.Fatalf("selected media: %+v ok=%v", m, ok)
	}
	p, ok := m.SelectedPart()
	if !ok || len(p.Streams) != 2 {
		t.Fatalf("selected part: %+v", p)
	}
	st, ok := p.VideoStream()
	if !ok || st.DisplayTitle != "4K (HEVC Main 10)" || st.StreamType != 1 {
		t.Fatalf("video stream: %+v", st)
	}
	if s.Player.Product != "Plex for Samsung" || s.Player.State != "playing" {
		t.Fatalf("player: %+v", s.Player)
	}
	if !s.IsVideoTranscode() {
		t.Fatal("item 0 is a video transcode")
	}

	ep := parsed.Items[2]
	if ep.DisplayTitle() != "Example Show - Episode Six" {
		t.Fatalf("display title %q", ep.DisplayTitle())
	}
	track := parsed.Items[4]
	if track.Kind != "Track" || track.IsVideoTranscode() {
		t.Fatalf("track: %+v", track)
	}
	noSession := parsed.Items[5]
	if noSession.SessionID != "" {
		t.Fatalf("expected no session id, got %q", noSession.SessionID)
	}
}

func TestParseSessionsEmpty(t *testing.T) {
	parsed, err := ParseSessions(fixture(t, "sessions_empty.xml"))
	if err != nil || parsed.Videos != 0 || len(parsed.Items) != 0 {
		t.Fatalf("got %+v err=%v", parsed, err)
	}
}

func TestParseSessionsRejectsGarbage(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":     "",
		"html":      "<html><body>nope</body></html>",
		"truncated": `<MediaContainer size="1"><Video sessionKey="1">`,
		"binary":    "\x00\x01\x02",
	} {
		if _, err := ParseSessions([]byte(doc)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestParseMetadataMedia(t *testing.T) {
	sources, err := ParseMetadataMedia(fixture(t, "metadata_800001.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].ID != "9001" || sources[0].Width != 1920 || sources[1].ID != "9002" || sources[1].Height != 2160 || sources[1].VideoResolution != "4k" {
		t.Fatalf("sources: %+v", sources)
	}
	if sources[0].IsUHD() || !sources[1].IsUHD() {
		t.Fatal("UHD flags wrong")
	}
	if sources[1].Dimensions() != "3840x2160" || (SourceMedia{}).Dimensions() != "?" {
		t.Fatal("dimensions rendering")
	}
	if _, err := ParseMetadataMedia([]byte(`<MediaContainer size="0"></MediaContainer>`)); err == nil {
		t.Fatal("no items must be an error")
	}
	if _, err := ParseMetadataMedia([]byte(`nope`)); err == nil {
		t.Fatal("garbage must be an error")
	}
}

func TestIsUHD(t *testing.T) {
	cases := []struct {
		w, h int
		res  string
		want bool
	}{
		{3840, 2160, "4k", true},
		{3840, 1600, "4k", true},
		{3840, 1600, "", true},
		{3840, 2160, "", true},
		{4096, 1716, "4k", true},
		{4096, 1716, "", true},
		{2880, 2160, "", true},
		{3840, 1080, "1080", false}, // full side-by-side 3D 1080p rip
		{1920, 2160, "1080", false}, // full over-under 3D 1080p rip
		{0, 2160, "", false},
		{3840, 0, "", false},
		{0, 0, "4k", true},
		{0, 0, "4K", true},
		{0, 0, "2160p", true},
		{0, 0, "2160", true},
		{0, 0, "uhd", true},
		{1920, 1080, "1080", false},
		{1920, 1080, "1080p", false},
		{2560, 1440, "1440p", false},
		{3839, 2159, "", false},
		{0, 0, "", false},
		{0, 0, "sd", false},
		{1280, 720, "720p", false},
	}
	for _, tc := range cases {
		if got := IsUHD(tc.w, tc.h, tc.res); got != tc.want {
			t.Errorf("IsUHD(%d,%d,%q)=%v want %v", tc.w, tc.h, tc.res, got, tc.want)
		}
	}
}

func TestNormalizeResolution(t *testing.T) {
	for in, want := range map[string]string{"1080p": "1080", "2160P": "2160", "4k": "4k", " 4K ": "4k", "sd": "sd", "480i": "480", "": ""} {
		if got := NormalizeResolution(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestScreen(t *testing.T) {
	byKey := mixedSessions(t)
	cases := []struct {
		key       string
		candidate bool
		reason    string
		mediaID   string
	}{
		{"7", true, "video-transcode", "1399271"},
		{"1", false, "direct-play", ""},
		{"16", true, "video-transcode", "1477136"},
		{"20", false, "audio-only-transcode", ""},
		{"21", false, "not-video", ""},
		{"22", true, "video-transcode", "1399271"},
		{"23", true, "video-transcode", "7001"},
		{"24", true, "video-transcode", "9001"},
		{"25", true, "video-transcode", ""},
	}
	for _, tc := range cases {
		s, ok := byKey[tc.key]
		if !ok {
			t.Fatalf("session %s missing from fixture", tc.key)
		}
		v := Screen(s)
		if v.Candidate != tc.candidate || v.Reason != tc.reason || v.MediaID != tc.mediaID {
			t.Errorf("key %s: got candidate=%v reason=%q media=%q, want %v %q %q", tc.key, v.Candidate, v.Reason, v.MediaID, tc.candidate, tc.reason, tc.mediaID)
		}
		if v.Terminate {
			t.Errorf("key %s: Screen must never decide to terminate", tc.key)
		}
	}
	// Direct stream with audio copied is reported as direct-stream.
	ds := byKey["20"]
	ds.Transcode = &Transcode{VideoDecision: "copy", AudioDecision: "copy"}
	if v := Screen(ds); v.Reason != "direct-stream" || v.Candidate {
		t.Fatalf("direct stream: %+v", v)
	}
	// Live TV transcodes are never candidates, even when video is transcoded.
	live := byKey["7"]
	live.Live = true
	if v := Screen(live); v.Reason != "live-tv" || v.Candidate {
		t.Fatalf("live tv: %+v", v)
	}
	// Padded decisions are normalised consistently by both predicates.
	padded := byKey["7"]
	padded.Transcode = &Transcode{VideoDecision: " Transcode ", AudioDecision: "copy"}
	if !padded.IsVideoTranscode() || !Screen(padded).Candidate {
		t.Fatal("padded decision must still count as a video transcode")
	}
}

func TestJudge(t *testing.T) {
	byKey := mixedSessions(t)
	meta := func(name string) []SourceMedia {
		sources, err := ParseMetadataMedia(fixture(t, name))
		if err != nil {
			t.Fatal(err)
		}
		return sources
	}
	type want struct {
		terminate bool
		reason    string
		sourceID  string
	}
	cases := []struct {
		name    string
		session Session
		sources []SourceMedia
		want    want
	}{
		{"4K source transcode", byKey["7"], meta("metadata_769612.xml"), want{true, "4k-transcode", "1399271"}},
		{"1080p source transcode", byKey["16"], meta("metadata_786405.xml"), want{false, "source-not-4k", "1477136"}},
		{"4K output from 1080p source is not 4K", byKey["23"], meta("metadata_700001.xml"), want{false, "source-not-4k", "7001"}},
		{"selected 1080p version of a two-version item", byKey["24"], meta("metadata_800001.xml"), want{false, "source-not-4k", "9001"}},
		{"unselected media, every version 4K", byKey["25"], meta("metadata_900001.xml"), want{true, "4k-transcode", ""}},
		{"unselected media, mixed versions", byKey["25"], meta("metadata_900001_mixed.xml"), want{false, "ambiguous-source", ""}},
		{"no metadata media", byKey["7"], nil, want{false, "source-unknown", ""}},
		{"unmatched id, sole 4K version", byKey["16"], meta("metadata_769612.xml"), want{false, "media-not-found", ""}},
		{"unmatched id, sole 1080p version", byKey["7"], meta("metadata_786405.xml"), want{false, "media-not-found", ""}},
		{"unmatched id, mixed versions", byKey["7"], meta("metadata_800001.xml"), want{false, "media-not-found", ""}},
		{"no selected media, sole 4K version", noSelected(byKey["7"]), meta("metadata_769612.xml"), want{true, "4k-transcode", "1399271"}},
		{"no selected media, sole 1080p version", noSelected(byKey["7"]), meta("metadata_786405.xml"), want{false, "source-not-4k", "1477136"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Judge(Screen(tc.session), tc.sources)
			if v.Terminate != tc.want.terminate || v.Reason != tc.want.reason {
				t.Fatalf("got terminate=%v reason=%q detail=%q", v.Terminate, v.Reason, v.Detail)
			}
			gotID := ""
			if v.Source != nil {
				gotID = v.Source.ID
			}
			if gotID != tc.want.sourceID {
				t.Fatalf("source id %q, want %q", gotID, tc.want.sourceID)
			}
		})
	}

	// Selecting the 4K version of the two-version item flips the verdict.
	two := byKey["24"]
	two.Media[0].Selected = false
	two.Media[1].Selected = true
	v := Judge(Screen(two), meta("metadata_800001.xml"))
	if !v.Terminate || v.Reason != "4k-transcode" || v.Source == nil || v.Source.ID != "9002" {
		t.Fatalf("selected 4K version: %+v", v)
	}

	// Judge must never promote a non-candidate.
	dp := Judge(Screen(byKey["1"]), meta("metadata_769612.xml"))
	if dp.Terminate || dp.Candidate {
		t.Fatalf("direct play must stay untouched: %+v", dp)
	}
}

// noSelected returns a copy of s whose media list has no selected entry and
// more than one candidate, so Screen yields an empty MediaID.
func noSelected(s Session) Session {
	s.Media = []Media{{ID: "x1"}, {ID: "x2"}}
	return s
}

func TestEvidenceIsSanitized(t *testing.T) {
	byKey := mixedSessions(t)
	s := byKey["7"]
	sources, _ := ParseMetadataMedia(fixture(t, "metadata_769612.xml"))
	v := Judge(Screen(s), sources)
	ev := Evidence(s, v)
	for _, want := range []string{"source=3840x2160/4k", "transcode=video:transcode,audio:transcode", "output=1280x720", "protocol=dash", "session_media=1399271/1280x720/720p", `stream_title="4K (HEVC Main 10)"`, `player="Plex for Samsung"`, "state=playing"} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence missing %q: %s", want, ev)
		}
	}
	for _, leak := range []string{"10.0.0.5", "203.0.113.10", "aaaa-bbbb"} {
		if strings.Contains(ev, leak) {
			t.Errorf("evidence leaks %q: %s", leak, ev)
		}
	}
	if ev2 := Evidence(byKey["1"], Screen(byKey["1"])); !strings.Contains(ev2, "source=unmatched") || strings.Contains(ev2, "transcode=") {
		t.Fatalf("direct play evidence: %s", ev2)
	}
}
