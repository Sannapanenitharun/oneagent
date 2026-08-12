// Package awssig implements AWS Signature Version 4 request signing using
// only the Go standard library. The full AWS SDK for Go pulls in a large
// dependency tree unavailable through this environment's allowed module
// sources (it resolves through proxy.golang.org, which isn't reachable
// here) — SigV4 itself is a documented, stable algorithm
// (https://docs.aws.amazon.com/general/latest/gr/sigv4-signing.html) that
// only needs crypto/hmac and crypto/sha256, so hand-implementing it avoids
// the dependency entirely rather than working around a missing one.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const awsDateFormat = "20060102T150405Z"
const awsDateOnlyFormat = "20060102"

// Credentials holds the AWS access key pair used to sign a request.
// SessionToken is optional (set for temporary/STS credentials).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// SignRequest signs req in place, adding the Authorization header (and
// X-Amz-Security-Token / X-Amz-Date as needed) per SigV4. body is passed
// separately (rather than read from req.Body) because SigV4 requires
// hashing the exact payload bytes, and req.Body may already be consumed
// or need to be reset after signing.
func SignRequest(req *http.Request, body []byte, creds Credentials, region, service string, signTime time.Time) {
	amzDate := signTime.UTC().Format(awsDateFormat)
	dateStamp := signTime.UTC().Format(awsDateOnlyFormat)

	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	// SigV4 always signs "host" as a real header value. Go's http.Request
	// keeps Host in the dedicated req.Host field rather than req.Header,
	// so callers that only set req.Header (not req.Host) would otherwise
	// silently sign an empty host — set it here rather than relying on
	// every caller to remember.
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	payloadHash := sha256Hex(body)
	canonicalHeaders, signedHeaders := canonicalizeHeaders(req.Header, req.Host)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(creds.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	authHeader := "AWS4-HMAC-SHA256 " +
		"Credential=" + creds.AccessKeyID + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature
	req.Header.Set("Authorization", authHeader)
}

func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func canonicalQuery(u *url.URL) string {
	vals := u.Query()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalizeHeaders returns (canonicalHeaders, signedHeaders). SigV4
// requires at minimum the "host" header to be signed; we sign host plus
// anything already set on the request (e.g. x-amz-date, content-type) so
// the signature actually covers what's sent.
func canonicalizeHeaders(h http.Header, host string) (string, string) {
	headerMap := map[string]string{"host": host}
	for k, v := range h {
		lk := strings.ToLower(k)
		if lk == "authorization" {
			continue // never sign the header we're about to set
		}
		headerMap[lk] = strings.Join(v, ",")
	}

	keys := make([]string, 0, len(headerMap))
	for k := range headerMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canon strings.Builder
	for _, k := range keys {
		canon.WriteString(k)
		canon.WriteString(":")
		canon.WriteString(strings.TrimSpace(headerMap[k]))
		canon.WriteString("\n")
	}
	return canon.String(), strings.Join(keys, ";")
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
