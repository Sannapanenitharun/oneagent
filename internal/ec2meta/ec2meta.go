// Package ec2meta reads the EC2 instance identity document from the Instance
// Metadata Service, so telemetry from an EC2 host carries the instance id,
// type, region and account rather than just a hostname.
//
// This is not a cloud API client and needs no credentials. IMDS lives at a
// link-local address that only the instance itself can reach, answers only
// about the instance asking, and requires no access key, no IAM permission and
// no configuration. That distinction matters in this codebase: the CloudWatch
// collector removed earlier reached out to an AWS API with credentials to ask
// about other resources, which is a different thing entirely from the host
// describing itself.
//
// Everything the agent needs comes from one document:
//
//	GET /latest/dynamic/instance-identity/document
//	{"accountId":"…","region":"…","instanceId":"i-…","instanceType":"t3.medium",
//	 "availabilityZone":"us-east-1a","imageId":"ami-…", …}
//
// so detection is a token request plus a single GET.
//
// The instance's Name tag is fetched separately and optionally:
//
//	GET /latest/meta-data/tags/instance/Name
//
// Tags are not part of the identity document and are not exposed by IMDS at
// all unless the instance was launched with InstanceMetadataTags enabled, or
// had it turned on later with modify-instance-metadata-options. Where that was
// not done the path 404s, which is the common case and not an error. Reading
// the tag any other way would mean calling ec2:DescribeTags with credentials —
// a cloud API client, which this deliberately is not.
package ec2meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is the IMDS link-local address, fixed by AWS.
const DefaultEndpoint = "http://169.254.169.254"

// DefaultTimeout bounds the whole detection.
//
// It is short on purpose. This runs at startup on every host, and the common
// case for a non-EC2 machine is that the link-local address either refuses
// immediately or blackholes and never answers. A generous timeout turns the
// second case into a startup stall on every laptop and every non-AWS server,
// which is a far more likely outcome than a slow-but-present IMDS.
const DefaultTimeout = time.Second

// Metadata is the subset of the identity document the agent uses.
type Metadata struct {
	InstanceID       string
	InstanceType     string
	Region           string
	AccountID        string
	AvailabilityZone string
	ImageID          string
	// Name is the instance's Name tag — the label the console shows and the
	// one people actually use to refer to a host. Empty whenever the instance
	// does not expose tags through IMDS, which is the default.
	Name string
}

// identityDocument mirrors the JSON field names IMDS returns.
type identityDocument struct {
	AccountID        string `json:"accountId"`
	Region           string `json:"region"`
	InstanceID       string `json:"instanceId"`
	InstanceType     string `json:"instanceType"`
	AvailabilityZone string `json:"availabilityZone"`
	ImageID          string `json:"imageId"`
}

// Detector queries IMDS. The zero value is not usable; call NewDetector.
type Detector struct {
	endpoint string
	client   *http.Client
}

// NewDetector builds a detector against the real IMDS endpoint.
func NewDetector(timeout time.Duration) *Detector {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Detector{endpoint: DefaultEndpoint, client: newIMDSClient(timeout)}
}

// newDetectorAt points a detector at an arbitrary base URL, for tests.
func newDetectorAt(endpoint string, timeout time.Duration) *Detector {
	return &Detector{endpoint: endpoint, client: newIMDSClient(timeout)}
}

// newIMDSClient builds the HTTP client used for metadata requests.
//
// Two settings are deliberate rather than incidental:
//
// Proxy is nil. The default transport honours HTTP_PROXY/http_proxy from the
// environment, and a host with a proxy configured would otherwise send its
// instance-identity request — account id included — to that proxy instead of to
// the link-local address. A link-local destination must never be proxied, so
// this opts out rather than relying on NO_PROXY being set correctly.
//
// Redirects are refused. Nothing at IMDS legitimately redirects, and following
// one would mean sending metadata credentials-style headers to whatever the
// response named.
func newIMDSClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
			ResponseHeaderTimeout: timeout,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("ec2meta: refusing to follow a redirect from IMDS")
		},
	}
}

