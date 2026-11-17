// body_fields.go reads and edits top-level fields of a replayable JSON
// request body without holding a large body in memory more than once.

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

func isJSONRequest(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A media type with a malformed parameter ("application/json; x")
		// is still JSON to an engine that reads only the type; the fields
		// the gateway owns must not slip past it.
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// ensureReplayableBody makes the request body replayable when nothing on
// its way did (a request that named its model outside the body is never
// spooled to find it), so the fields the gateway owns are read and rewritten
// on every request. A websocket upgrade has no body to own.
func ensureReplayableBody(r *http.Request, maxBody int64) (func(), error) {
	if r.GetBody != nil || r.Body == nil || r.Body == http.NoBody || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return func() {}, nil
	}
	if maxBody > 0 && r.ContentLength > maxBody {
		return func() {}, tooLarge(maxBody)
	}
	b, err := newReplayBodyFromReader(r.Body, maxBody)
	if err != nil {
		return func() {}, err
	}
	_ = r.Body.Close()
	installReplayBody(r, b)
	return func() { _ = b.Close() }, nil
}

// errNotUTF8JSON refuses a JSON body in another encoding: a JSON parser that
// detects UTF-16 or UTF-32 (Python's does) reads keys a byte search for the
// gateway-owned fields cannot see.
var errNotUTF8JSON = refuse(ErrorRequestBodyNotUTF8, nil)

// ownedJSONBody reports whether the gateway owns fields in the request body:
// any body an engine may parse as JSON, which is one declared JSON (or not
// declared at all) or one that reads as a JSON object whatever its declared
// type, since an engine may parse a body without looking at its type. A
// multipart upload, a websocket and a body that cannot be replayed are not.
// A body declared JSON or text that is not UTF-8 is refused.
func ownedJSONBody(r *http.Request) (bool, error) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.GetBody == nil {
		return false, nil
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		return false, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return false, err
	}
	defer body.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	head = head[:n]
	declared := isJSONRequest(r)
	if declared || strings.HasPrefix(mediaType, "text/") {
		if bytes.HasPrefix(head, []byte{0xfe, 0xff}) || bytes.HasPrefix(head, []byte{0xff, 0xfe}) || bytes.IndexByte(head[:min(4, len(head))], 0) >= 0 {
			return false, errNotUTF8JSON
		}
	}
	if declared {
		return true, nil
	}
	trimmed := bytes.TrimLeft(head, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{', nil
}

// asciiLetterEscape opens a JSON unicode escape of a character below 0x100;
// hasASCIILetterEscape narrows it to the code points 0x40-0x7f, which hold
// the ASCII letters and the underscore. A client can spell a key the gateway owns with such an
// escape: the engine's parser reads it as the plain key, a byte search does
// not. The escapes clients commonly send (non-ASCII text; "<", ">" and "&"
// from HTML-safe encoders) are outside that range.
var asciiLetterEscape = []byte{'\\', 'u', '0', '0'}

func hasASCIILetterEscape(window []byte) bool {
	for from := 0; ; {
		i := bytes.Index(window[from:], asciiLetterEscape)
		if i < 0 {
			return false
		}
		if at := from + i + len(asciiLetterEscape); at < len(window) && window[at] >= '4' && window[at] <= '7' {
			return true
		}
		from += i + 1
	}
}

// readerContainsEach marks found[i] when needles[i] occurs in the stream,
// in one pass over a pooled buffer, stopping once every needle was seen. A
// stream with an escaped ASCII letter (hasASCIILetterEscape) marks them all,
// so the caller looks its keys up exactly, decoding them.
func readerContainsEach(source io.Reader, needles [][]byte, found []bool) error {
	// keep is how many bytes of a chunk's tail the next one is searched
	// with, so a match across two reads is found; the escape needs its
	// digit after the four bytes.
	keep := len(asciiLetterEscape)
	for _, n := range needles {
		keep = max(keep, len(n)-1)
	}
	bufp := scanBuffers.Get().(*[]byte)
	defer scanBuffers.Put(bufp)
	buf := *bufp
	remaining := len(needles)
	held := 0
	for {
		n, err := source.Read(buf[held:])
		if n > 0 {
			window := buf[:held+n]
			if hasASCIILetterEscape(window) {
				for i := range found {
					found[i] = true
				}
				return nil
			}
			for i, needle := range needles {
				if !found[i] && bytes.Contains(window, needle) {
					found[i] = true
					remaining--
				}
			}
			if remaining == 0 {
				return nil
			}
			held = min(keep, len(window))
			copy(buf, window[len(window)-held:])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var scanBuffers = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

// setRequestJSONField replaces every top-level value of key in the request
// body with encoded and installs the result as the new replayable body; with
// insert, a body without the key gets it as its first member. Without
// insert, a body that has no such key is left as it is, uncopied.
func setRequestJSONField(r *http.Request, key string, encoded []byte, insert bool, maxBody int64) (func(), error) {
	return rewriteRequestBody(r, maxBody, func(source io.ReadSeeker, out io.Writer) (bool, error) {
		ranges, err := jsonKeyRanges(source, key)
		if err != nil || (len(ranges) == 0 && !insert) {
			return false, err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		if len(ranges) == 0 {
			return true, insertJSONField(source, out, key, encoded)
		}
		return true, spliceJSONRanges(source, out, ranges, encoded)
	})
}

// insertRequestJSONField adds key: encoded as the first member of a request
// body known not to name key at all (bodyNames), without looking for it and
// without copying it: the new body reads the old one up to its opening
// brace, then the member, then the rest. The old body must stay readable for
// as long as the new one is (the request's replay body lives until the
// request ends).
func insertRequestJSONField(r *http.Request, key string, encoded []byte, maxBody int64) (func(), error) {
	if r.GetBody == nil {
		return func() {}, errors.New("request body is not replayable")
	}
	if r.ContentLength < 0 {
		return rewriteRequestBody(r, maxBody, func(source io.ReadSeeker, out io.Writer) (bool, error) {
			return true, insertJSONField(source, out, key, encoded)
		})
	}
	base := r.GetBody
	at, empty, err := jsonObjectStart(base)
	if err != nil {
		return func() {}, err
	}
	name, _ := json.Marshal(key)
	member := append(append(name, ':'), encoded...)
	if !empty {
		member = append(member, ',')
	}
	size := r.ContentLength + int64(len(member))
	if maxBody > 0 && size > maxBody {
		return func() {}, tooLarge(maxBody)
	}
	open := func() (io.ReadCloser, error) {
		source, err := base()
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{io.MultiReader(io.LimitReader(source, at), bytes.NewReader(member), source), source}, nil
	}
	body, err := open()
	if err != nil {
		return func() {}, err
	}
	r.Body, r.GetBody, r.ContentLength, r.TransferEncoding = body, open, size, nil
	return func() {}, nil
}

// jsonObjectStart finds where a JSON object's members begin: just past its
// opening brace, after any leading whitespace; empty says the object has no
// members.
func jsonObjectStart(open func() (io.ReadCloser, error)) (int64, bool, error) {
	source, err := open()
	if err != nil {
		return 0, false, err
	}
	defer source.Close()
	reader := bufio.NewReaderSize(source, 512)
	var at int64
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, false, errors.New("request body is not a JSON object")
		}
		at++
		if b == '{' {
			break
		}
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return 0, false, errors.New("request body is not a JSON object")
		}
	}
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return at, false, nil
		}
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return at, b == '}', nil
		}
	}
}

