package guard

import (
	"fmt"
	"strings"
)

// UHD thresholds. A source is 4K when Plex labels it 4k/2160/uhd, or when
// its dimensions are UHD-sized: at least 3840 wide with at least 1600 lines
// (ultra-wide 2.40:1 UHD encodes are 3840x1600), or at least 2160 lines
// with at least 2880 columns (4:3 and 3:2 UHD). Requiring both dimensions
// keeps full side-by-side / over-under 3D 1080p rips (3840x1080, 1920x2160)
// out.
const (
	UHDMinWidth       = 3840
	UHDMinHeightWide  = 1600
	UHDMinHeight      = 2160
	UHDMinWidthNarrow = 2880
)

// IsUHD decides whether a source with these attributes is 4K/UHD.
func IsUHD(width, height int, videoResolution string) bool {
	switch NormalizeResolution(videoResolution) {
	case "4k", "2160", "uhd":
		return true
	}
	if width >= UHDMinWidth && height >= UHDMinHeightWide {
		return true
	}
	if height >= UHDMinHeight && width >= UHDMinWidthNarrow {
		return true
	}
	return false
}

// NormalizeResolution lower-cases a Plex videoResolution label and strips a
// trailing scan-type letter, so "2160p", "2160" and "4K" compare equal to
// their canonical forms. (The sessions document says "1080p" where the
// metadata document says "1080".)
func NormalizeResolution(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "p")
	s = strings.TrimSuffix(s, "i")
	return s
}

// Verdict is the outcome of classifying one session.
type Verdict struct {
	// Candidate is true when the session is a video transcode whose source
	// must be checked. False means the session is left alone regardless of
	// resolution (direct play, direct stream, audio transcode, ...).
	Candidate bool
	// Terminate is true only when the source was confirmed to be 4K/UHD.
	Terminate bool
	// Reason is a short stable label for logs and tests.
	Reason string
	// Detail is human-readable evidence.
	Detail string
	// MediaID is the id of the session's selected Media, used to find the
	// source in the library metadata. Empty when it could not be determined.
	MediaID string
	// Source is the matched library media, when found.
	Source *SourceMedia
}

// Screen performs the I/O-free first pass: it decides whether the session
// is a video transcode at all and which Media id identifies the source.
func Screen(s Session) Verdict {
	if s.Kind != "Video" {
		return Verdict{Reason: "not-video", Detail: fmt.Sprintf("%s session", strings.ToLower(nonEmpty(s.Kind, "unknown")))}
	}
	if s.Transcode == nil {
		return Verdict{Reason: "direct-play", Detail: "no TranscodeSession element"}
	}
	if s.Live {
		return Verdict{Reason: "live-tv", Detail: "live session has no library item to resolve a source from"}
	}
	vd, ad := s.decisions()
	if !s.IsVideoTranscode() {
		detail := fmt.Sprintf("videoDecision=%q audioDecision=%q", vd, ad)
		switch {
		case vd == "copy" && ad == "transcode":
			return Verdict{Reason: "audio-only-transcode", Detail: detail}
		case vd == "copy":
			return Verdict{Reason: "direct-stream", Detail: detail}
		case vd == "" && ad == "transcode":
			return Verdict{Reason: "audio-only-transcode", Detail: detail}
		default:
			return Verdict{Reason: "no-video-transcode", Detail: detail}
		}
	}
	v := Verdict{Candidate: true, Reason: "video-transcode"}
	if m, ok := s.SelectedMedia(); ok {
		v.MediaID = strings.TrimSpace(m.ID)
	}
	if v.MediaID == "" {
		v.Detail = "no selected Media in session; will require every library version to be 4K"
	}
	return v
}

