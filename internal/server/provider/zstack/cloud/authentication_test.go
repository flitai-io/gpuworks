package cloud

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

func validTestAccessKey(r *http.Request) bool {
	date := r.Header.Get("Date")
	when, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -07:00", date)
	if err != nil || time.Since(when).Abs() > 15*time.Minute {
		return false
	}
	mac := hmac.New(sha1.New, []byte("test-access-secret"))
	// RestServer signs the decoded URI without the servlet context or query.
	mac.Write([]byte(r.Method + "\n" + date + "\n" + strings.TrimPrefix(r.URL.Path, "/zstack")))
	expected := "ZStack test-access-key:" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(r.Header.Get("Authorization")), []byte(expected))
}
