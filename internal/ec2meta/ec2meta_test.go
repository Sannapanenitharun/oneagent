package ec2meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const identityJSON = `{
  "accountId" : "123456789012",
  "architecture" : "x86_64",
  "availabilityZone" : "us-east-1a",
  "imageId" : "ami-0abcdef1234567890",
  "instanceId" : "i-0123456789abcdef0",
  "instanceType" : "t3.medium",
  "privateIp" : "10.0.1.42",
  "region" : "us-east-1",
  "version" : "2017-09-30"
}`

// imdsV2 serves the token handshake and rejects unauthenticated reads, the way
// an instance with IMDSv2 enforced behaves.
func imdsV2(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				t.Errorf("token request missing the TTL header IMDS requires")
			}
			_, _ = w.Write([]byte("AQAAA-test-token"))
		case r.URL.Path == "/latest/dynamic/instance-identity/document":
			if r.Header.Get("X-aws-ec2-metadata-token") != "AQAAA-test-token" {
				w.WriteHeader(http.StatusUnauthorized) // v2 enforced
				return
			}
			_, _ = w.Write([]byte(identityJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestDetect_IMDSv2(t *testing.T) {
	srv := imdsV2(t)
	defer srv.Close()

	got, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.InstanceID != "i-0123456789abcdef0" {
		t.Errorf("instance id = %q", got.InstanceID)
	}
	if got.InstanceType != "t3.medium" {
		t.Errorf("instance type = %q", got.InstanceType)
	}
	if got.Region != "us-east-1" {
		t.Errorf("region = %q", got.Region)
	}
	if got.AccountID != "123456789012" {
		t.Errorf("account = %q", got.AccountID)
	}
	if got.AvailabilityZone != "us-east-1a" {
		t.Errorf("az = %q", got.AvailabilityZone)
	}
	if got.ImageID != "ami-0abcdef1234567890" {
		t.Errorf("ami = %q", got.ImageID)
	}
}

// Older instances still allow unauthenticated reads. The token request fails
// there, and detection has to carry on rather than give up.
func TestDetect_FallsBackToIMDSv1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNotFound) // no v2 support
			return
		}
		if r.URL.Path == "/latest/dynamic/instance-identity/document" {
			_, _ = w.Write([]byte(identityJSON))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background())
	if err != nil {
		t.Fatalf("v1 fallback failed: %v", err)
	}
	if got.InstanceID != "i-0123456789abcdef0" {
		t.Errorf("instance id = %q", got.InstanceID)
	}
}

// The overwhelmingly common case: not an EC2 instance. This must fail, and fail
// quickly, because it runs at startup on every host.
func TestDetect_NotOnEC2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background()); err == nil {
		t.Fatal("expected an error off EC2")
	}
}

// Something else answering on the link-local address must not be mistaken for
// an instance identity document.
func TestDetect_RejectsResponseWithoutInstanceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"region":"us-east-1"}`))
	}))
	defer srv.Close()

	if _, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background()); err == nil {
		t.Fatal("a document with no instanceId was accepted")
	}
}

func TestDetect_GarbageBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	if _, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background()); err == nil {
		t.Fatal("non-JSON body was accepted")
	}
}

// A blackholed link-local address is the realistic non-AWS failure mode: no
// refusal, no answer. Detection must give up on its own timeout rather than
// hanging agent startup.
func TestDetect_HangingEndpointTimesOut(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := newDetectorAt(srv.URL, 200*time.Millisecond).Detect(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hanging endpoint was treated as success")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("took %v to give up; startup would stall", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detection never returned against a hanging endpoint")
	}
}

// A proxy configured in the environment must not receive the instance identity
// request — it is a link-local address, and the document contains the account
// id.
func TestIMDSClient_IgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")

	srv := imdsV2(t)
	defer srv.Close()

	// Succeeding proves the request went direct: the proxy above does not
	// resolve, so a proxied request could not have reached the test server.
	if _, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background()); err != nil {
		t.Fatalf("request was routed through the proxy: %v", err)
	}
}

func TestResourceAttributes(t *testing.T) {
	m := &Metadata{
		InstanceID:       "i-0123456789abcdef0",
		InstanceType:     "t3.medium",
		Region:           "us-east-1",
		AccountID:        "123456789012",
		AvailabilityZone: "us-east-1a",
		ImageID:          "ami-0abcdef1234567890",
	}
	got := m.ResourceAttributes()

	want := map[string]string{
		"cloud.provider":          "aws",
		"cloud.platform":          "aws_ec2",
		"cloud.account.id":        "123456789012",
		"cloud.region":            "us-east-1",
		"cloud.availability_zone": "us-east-1a",
		"host.id":                 "i-0123456789abcdef0",
		"host.type":               "t3.medium",
		"host.image.id":           "ami-0abcdef1234567890",
	}
	if len(got) != len(want) {
		t.Fatalf("attribute count = %d, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// Missing fields are omitted rather than emitted empty: an empty attribute is a
// claim that the value is empty, which is different from not knowing it.
func TestResourceAttributes_OmitsUnknownFields(t *testing.T) {
	got := (&Metadata{InstanceID: "i-abc"}).ResourceAttributes()
	for _, k := range []string{"cloud.region", "host.type", "cloud.account.id", "host.image.id"} {
		if _, present := got[k]; present {
			t.Errorf("%s present despite being unknown", k)
		}
	}
	if got["host.id"] != "i-abc" {
		t.Errorf("host.id = %q", got["host.id"])
	}
}

func TestResourceAttributes_NilIsSafe(t *testing.T) {
	var m *Metadata
	if got := m.ResourceAttributes(); got != nil {
		t.Errorf("nil metadata produced %v", got)
	}
}

// An instance with InstanceMetadataTags enabled exposes its tags, and the Name
// tag is the label an operator recognises the host by.
func TestDetect_ReadsNameTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			_, _ = w.Write([]byte("AQAAA-test-token"))
		case r.URL.Path == "/latest/dynamic/instance-identity/document":
			_, _ = w.Write([]byte(identityJSON))
		case r.URL.Path == "/latest/meta-data/tags/instance/Name":
			if r.Header.Get("X-aws-ec2-metadata-token") != "AQAAA-test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// IMDS returns the bare value; the trailing newline is here to
			// prove it does not survive into an attribute.
			_, _ = w.Write([]byte("prod-web-01\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Name != "prod-web-01" {
		t.Errorf("name = %q, want the trimmed tag value", got.Name)
	}
	if got.ResourceAttributes()["host.name"] != "prod-web-01" {
		t.Errorf("host.name = %q", got.ResourceAttributes()["host.name"])
	}
}

// Tags are not exposed through IMDS unless someone turned that on, so a 404 on
// the tag path is the ordinary case and must not fail detection.
func TestDetect_SucceedsWithoutTags(t *testing.T) {
	srv := imdsV2(t) // serves the identity document and 404s everything else
	defer srv.Close()

	got, err := newDetectorAt(srv.URL, time.Second).Detect(context.Background())
	if err != nil {
		t.Fatalf("detect must succeed without tags: %v", err)
	}
	if got.InstanceID != "i-0123456789abcdef0" {
		t.Errorf("instance id = %q", got.InstanceID)
	}
	if got.Name != "" {
		t.Errorf("name = %q, want empty when tags are not exposed", got.Name)
	}
	if _, ok := got.ResourceAttributes()["host.name"]; ok {
		t.Error("host.name must be absent rather than empty when there is no tag")
	}
}
