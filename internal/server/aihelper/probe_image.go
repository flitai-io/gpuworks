package aihelper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// ImageCheck is what sending an image to a chat endpoint established.
type ImageCheck string

const (
	// ImageAccepted: the model answered a message carrying an image and its
	// answer shows it saw the image (it names the image's color).
	ImageAccepted ImageCheck = "accepted"
	// ImageIgnored: the endpoint answered with a completion that does not show
	// the model saw the image. Some endpoints drop the image part of a request
	// to a text-only model and answer the text alone - DeepSeek's
	// deepseek-v4-pro did on 2026-11-13, with a 200, a prompt token count with no
	// image in it and the answer "Unknown" - so a 200 is not acceptance.
	ImageIgnored ImageCheck = "ignored"
	// ImageRejected: the endpoint refused the request because of the image
	// (a 400, 415 or 422 that does not name the model) while the same call
	// without one succeeds.
	ImageRejected ImageCheck = "rejected"
	// ImageUnknown: the check could not tell - the endpoint did not answer,
	// refused the key, or failed for a reason unrelated to the image.
	ImageUnknown ImageCheck = "unknown"
)

// ImageProbeResult is one image check. Status and Detail are the endpoint's
// own words where it spoke; Detail never contains the API key.
type ImageProbeResult struct {
	Check  ImageCheck
	Status int
	Detail string
}

// ContentPart is one part of a message whose content is not a single text:
// OpenAI's {"type":"text"} and {"type":"image_url"} parts, which DeepSeek,
// DashScope and OpenRouter accept in the same shape.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is an image part's source: an https URL or a data: URL.
type ImageURL struct {
	URL string `json:"url"`
}

type partsMessage struct {
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}

type partsRequest struct {
	Model    string         `json:"model"`
	Messages []partsMessage `json:"messages"`
	Stream   bool           `json:"stream"`
}

// probeImageDataURL is a 32x32 solid red PNG: large enough for providers that
// refuse very small images, small enough to cost a handful of tokens. Its one
// color is what an answer must name for the model to have seen it.
var probeImageDataURL = func() string {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}()

// ProbeImage asks the endpoint about a small image, with the endpoint's own
// model and key, to find out whether that model accepts image input. It is
// meaningful only after Probe answered ProbeOK: a failure that also hits a
// text-only call says nothing about images, which is why only an answer that
// blames the request itself counts as ImageRejected.
func (c *Client) ProbeImage(ctx context.Context) ImageProbeResult {
	body, err := json.Marshal(partsRequest{Model: c.ep.Model, Messages: []partsMessage{{Role: "user", Content: []ContentPart{
		{Type: "text", Text: "What color is this image? Answer with one word."},
		{Type: "image_url", ImageURL: &ImageURL{URL: probeImageDataURL}},
	}}}})
	if err != nil {
		return ImageProbeResult{Check: ImageUnknown, Detail: "the request could not be built"}
	}
	call, err := ChatCompletionsURL(c.ep.BaseURL)
	if err != nil {
		return ImageProbeResult{Check: ImageUnknown, Detail: c.redact(err.Error())}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call, bytes.NewReader(body))
	if err != nil {
		return ImageProbeResult{Check: ImageUnknown, Detail: c.redact(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.ep.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return ImageProbeResult{Check: ImageUnknown, Detail: c.redact(transportDetail(err))}
	}
	defer c.http.CloseIdleConnections()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := ImageProbeResult{Check: ImageUnknown, Status: resp.StatusCode, Detail: c.redact(boundedText(raw))}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var cr ChatResponse
		if json.Unmarshal(raw, &cr) == nil && len(cr.Choices) > 0 {
			answer := cr.Choices[0].Message.Content
			out.Check, out.Detail = ImageIgnored, boundedText([]byte(answer))
			if namesRed(answer) {
				out.Check, out.Detail = ImageAccepted, ""
			}
		}
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnsupportedMediaType || resp.StatusCode == http.StatusUnprocessableEntity:
		if !namesModel(raw) {
			out.Check = ImageRejected
		}
	}
	return out
}

// namesRed reports whether an answer names the probe image's color, as a
// word, in English or Chinese: "Red", "red.", "红色" - and not "covered".
func namesRed(answer string) bool {
	if strings.Contains(answer, "红") {
		return true
	}
	for _, word := range strings.FieldsFunc(strings.ToLower(answer), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch word {
		case "red", "crimson", "scarlet", "reddish":
			return true
		}
	}
	return false
}

// ImageProbeTimeout bounds one image check, on top of the text probe: an
// image costs the endpoint more than a word.
const ImageProbeTimeout = 20 * time.Second
