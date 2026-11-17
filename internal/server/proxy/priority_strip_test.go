package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func priorityRequest(t *testing.T, contentType, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.Header.Set("X-Vllm-Priority", "-100")
	return r
}

func forwardedBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A key spelled with an escaped letter is the plain key to the engine's
// parser, so the scan reports every key as named and the caller looks them
// up exactly; the escapes clients commonly send do not. Matches across two
// reads are found, a byte at a time.
func TestBodyNamesSeesEscapedKeys(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []bool
	}{
		{"plain", `{"model":"m","priority":1}`, []bool{false, true}},
		{"escaped priority", jsonEscapes(`{"model":"m","%u0070riority":1}`), []bool{true, true}},
		{"escaped kv field", jsonEscapes(`{"kv_tr%u0061nsfer_params":{}}`), []bool{true, true}},
		{"escaped underscore", jsonEscapes(`{"kv%u005ftransfer_params":{}}`), []bool{true, true}},
		{"non-ASCII text", jsonEscapes(`{"messages":[{"content":"%u4e2d%u6587 caf%u00e9"}]}`), []bool{false, false}},
		{"HTML-safe escapes", jsonEscapes(`{"messages":[{"content":"%u003cb%u003e %u0026"}]}`), []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, oneByte := range []bool{false, true} {
				var source io.Reader = strings.NewReader(tc.body)
				if oneByte {
					source = iotest.OneByteReader(source)
				}
				found := make([]bool, 2)
				if err := readerContainsEach(source, [][]byte{[]byte(`"` + kvTransferField + `"`), []byte(`"priority"`)}, found); err != nil {
					t.Fatal(err)
				}
				if found[0] != tc.want[0] || found[1] != tc.want[1] {
					t.Fatalf("one byte at a time %v: found = %v, want %v", oneByte, found, tc.want)
				}
			}
		})
	}
}

// Setting a field the body does not have, without insert, leaves the body
// as it is and uncopied; the escaped spelling of the key is replaced.
func TestSetRequestJSONFieldWithoutTheKey(t *testing.T) {
	r := priorityRequest(t, "application/json", `{"model":"m","messages":[{"content":"\"priority\": 5"}]}`)
	before := r.Body
	cleanup, err := setRequestJSONField(r, priorityField, []byte("0"), false, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || r.Body != before {
		t.Fatalf("err=%v, body replaced: %v", err, r.Body != before)
	}
	r = priorityRequest(t, "application/json", jsonEscapes(`{"model":"m","%u0070riority":-1000}`))
	cleanup, err = setRequestJSONField(r, priorityField, []byte("0"), false, DefaultMaxBodyBytes)
	defer cleanup()
	if got := forwardedBody(t, r); err != nil || got != jsonEscapes(`{"model":"m","%u0070riority":0}`) {
		t.Fatalf("err=%v body=%s", err, got)
	}
}

// The inserted body reads as the copied insertion would, every time it is
// read, with its length; a body spooled to disk works the same, and one that
// is not a JSON object is refused.
func TestInsertRequestJSONFieldReadsLikeACopy(t *testing.T) {
	big := `{"model":"m","messages":[{"content":"` + strings.Repeat("x", bodyMemoryThreshold+10) + `"}]}`
	for _, body := range []string{`{"model":"m"}`, " \n\t{ \"model\":\"m\"}", `{}`, `{ }`, big} {
		rb, err := newReplayBody([]byte(body), DefaultMaxBodyBytes)
		if err != nil {
			t.Fatal(err)
		}
		r := priorityRequest(t, "application/json", "")
		installReplayBody(r, rb)
		cleanup, err := insertRequestJSONField(r, priorityField, []byte("-20"), DefaultMaxBodyBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { cleanup(); _ = rb.Close() }()
		var want strings.Builder
		if err := insertJSONField(strings.NewReader(body), &want, priorityField, []byte("-20")); err != nil {
			t.Fatal(err)
		}
		first, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		// The copy drops the whitespace after the brace; the view keeps it.
		var got, wanted map[string]any
		if err := json.Unmarshal(first, &got); err != nil || json.Unmarshal([]byte(want.String()), &wanted) != nil {
			t.Fatalf("body %.40q: %v in %.80q", body, err, first)
		}
		if got["priority"] != float64(-20) || len(got) != len(wanted) || got["model"] != wanted["model"] {
			t.Fatalf("body %.40q reads as %.80v, want %.80v", body, got, wanted)
		}
		if again := forwardedBody(t, r); again != string(first) || r.ContentLength != int64(len(first)) {
			t.Fatalf("body %.40q: second read differs or length %d != %d", body, r.ContentLength, len(first))
		}
	}
	r := priorityRequest(t, "application/json", `["not an object"]`)
	if _, err := insertRequestJSONField(r, priorityField, []byte("1"), DefaultMaxBodyBytes); err == nil {
		t.Fatal("an array took a member")
	}
}
