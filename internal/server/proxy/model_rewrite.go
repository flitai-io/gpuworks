package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// rewriteModel retains large uploads on disk and changes only the routing
// field. Binary payloads without a model field are forwarded unchanged.
func rewriteModel(r *http.Request, name string, maxBody int64) (func(), error) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		query := r.URL.Query()
		query.Set("model", name)
		r.URL.RawQuery = query.Encode()
		return func() {}, nil
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil && r.Header.Get("Content-Type") != "" {
		return func() {}, err
	}
	isMultipart := strings.HasPrefix(mediaType, "multipart/")
	if !isMultipart && mediaType != "" && mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return func() {}, nil
	}
	// extractModel already installs a disk-backed *os.File for large request
	// bodies. Read that seekable replay directly instead of copying it into a
	// second replayBody before producing the rewritten body. Only requests that
	// arrived through a query/header model and are not replayable need a first
	// spool here, which is bounded by the same writer.
	var source io.ReadSeeker
	var sourceCleanup func()
	if seekable, ok := r.Body.(io.ReadSeeker); ok {
		source = seekable
		sourceCleanup = func() {}
	} else {
		var reader io.ReadCloser
		if r.GetBody != nil {
			reader, err = r.GetBody()
		} else {
			reader = r.Body
		}
		if err != nil {
			return func() {}, err
		}
		b, spoolErr := newReplayBodyFromReader(reader, maxBody)
		_ = reader.Close()
		if spoolErr != nil {
			return func() {}, spoolErr
		}
		source = b.newReadSeeker()
		sourceCleanup = func() { _ = b.Close() }
	}
	defer sourceCleanup()
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return func() {}, err
	}
	encoded := newReplayWriter(maxBody)
	if isMultipart {
		err = rewriteMultipartModel(source, encoded, params["boundary"], name)
	} else {
		err = rewriteJSONModel(source, encoded, name)
	}
	if err != nil {
		encoded.Abort()
		return func() {}, fmt.Errorf("rewrite engine model: %w", err)
	}
	out, err := encoded.Close()
	if err != nil {
		return func() {}, err
	}
	installReplayBody(r, out)
	return func() { _ = out.Close() }, nil
}

func rewriteMultipartModel(source io.Reader, output io.Writer, boundary, name string) error {
	reader := multipart.NewReader(source, boundary)
	writer := multipart.NewWriter(output)
	if err := writer.SetBoundary(boundary); err != nil {
		return err
	}
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return writer.Close()
		}
		if err != nil {
			return err
		}
		destination, err := writer.CreatePart(part.Header)
		if err != nil {
			return err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(destination, name)
		} else {
			_, err = io.Copy(destination, part)
		}
		if err != nil {
			return err
		}
		_ = part.Close()
	}
}

func rewriteJSONModel(source io.ReadSeeker, output io.Writer, name string) error {
	encoded, _ := json.Marshal(name)
	return rewriteJSONField(source, output, "model", encoded, false)
}

// rewriteJSONField streams source to output with every top-level value of key
// replaced by encoded. When the key is absent and insert is set, it is added
// as the object's first member; nested fields of the same name stay intact.
func rewriteJSONField(source io.ReadSeeker, output io.Writer, key string, encoded []byte, insert bool) error {
	ranges, err := jsonKeyRanges(source, key)
	if err != nil {
		return err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if len(ranges) == 0 && insert {
		return insertJSONField(source, output, key, encoded)
	}
	var offset int64
	for _, span := range ranges {
		if _, err := io.CopyN(output, source, span[0]-offset); err != nil {
			return err
		}
		if _, err := source.Seek(span[1], io.SeekStart); err != nil {
			return err
		}
		if _, err := output.Write(encoded); err != nil {
			return err
		}
		offset = span[1]
	}
	_, err = io.Copy(output, source)
	return err
}

// insertJSONField copies a JSON object with key: encoded as its first member.
func insertJSONField(source io.Reader, output io.Writer, key string, encoded []byte) error {
	reader := bufio.NewReader(source)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return fmt.Errorf("request body is not a JSON object")
		}
		if _, err := output.Write([]byte{b}); err != nil {
			return err
		}
		if b == '{' {
			break
		}
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return fmt.Errorf("request body is not a JSON object")
		}
	}
	name, _ := json.Marshal(key)
	member := append(append(append(name, ':'), encoded...), ',')
	// An empty object takes the member without the separator.
	next, err := reader.Peek(1)
	for err == nil && (next[0] == ' ' || next[0] == '\t' || next[0] == '\n' || next[0] == '\r') {
		_, _ = reader.ReadByte()
		next, err = reader.Peek(1)
	}
	if err == nil && next[0] == '}' {
		member = member[:len(member)-1]
	}
	if _, err := output.Write(member); err != nil {
		return err
	}
	_, err = io.Copy(output, reader)
	return err
}

// jsonKeyRanges locates the top-level values of one key, whatever their type,
// without buffering values: media strings can be megabytes.
func jsonKeyRanges(source io.Reader, want string) ([][2]int64, error) {
	reader := bufio.NewReader(source)
	var spans [][2]int64
	var offset int64
	depth, key := 0, ""
	expectingKey := false
	// valueStart is where a wanted value began (-1 when none is open);
	// valueEnd is one past its last significant byte, for scalars.
	valueStart, valueEnd := int64(-1), int64(-1)
	awaitingValue := false
	closeValue := func() {
		if valueStart >= 0 {
			spans = append(spans, [2]int64{valueStart, valueEnd})
			valueStart = -1
		}
	}
	for {
		value, err := reader.ReadByte()
		if err == io.EOF {
			return spans, nil
		}
		if err != nil {
			return nil, err
		}
		start := offset
		offset++
		if awaitingValue && value != ' ' && value != '\t' && value != '\n' && value != '\r' {
			awaitingValue = false
			valueStart, valueEnd = start, offset
		}
		switch value {
		case '{', '[':
			depth++
			if depth == 1 && value == '{' {
				expectingKey = true
			}
		case '}', ']':
			depth--
			if depth == 1 && valueStart >= 0 {
				valueEnd = offset
				closeValue()
			} else if depth == 0 {
				closeValue()
			}
		case ',':
			if depth == 1 {
				closeValue()
				expectingKey, key = true, ""
			}
		case ':':
			if depth == 1 {
				expectingKey = false
				awaitingValue = key == want
			}
		case '"':
			capture := depth == 1 && expectingKey
			text := []byte{'"'}
			escaped := false
			for {
				char, err := reader.ReadByte()
				if err != nil {
					return nil, err
				}
				offset++
				if capture {
					if len(text) > maxMultipartModelBytes {
						return nil, fmt.Errorf("JSON key exceeds %d bytes", maxMultipartModelBytes)
					}
					text = append(text, char)
				}
				if char == '"' && !escaped {
					break
				}
				if char == '\\' && !escaped {
					escaped = true
				} else {
					escaped = false
				}
			}
			if capture {
				if err := json.Unmarshal(text, &key); err != nil {
					return nil, err
				}
			} else if depth == 1 && valueStart == start {
				valueEnd = offset
				closeValue()
			}
		default:
			if depth == 1 && valueStart >= 0 && value != ' ' && value != '\t' && value != '\n' && value != '\r' {
				valueEnd = offset
			}
		}
	}
}
