package guard

import "fmt"

// SourceMedia is one <Media> version of a library item as reported by
// /library/metadata/{ratingKey}. Unlike the sessions document, these values
// describe the file on disk.
type SourceMedia struct {
	ID              string
	Width           int
	Height          int
	VideoResolution string
	Container       string
	VideoCodec      string
}

// Dimensions renders "3840x2160" or "?" when unknown.
func (m SourceMedia) Dimensions() string {
	if m.Width == 0 && m.Height == 0 {
		return "?"
	}
	return fmt.Sprintf("%dx%d", m.Width, m.Height)
}

// IsUHD reports whether this media is a 4K/UHD source.
func (m SourceMedia) IsUHD() bool {
	return IsUHD(m.Width, m.Height, m.VideoResolution)
}

// ParseMetadataMedia extracts every <Media> element from a
// /library/metadata/{ratingKey} document.
func ParseMetadataMedia(data []byte) ([]SourceMedia, error) {
	c, err := decodeContainer(data)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if len(c.Items) == 0 {
		return nil, fmt.Errorf("metadata: document has no items")
	}
	var out []SourceMedia
	for _, it := range c.Items {
		for _, xm := range it.Media {
			out = append(out, SourceMedia{
				ID:              xm.get("id"),
				Width:           xm.int("width"),
				Height:          xm.int("height"),
				VideoResolution: xm.get("videoResolution"),
				Container:       xm.get("container"),
				VideoCodec:      xm.get("videoCodec"),
			})
		}
	}
	return out, nil
}