// rewriteRequestBody runs rewrite from the replayable request body to a new
// one, and installs that when rewrite says it changed the body.
func rewriteRequestBody(r *http.Request, maxBody int64, rewrite func(source io.ReadSeeker, out io.Writer) (bool, error)) (func(), error) {
	if r.GetBody == nil {
		return func() {}, errors.New("request body is not replayable")
	}
	body, err := r.GetBody()
	if err != nil {
		return func() {}, err
	}
	defer body.Close()
	source, ok := body.(io.ReadSeeker)
	var spooled *replayBody
	if !ok {
		spooled, err = newReplayBodyFromReader(body, maxBody)
		if err != nil {
			return func() {}, err
		}
		defer spooled.Close()
		source = spooled.newReadSeeker()
	}
	out := newReplayWriter(maxBody)
	changed, err := rewrite(source, out)
	if err != nil || !changed {
		out.Abort()
		return func() {}, err
	}
	replaced, err := out.Close()
	if err != nil {
		return func() {}, err
	}
	installReplayBody(r, replaced)
	return func() { _ = replaced.Close() }, nil
}

// readRequestBody reads the replayable request body, bounded.
func readRequestBody(r *http.Request, maxBody int64) ([]byte, error) {
	if r.GetBody == nil {
		return nil, errors.New("request body is not replayable")
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, err
	}
	defer body.Close()
	limit := maxBody
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, tooLarge(limit)
	}
	return data, nil
}

func mustRaw(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
