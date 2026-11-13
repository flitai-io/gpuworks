package aliyun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// signAlgorithm is Alibaba Cloud's V3 request signature, the one the
// OpenAPI docs recommend over the deprecated RPC V1 HMAC-SHA1 scheme:
// https://help.aliyun.com/zh/sdk/product-overview/v3-request-structure-and-signature
const signAlgorithm = "ACS3-HMAC-SHA256"

// emptyPayloadHash is the hex SHA-256 of an empty body: every call here sends
// its parameters in the query string.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signedRequest is what a V3 signature covers: method, host, path, query and
// the x-acs-* headers. The body is always empty. RPC-style APIs (ECS) post to
// "/"; ROA-style ones (ACK) name the resource in the path.
type signedRequest struct {
	method  string
	host    string
	path    string // "" is "/"
	query   map[string]string
	headers map[string]string // lower-case names; host is added by sign
}

// sign returns the Authorization header value for req.
func sign(req signedRequest, accessKeyID, accessKeySecret string) string {
	headers := map[string]string{"host": req.host}
	for name, value := range req.headers {
		headers[strings.ToLower(name)] = value
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		if name == "host" || name == "content-type" || strings.HasPrefix(name, "x-acs-") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(headers[name]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	path := req.path
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		req.method,
		canonicalPath(path),
		canonicalQuery(req.query),
		canonicalHeaders.String(),
		signedHeaders,
		emptyPayloadHash,
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	stringToSign := signAlgorithm + "\n" + hex.EncodeToString(digest[:])
	mac := hmac.New(sha256.New, []byte(accessKeySecret))
	mac.Write([]byte(stringToSign))
	return signAlgorithm + " Credential=" + accessKeyID + ",SignedHeaders=" + signedHeaders + ",Signature=" + hex.EncodeToString(mac.Sum(nil))
}

// canonicalPath percent-encodes each segment of a resource path.
func canonicalPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = percentEncode(s)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts the parameters by name and percent-encodes both names
// and values the way the signature expects (RFC 3986: space is %20, '~' is
// kept, '*' is %2A).
func canonicalQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, percentEncode(key)+"="+percentEncode(query[key]))
	}
	return strings.Join(parts, "&")
}

func percentEncode(s string) string {
	encoded := url.QueryEscape(s)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "*", "%2A")
	return strings.ReplaceAll(encoded, "%7E", "~")
}