// Detect fetches the instance identity document.
//
// It returns an error on any host that is not an EC2 instance, which is an
// ordinary outcome rather than a fault: callers are expected to carry on
// without the attributes.
func (d *Detector) Detect(ctx context.Context) (*Metadata, error) {
	// IMDSv2 is the default on current instances and can be enforced, in which
	// case an unauthenticated GET returns 401. IMDSv1 is still enabled on many
	// older instances and needs no token. Trying v2 first and falling back
	// covers both without needing to know which is configured.
	token, _ := d.fetchToken(ctx)

	body, err := d.get(ctx, "/latest/dynamic/instance-identity/document", token)
	if err != nil {
		return nil, err
	}

	var doc identityDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("ec2meta: parsing identity document: %w", err)
	}
	// A response that parses but carries no instance id is not EC2 — most
	// likely something else answering on the link-local address.
	if doc.InstanceID == "" {
		return nil, errors.New("ec2meta: identity document has no instanceId")
	}

	return &Metadata{
		InstanceID:       doc.InstanceID,
		InstanceType:     doc.InstanceType,
		Region:           doc.Region,
		AccountID:        doc.AccountID,
		AvailabilityZone: doc.AvailabilityZone,
		ImageID:          doc.ImageID,
		// Deliberately last, and deliberately ignoring its error: the instance
		// is already identified by this point, and a missing Name tag must not
		// turn a successful detection into a failed one.
		Name: d.fetchNameTag(ctx, token),
	}, nil
}

// fetchNameTag reads the instance's Name tag, returning "" when it is not
// available for any reason.
//
// Every failure mode here is ordinary rather than exceptional — tags not
// exposed through IMDS at all (the default, a 404), no Name tag set on an
// instance that does expose them (also a 404), or the request timing out — and
// each one means the same thing to the caller: there is no name to use. So the
// signature reports absence rather than an error nobody would act on.
func (d *Detector) fetchNameTag(ctx context.Context, token string) string {
	body, err := d.get(ctx, "/latest/meta-data/tags/instance/Name", token)
	if err != nil {
		return ""
	}
	// IMDS returns the raw tag value with no quoting or trailing newline, but a
	// tag can legitimately have been set with surrounding whitespace, and that
	// would end up in a resource attribute and an agent id.
	return strings.TrimSpace(string(body))
}

// fetchToken performs the IMDSv2 handshake. A failure is not fatal: the caller
// retries unauthenticated, which succeeds wherever IMDSv1 is still enabled.
func (d *Detector) fetchToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.endpoint+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	// Six hours is the maximum IMDS allows. The token is used once, moments
	// from now, so the TTL is immaterial — it is required to be present.
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ec2meta: token request returned %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *Detector) get(ctx context.Context, path, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-aws-ec2-metadata-token", token)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ec2meta: %s returned %d", path, resp.StatusCode)
	}
	// The identity document is well under a kilobyte; the cap is here so a
	// hostile responder on the link-local address cannot stream indefinitely.
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// ResourceAttributes renders the metadata as OpenTelemetry resource attributes.
//
// The names follow the OTel cloud and host semantic conventions, which is what
// makes a backend recognise the signal as coming from a specific EC2 instance
// rather than an anonymous Linux box.
//
// Note that host.id is the instance id here. That is what the convention
// specifies on EC2, and it is what links telemetry to the instance — but it
// replaces the /etc/machine-id value the agent uses elsewhere, so a host
// already being monitored will appear under a new identity the first time it
// reports with detection enabled.
func (m *Metadata) ResourceAttributes() map[string]string {
	if m == nil {
		return nil
	}
	attrs := map[string]string{
		"cloud.provider": "aws",
		"cloud.platform": "aws_ec2",
	}
	set := func(k, v string) {
		if v != "" {
			attrs[k] = v
		}
	}
	set("cloud.account.id", m.AccountID)
	set("cloud.region", m.Region)
	set("cloud.availability_zone", m.AvailabilityZone)
	set("host.id", m.InstanceID)
	set("host.type", m.InstanceType)
	set("host.image.id", m.ImageID)
	// host.name is the Name tag rather than the private DNS name. The
	// convention permits either, and the tag is what an operator recognises;
	// the DNS name is derived from the private IP and tells them nothing they
	// cannot already see. Absent unless the instance exposes tags, in which
	// case the attribute is simply not set rather than guessed at.
	set("host.name", m.Name)
	return attrs
}
