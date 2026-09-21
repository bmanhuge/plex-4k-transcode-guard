package guard

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Session is one entry of /status/sessions, reduced to what the guard uses.
type Session struct {
	// Kind is the element name: Video, Track or Photo.
	Kind string

	SessionKey       string
	RatingKey        string
	Title            string
	GrandparentTitle string
	// Live is set for Live TV / DVR sessions, which have no library item.
	Live bool

	User   string
	Player Player

	// SessionID is the id attribute of the <Session> child element. It is the
	// identifier accepted by /status/sessions/terminate.
	SessionID string

	// Transcode is nil when there is no <TranscodeSession> (direct play).
	Transcode *Transcode
	// Media lists the <Media> children. While transcoding, Plex reports the
	// transcoded OUTPUT here (for example 1280x720 videoResolution="720p"
	// protocol="dash"), not the source. Use the id to look the source up in
	// /library/metadata/{RatingKey}.
	Media []Media
}

// Player describes the client (no addresses or identifiers are kept).
type Player struct {
	Product string
	State   string
}

// Transcode mirrors the <TranscodeSession> element. Width and Height are the
// OUTPUT dimensions.
type Transcode struct {
	VideoDecision string
	AudioDecision string
	Protocol      string
	Width         int
	Height        int
}

// Media mirrors a <Media> element of the sessions document.
type Media struct {
	ID              string
	Width           int
	Height          int
	VideoResolution string
	Selected        bool
	Parts           []Part
}

// Part mirrors a <Part> element.
type Part struct {
	Selected bool
	Streams  []Stream
}

// Stream mirrors a <Stream> element. StreamType 1 is video. DisplayTitle is
// derived by Plex from the source stream and is kept as log evidence only.
type Stream struct {
	StreamType   int
	DisplayTitle string
	Selected     bool
}

// Sessions is the parsed /status/sessions document.
type Sessions struct {
	Items  []Session
	Videos int
}

// DisplayTitle renders a human-readable title ("Show - Episode" for
// episodes, plain title otherwise).
func (s Session) DisplayTitle() string {
	if s.GrandparentTitle != "" {
		return s.GrandparentTitle + " - " + s.Title
	}
	return s.Title
}

// SelectedMedia returns the <Media> marked selected="1". When none is
// marked and exactly one exists, that one is returned. Otherwise ok is
// false (ambiguous).
func (s Session) SelectedMedia() (m Media, ok bool) {
	for _, m := range s.Media {
		if m.Selected {
			return m, true
		}
	}
	if len(s.Media) == 1 {
		return s.Media[0], true
	}
	return Media{}, false
}

// SelectedPart returns the selected <Part>, falling back to a sole part.
func (m Media) SelectedPart() (p Part, ok bool) {
	for _, p := range m.Parts {
		if p.Selected {
			return p, true
		}
	}
	if len(m.Parts) == 1 {
		return m.Parts[0], true
	}
	return Part{}, false
}

// VideoStream returns the selected (or first) video stream of the part.
func (p Part) VideoStream() (st Stream, ok bool) {
	var first *Stream
	for i := range p.Streams {
		s := &p.Streams[i]
		if s.StreamType != 1 {
			continue
		}
		if s.Selected {
			return *s, true
		}
		if first == nil {
			first = s
		}
	}
	if first != nil {
		return *first, true
	}
	return Stream{}, false
}

// decision returns the normalised video and audio decisions ("transcode",
// "copy", or "" when there is no TranscodeSession).
func (s Session) decisions() (video, audio string) {
	if s.Transcode == nil {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(s.Transcode.VideoDecision)), strings.ToLower(strings.TrimSpace(s.Transcode.AudioDecision))
}

// IsVideoTranscode reports whether the session is a video session whose
// video stream is being transcoded (videoDecision="transcode"). Direct play
// (no TranscodeSession), direct stream (videoDecision="copy") and
// audio-only transcodes all return false.
func (s Session) IsVideoTranscode() bool {
	video, _ := s.decisions()
	return s.Kind == "Video" && video == "transcode"
}

type xmlAttrs struct {
	Attrs []xml.Attr `xml:",any,attr"`
}

func (x xmlAttrs) get(name string) string {
	for _, a := range x.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (x xmlAttrs) int(name string) int {
	v := strings.TrimSpace(x.get(name))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0
		}
		return int(f)
	}
	return n
}

func (x xmlAttrs) flag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(x.get(name))) {
	case "1", "true":
		return true
	}
	return false
}

type xmlStream struct {
	xmlAttrs
}

type xmlPart struct {
	xmlAttrs
	Streams []xmlStream `xml:"Stream"`
}

type xmlMedia struct {
	xmlAttrs
	Parts []xmlPart `xml:"Part"`
}

type xmlItem struct {
	XMLName xml.Name
	xmlAttrs
	Media     []xmlMedia `xml:"Media"`
	User      *xmlAttrs  `xml:"User"`
	Player    *xmlAttrs  `xml:"Player"`
	Session   *xmlAttrs  `xml:"Session"`
	Transcode *xmlAttrs  `xml:"TranscodeSession"`
}

type xmlContainer struct {
	XMLName xml.Name `xml:"MediaContainer"`
	xmlAttrs
	Items []xmlItem `xml:",any"`
}

func decodeContainer(data []byte) (*xmlContainer, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("empty response")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	var c xmlContainer
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing xml: %w", err)
	}
	if c.XMLName.Local != "MediaContainer" {
		return nil, fmt.Errorf("parsing xml: unexpected root element %q", c.XMLName.Local)
	}
	return &c, nil
}

// ParseSessions parses a /status/sessions document.
func ParseSessions(data []byte) (Sessions, error) {
	c, err := decodeContainer(data)
	if err != nil {
		return Sessions{}, fmt.Errorf("sessions: %w", err)
	}
	var out Sessions
	for _, it := range c.Items {
		s := Session{
			Kind:             it.XMLName.Local,
			SessionKey:       it.get("sessionKey"),
			RatingKey:        it.get("ratingKey"),
			Title:            it.get("title"),
			GrandparentTitle: it.get("grandparentTitle"),
			Live:             it.flag("live"),
		}
		if s.Kind == "Video" {
			out.Videos++
		}
		if it.User != nil {
			s.User = it.User.get("title")
		}
		if it.Player != nil {
			s.Player = Player{Product: it.Player.get("product"), State: it.Player.get("state")}
		}
		if it.Session != nil {
			s.SessionID = strings.TrimSpace(it.Session.get("id"))
		}
		if it.Transcode != nil {
			s.Transcode = &Transcode{
				VideoDecision: it.Transcode.get("videoDecision"),
				AudioDecision: it.Transcode.get("audioDecision"),
				Protocol:      it.Transcode.get("protocol"),
				Width:         it.Transcode.int("width"),
				Height:        it.Transcode.int("height"),
			}
		}
		for _, xm := range it.Media {
			m := Media{
				ID:              xm.get("id"),
				Width:           xm.int("width"),
				Height:          xm.int("height"),
				VideoResolution: xm.get("videoResolution"),
				Selected:        xm.flag("selected"),
			}
			for _, xp := range xm.Parts {
				p := Part{Selected: xp.flag("selected")}
				for _, xs := range xp.Streams {
					p.Streams = append(p.Streams, Stream{
						StreamType:   xs.int("streamType"),
						DisplayTitle: xs.get("displayTitle"),
						Selected:     xs.flag("selected"),
					})
				}
				m.Parts = append(m.Parts, p)
			}
			s.Media = append(s.Media, m)
		}
		out.Items = append(out.Items, s)
	}
	return out, nil
}
