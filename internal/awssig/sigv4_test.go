package awssig

import (
	"net/http"
	"testing"
	"time"
)

// TestSignRequest_AWSOfficialVanillaVector checks this implementation
// against the "get-vanilla" case from AWS's own published SigV4 test
// suite (docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html,
// mirrored at github.com/mhart/aws4/tree/master/test/aws-sig-v4-test-suite).
// This is the standard vector used to validate independent SigV4
// implementations — a plain GET / with no query string, signed with the
// documented example credentials.
func TestSignRequest_AWSOfficialVanillaVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Host = "example.amazonaws.com"

	creds := Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	signTime, err := time.Parse("20060102T150405Z", "20150830T123600Z")
	if err != nil {
		t.Fatalf("parsing fixture time: %v", err)
	}

	SignRequest(req, nil, creds, "us-east-1", "service", signTime)

	const wantAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"

	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Errorf("Authorization header mismatch:\n got:  %s\n want: %s", got, wantAuth)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q, want 20150830T123600Z", got)
	}
}

// TestSignRequest_Deterministic guards against accidental nondeterminism
// (e.g. map iteration order leaking into canonical header ordering) —
// signing the same request twice must produce identical signatures.
func TestSignRequest_Deterministic(t *testing.T) {
	creds := Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}
	signTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	sign := func() string {
		req, _ := http.NewRequest(http.MethodGet, "https://monitoring.us-east-1.amazonaws.com/?Action=GetMetricData&Version=2010-08-01", nil)
		req.Host = "monitoring.us-east-1.amazonaws.com"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		SignRequest(req, nil, creds, "us-east-1", "monitoring", signTime)
		return req.Header.Get("Authorization")
	}

	a, b := sign(), sign()
	if a != b {
		t.Errorf("signing is nondeterministic:\n run1: %s\n run2: %s", a, b)
	}
}
