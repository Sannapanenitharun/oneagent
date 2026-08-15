//go:build ignore

// DISABLED: this agent currently ships AWS/CloudWatch only.
//
// The code is kept rather than deleted so it can be brought back without
// rewriting it. The build tag above excludes it from compilation; delete that
// line (and uncomment the matching blocks in internal/config/config.go,
// internal/daemon/daemon.go and configs/agent.yaml) to re-enable.
//
// Note that being excluded from the build also means it is excluded from
// go vet, gofmt and the test suite, so it will drift as the rest of the agent
// changes. Expect to fix it up before trusting it again.

package collector

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file implements GCP Cloud Monitoring (the product formerly branded
// "Stackdriver") polling via direct REST calls + a hand-rolled service
// account JWT bearer flow — not the google-cloud-go SDK, for the same
// reason as the CloudWatch adapter: the SDK's dependency tree resolves
// through proxy.golang.org, unreachable in this sandbox. The JWT bearer
// flow (RFC 7523) and the Monitoring REST API are both stable, documented
// wire protocols that only need stdlib crypto + net/http.
//
// VERIFICATION STATUS: the JWT construction and RS256 signing are unit
// tested (signature verifies against the matching public key — see
// gcpmonitoring_test.go) and the REST polling logic is tested against a
// local mock server. Neither oauth2.googleapis.com nor
// monitoring.googleapis.com are reachable from this sandbox, so the actual
// token exchange and live API call have not been run against real GCP.

// gcpServiceAccountKey is the subset of a downloaded GCP service account
// JSON key file this adapter needs.
type gcpServiceAccountKey struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// GCPMonitoringConfig configures polling of one Cloud Monitoring metric
// type for one project.
type GCPMonitoringConfig struct {
	AgentID           string
	ProjectID         string
	MetricType        string // e.g. "compute.googleapis.com/instance/cpu/utilization"
	ServiceAccountKey []byte // raw contents of the downloaded JSON key file
	Interval          time.Duration
}

type GCPMonitoringCollector struct {
	agentID    string
	projectID  string
	metricType string
	interval   time.Duration
	saKey      gcpServiceAccountKey
	privateKey *rsa.PrivateKey
	client     *http.Client
	stop       chan struct{}

	// overridable for tests
	tokenEndpoint     string
	monitoringBaseURL string

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func NewGCPMonitoringCollector(cfg GCPMonitoringConfig) (*GCPMonitoringCollector, error) {
	var saKey gcpServiceAccountKey
	if err := json.Unmarshal(cfg.ServiceAccountKey, &saKey); err != nil {
		return nil, fmt.Errorf("parsing GCP service account key: %w", err)
	}
	privKey, err := parseRSAPrivateKeyPEM(saKey.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("parsing GCP service account private key: %w", err)
	}
	tokenURI := saKey.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	return &GCPMonitoringCollector{
		agentID:           cfg.AgentID,
		projectID:         cfg.ProjectID,
		metricType:        cfg.MetricType,
		interval:          cfg.Interval,
		saKey:             saKey,
		privateKey:        privKey,
		client:            &http.Client{Timeout: 10 * time.Second},
		stop:              make(chan struct{}),
		tokenEndpoint:     tokenURI,
		monitoringBaseURL: "https://monitoring.googleapis.com",
	}, nil
}

func (g *GCPMonitoringCollector) Name() string { return "gcp.monitoring" }

func (g *GCPMonitoringCollector) Start(ctx context.Context, out chan<- Envelope) error {
	ticker := time.NewTicker(g.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-g.stop:
				return
			case <-ticker.C:
				if env, err := g.poll(ctx); err == nil {
					out <- env
				}
			}
		}
	}()
	return nil
}

func (g *GCPMonitoringCollector) Stop() error {
	close(g.stop)
	return nil
}

