package guard

import (
	"fmt"
	"strings"
)

// UHD thresholds. A source is 4K when either dimension reaches these values
// or Plex labels it 4k/2160/uhd. Ultra-wide UHD encodes (3840x1600) are
// caught by the width test.
const (
	UHDMinWidth  = 3840
	UHDMinHeight = 2160
)

// IsUHD decides whether a source with these attributes is 4K/UHD.
func IsUHD(width, height int, videoResolution string) bool {
	if width >= UHDMinWidth || height >= UHDMinHeight {
		return true
	}
	switch NormalizeResolution(videoResolution) {
	case "4k", "2160", "uhd":
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
	vd := strings.ToLower(strings.TrimSpace(s.Transcode.VideoDecision))
	ad := strings.ToLower(strings.TrimSpace(s.Transcode.AudioDecision))
	if vd != "transcode" {
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
// session's Media id against the item's versions; when there is no match it
// only concludes 4K if the item has a single version or every version is 4K.
// Any remaining ambiguity resolves to "do not terminate".
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
				src := sources[i]
				v.Source = &src
				v.Terminate = src.IsUHD()
				if v.Terminate {
					v.Reason = "4k-transcode"
				} else {
					v.Reason = "source-not-4k"
				}
				v.Detail = fmt.Sprintf("source media id=%s %s videoResolution=%q", src.ID, src.Dimensions(), src.VideoResolution)
				return v
			}
		}
	}
	if len(sources) == 1 {
		src := sources[0]
		v.Source = &src
		v.Terminate = src.IsUHD()
		if v.Terminate {
			v.Reason = "4k-transcode"
		} else {
			v.Reason = "source-not-4k"
		}
		v.Detail = fmt.Sprintf("sole library version id=%s %s videoResolution=%q (session media id %q not matched)", src.ID, src.Dimensions(), src.VideoResolution, v.MediaID)
		return v
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
		v.Reason = "4k-transcode"
		v.Detail = fmt.Sprintf("all %d library versions are 4K (session media id %q not matched)", len(sources), v.MediaID)
	case noneUHD:
		v.Terminate = false
		v.Reason = "source-not-4k"
		v.Detail = fmt.Sprintf("none of %d library versions is 4K (session media id %q not matched)", len(sources), v.MediaID)
	default:
		v.Terminate = false
		v.Reason = "ambiguous-source"
		v.Detail = fmt.Sprintf("%d library versions with mixed resolutions and session media id %q not matched; leaving session alone", len(sources), v.MediaID)
	}
	return v
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