// Judge performs the second pass with the library metadata. It matches the
// session's Media id against the item's versions. A session media id that
// is not among the item's versions means the item no longer describes what
// is playing (split apart, re-matched or re-scanned mid-stream), so nothing
// is terminated. Only when the session carries no selected Media at all is
// the item judged by its versions: a single version, or every version being
// 4K, is conclusive; anything else is left alone.
func Judge(v Verdict, sources []SourceMedia) Verdict {
	if !v.Candidate {
		return v
	}
	if len(sources) == 0 {
		v.Terminate = false
		v.Reason = "source-unknown"
		v.Detail = "library metadata lists no media versions"
		return v
	}
	if v.MediaID != "" {
		for i := range sources {
			if sources[i].ID == v.MediaID {
				return v.decide(sources[i], fmt.Sprintf("source media id=%s %s videoResolution=%q", sources[i].ID, sources[i].Dimensions(), sources[i].VideoResolution))
			}
		}
		v.Terminate = false
		v.Reason = "media-not-found"
		v.Detail = fmt.Sprintf("session media id %q is not among the %d library versions; leaving session alone", v.MediaID, len(sources))
		return v
	}
	if len(sources) == 1 {
		src := sources[0]
		return v.decide(src, fmt.Sprintf("sole library version id=%s %s videoResolution=%q (session has no selected media)", src.ID, src.Dimensions(), src.VideoResolution))
	}
	allUHD, noneUHD := true, true
	for _, src := range sources {
		if src.IsUHD() {
			noneUHD = false
		} else {
			allUHD = false
		}
	}
	switch {
	case allUHD:
		v.Terminate = true
		v.Reason = uhdReason(true)
		v.Detail = fmt.Sprintf("all %d library versions are 4K (session has no selected media)", len(sources))
	case noneUHD:
		v.Terminate = false
		v.Reason = uhdReason(false)
		v.Detail = fmt.Sprintf("none of %d library versions is 4K (session has no selected media)", len(sources))
	default:
		v.Terminate = false
		v.Reason = "ambiguous-source"
		v.Detail = fmt.Sprintf("%d library versions with mixed resolutions and no selected media; leaving session alone", len(sources))
	}
	return v
}

// decide records src as the resolved source and derives the verdict from it.
func (v Verdict) decide(src SourceMedia, detail string) Verdict {
	v.Source = &src
	v.Terminate = src.IsUHD()
	v.Reason = uhdReason(v.Terminate)
	v.Detail = detail
	return v
}

func uhdReason(uhd bool) string {
	if uhd {
		return "4k-transcode"
	}
	return "source-not-4k"
}

// Evidence renders the transcode and source facts for a log line. It never
// includes addresses or tokens.
func Evidence(s Session, v Verdict) string {
	var b strings.Builder
	if v.Source != nil {
		fmt.Fprintf(&b, "source=%s/%s", v.Source.Dimensions(), nonEmpty(v.Source.VideoResolution, "?"))
	} else {
		b.WriteString("source=unmatched")
	}
	if s.Transcode != nil {
		fmt.Fprintf(&b, " transcode=video:%s,audio:%s", nonEmpty(s.Transcode.VideoDecision, "?"), nonEmpty(s.Transcode.AudioDecision, "?"))
		if s.Transcode.Width > 0 || s.Transcode.Height > 0 {
			fmt.Fprintf(&b, " output=%dx%d", s.Transcode.Width, s.Transcode.Height)
		}
		if s.Transcode.Protocol != "" {
			fmt.Fprintf(&b, " protocol=%s", s.Transcode.Protocol)
		}
	}
	if m, ok := s.SelectedMedia(); ok {
		fmt.Fprintf(&b, " session_media=%s/%dx%d/%s", nonEmpty(m.ID, "?"), m.Width, m.Height, nonEmpty(m.VideoResolution, "?"))
		if p, ok := m.SelectedPart(); ok {
			if st, ok := p.VideoStream(); ok && st.DisplayTitle != "" {
				fmt.Fprintf(&b, " stream_title=%q", st.DisplayTitle)
			}
		}
	}
	if s.Player.Product != "" {
		fmt.Fprintf(&b, " player=%q", s.Player.Product)
	}
	if s.Player.State != "" {
		fmt.Fprintf(&b, " state=%s", s.Player.State)
	}
	return b.String()
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