func (g *GCPMonitoringCollector) poll(ctx context.Context) (Envelope, error) {
	token, err := g.accessToken(ctx)
	if err != nil {
		return Envelope{}, fmt.Errorf("getting GCP access token: %w", err)
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-2 * g.interval)
	filter := fmt.Sprintf(`metric.type="%s"`, g.metricType)

	q := url.Values{}
	q.Set("filter", filter)
	q.Set("interval.startTime", startTime.Format(time.RFC3339))
	q.Set("interval.endTime", endTime.Format(time.RFC3339))

	reqURL := fmt.Sprintf("%s/v3/projects/%s/timeSeries?%s", g.monitoringBaseURL, g.projectID, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := g.client.Do(req)
	if err != nil {
		return Envelope{}, fmt.Errorf("gcp monitoring request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Envelope{}, err
	}
	if resp.StatusCode >= 300 {
		return Envelope{}, fmt.Errorf("gcp monitoring http %d: %s", resp.StatusCode, string(body))
	}

	return parseGCPTimeSeries(body, g.agentID, g.metricType)
}

// accessToken returns a cached OAuth2 bearer token, refreshing it via the
// JWT bearer grant (RFC 7523) if expired or absent. GCP access tokens are
// typically valid for 1 hour; refresh a minute early to avoid using a
// token that expires mid-request.
func (g *GCPMonitoringCollector) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cachedToken != "" && time.Now().Before(g.tokenExpiry.Add(-1*time.Minute)) {
		return g.cachedToken, nil
	}

	jwt, err := buildSignedJWT(g.saKey.ClientEmail, g.tokenEndpoint,
		"https://www.googleapis.com/auth/monitoring.read", g.privateKey, time.Now())
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("token exchange http %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}

	g.cachedToken = tokenResp.AccessToken
	g.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return g.cachedToken, nil
}

// buildSignedJWT constructs and RS256-signs a JWT bearer assertion per
// RFC 7523 / Google's service account OAuth flow.
func buildSignedJWT(issuer, audience, scope string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   issuer,
		"scope": scope,
		"aud":   audience,
		"iat":   now.Unix(),
		"exp":   now.Add(1 * time.Hour).Unix(),
	}

	headerB64, err := base64URLEncodeJSON(header)
	if err != nil {
		return "", err
	}
	claimsB64, err := base64URLEncodeJSON(claims)
	if err != nil {
		return "", err
	}

	signingInput := headerB64 + "." + claimsB64
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func base64URLEncodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func parseRSAPrivateKeyPEM(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}
	// GCP service account keys are PKCS#8-encoded.
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Fall back to PKCS#1 in case of an older/alternate key format.
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	return rsaKey, nil
}

// parseGCPTimeSeries extracts the most recent point from a Cloud Monitoring
// ListTimeSeries response.
func parseGCPTimeSeries(body []byte, agentID, metricType string) (Envelope, error) {
	var parsed struct {
		TimeSeries []struct {
			Resource struct {
				Type   string            `json:"type"`
				Labels map[string]string `json:"labels"`
			} `json:"resource"`
			Points []struct {
				Interval struct {
					EndTime string `json:"endTime"`
				} `json:"interval"`
				Value struct {
					DoubleValue *float64 `json:"doubleValue"`
					Int64Value  *string  `json:"int64Value"`
				} `json:"value"`
			} `json:"points"`
		} `json:"timeSeries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Envelope{}, fmt.Errorf("parsing gcp monitoring response: %w", err)
	}
	if len(parsed.TimeSeries) == 0 || len(parsed.TimeSeries[0].Points) == 0 {
		return Envelope{}, fmt.Errorf("no timeseries points in gcp monitoring response window")
	}

	ts := parsed.TimeSeries[0]
	p := ts.Points[0] // Cloud Monitoring returns points newest-first
	t, _ := time.Parse(time.RFC3339, p.Interval.EndTime)

	var value float64
	switch {
	case p.Value.DoubleValue != nil:
		value = *p.Value.DoubleValue
	case p.Value.Int64Value != nil:
		fmt.Sscanf(*p.Value.Int64Value, "%f", &value)
	}

	labels := map[string]string{"resource.type": ts.Resource.Type}
	for k, v := range ts.Resource.Labels {
		labels[k] = v
	}

	return Envelope{
		Kind:      KindMetric,
		AgentID:   agentID,
		Source:    "gcp.monitoring." + metricType,
		Timestamp: t,
		Labels:    labels,
		Value:     value,
	}, nil
}
