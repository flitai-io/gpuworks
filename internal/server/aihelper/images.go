package aihelper

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Image limits of one ask. The console shrinks a screenshot before sending it
// (a long side of at most ImageMaxSide pixels, re-encoded), so a real image is
// far below ImageMaxBytes; the bounds keep one request from carrying more than
// a model should read or a server should hold: at most
// ImagesPerRequest*ImageMaxBytes of images, base64 adds a third.
const (
	ImagesPerMessage = 4
	ImagesPerRequest = 8
	ImageMaxBytes    = 2 << 20
	ImageMaxSide     = 1568
)

// ErrImageInputOff answers an ask that carries images while the AI service in
// use is not marked as accepting them (store.AIConfig.ImageInput): sending
// them anyway would have the model answer about a screenshot it never saw.
var ErrImageInputOff = errors.New("the AI service in use is not marked as accepting images")

// ImageOnlyQuestion stands in for the question of an ask that sends images
// and no text.
const ImageOnlyQuestion = "Look at the attached image and help me with what it shows."

// imageTypes are the formats OpenAI-compatible vision endpoints take.
var imageTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// ImageError says which rule an image broke, as a stable code with the index
// of the image within its message.
type ImageError struct {
	Code  string // too_many, format, too_large
	Index int
}

func (e *ImageError) Error() string {
	return fmt.Sprintf("image %d: %s", e.Index+1, e.Code)
}

// ValidateImages checks the images of one message: base64 data: URLs of a
// supported type whose bytes are that type and fit ImageMaxBytes. The URL
// itself never appears in the error.
func ValidateImages(images []string) error {
	if len(images) > ImagesPerMessage {
		return &ImageError{Code: "too_many", Index: ImagesPerMessage}
	}
	for i, raw := range images {
		mediaType, payload, ok := strings.Cut(strings.TrimPrefix(raw, "data:"), ";base64,")
		if !strings.HasPrefix(raw, "data:") || !ok || !knownImageType(mediaType) {
			return &ImageError{Code: "format", Index: i}
		}
		if base64.StdEncoding.DecodedLen(len(payload)) > ImageMaxBytes+3 {
			return &ImageError{Code: "too_large", Index: i}
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil || http.DetectContentType(data) != mediaType {
			return &ImageError{Code: "format", Index: i}
		}
		if len(data) > ImageMaxBytes {
			return &ImageError{Code: "too_large", Index: i}
		}
	}
	return nil
}

func knownImageType(mediaType string) bool {
	for _, t := range imageTypes {
		if t == mediaType {
			return true
		}
	}
	return false
}

// MarshalJSON writes a message with images as content parts - the text, then
// each image - and any other message with its content as a string.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	parts := make([]ContentPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, ContentPart{Type: "text", Text: m.Content})
	}
	for _, url := range m.Images {
		parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: url}})
	}
	return json.Marshal(struct {
		plain
		Content []ContentPart `json:"content"`
	}{plain(m), parts})
}

// UpstreamError is an AI service that answered a call with an error status.
// Body is its own answer, at most 4 MiB.
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("llm http %d: %s", e.Status, e.Body)
}

// ImageRefused reports whether err is the service refusing a request because
// of its images: a 400, 415 or 422 that does not name the model - the same
// rule the image check applies (ProbeImage). It is meaningful only for a
// request that carried images.
func ImageRefused(err error) bool {
	var up *UpstreamError
	if !errors.As(err, &up) {
		return false
	}
	switch up.Status {
	case http.StatusBadRequest, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return !namesModel([]byte(up.Body))
	}
	return false
}
