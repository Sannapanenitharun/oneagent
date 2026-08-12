package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file implements Azure Monitor metrics polling via the OAuth2
// client-credentials grant (RFC 6749 §4.4, Azure AD's standard app-only
// auth flow) plus a direct REST call to the Azure Monitor Metrics API —
// not the Azure SDK for Go, for the same dependency-availability reason
// as the other two cloud adapters.
//
// VERIFICATION STATUS: the token-request construction and metrics
// response parsing are unit tested against local mock servers (see
// azuremonitor_test.go). Neither login.microsoftonline.com nor
// management.azure.com are reachable from this sandbox, so — like the
// GCP adapter — the actual live calls are untested against a real Azure
// subscription.

type AzureMonitorConfig struct {
	AgentID        string
	TenantID       string
	ClientID       string
	ClientSecret   string
	SubscriptionID string
	ResourceID     string // full ARM resource ID, e.g. /subscriptions/.../resourceGroups/.../providers/Microsoft.Compute/virtualMachines/vm1
	MetricName     string // e.g. "Percentage CPU"
	Interval       time.Duration
}

type AzureMonitorCollector struct {
	agentID    string
	tenantID   string
	clientID   string
	secret     string
	resourceID string
	metricName string
	interval   time.Duration
	client     *http.Client
	stop       chan struct{}

	// overridable for tests
	tokenEndpoint  string
	managementBase string

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func NewAzureMonitorCollector(cfg AzureMonitorConfig) *AzureMonitorCollector {
	return &AzureMonitorCollector{
		agentID:        cfg.AgentID,
		tenantID:       cfg.TenantID,
		clientID:       cfg.ClientID,
		secret:         cfg.ClientSecret,
		resourceID:     cfg.ResourceID,
		metricName:     cfg.MetricName,
		interval:       cfg.Interval,
		client:         &http.Client{Timeout: 10 * time.Second},
		stop:           make(chan struct{}),
		tokenEndpoint:  fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", cfg.TenantID),
		managementBase: "https://management.azure.com",
	}
}

func (a *AzureMonitorCollector) Name() string { return "azure.monitor" }

func (a *AzureMonitorCollector) Start(ctx context.Context, out chan<- Envelope) error {
	ticker := time.NewTicker(a.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.stop:
				return
			case <-ticker.C:
				if env, err := a.poll(ctx); err == nil {
					out <- env
				}
			}
		}
	}()
	return nil
}

func (a *AzureMonitorCollector) Stop() error {
	close(a.stop)
	return nil
}

func (a *AzureMonitorCollector) poll(ctx context.Context) (Envelope, error) {
	token, err := a.accessToken(ctx)
	if err != nil {
		return Envelope{}, fmt.Errorf("getting azure access token: %w", err)
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-2 * a.interval)
	timespan := startTime.Format(time.RFC3339) + "/" + endTime.Format(time.RFC3339)

	q := url.Values{}
	q.Set("api-version", "2019-07-01")
	q.Set("metricnames", a.metricName)
	q.Set("timespan", timespan)

	reqURL := fmt.Sprintf("%s%s/providers/microsoft.insights/metrics?%s", a.managementBase, a.resourceID, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return Envelope{}, fmt.Errorf("azure monitor request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Envelope{}, err
	}
	if resp.StatusCode >= 300 {
		return Envelope{}, fmt.Errorf("azure monitor http %d: %s", resp.StatusCode, string(body))
	}

	return parseAzureMetrics(body, a.agentID, a.metricName)
}

// accessToken returns a cached AAD app-only token, refreshing via the
// client-credentials grant if expired. Azure AD tokens are typically valid
// for 1 hour; refresh a minute early.
func (a *AzureMonitorCollector) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cachedToken != "" && time.Now().Before(a.tokenExpiry.Add(-1*time.Minute)) {
		return a.cachedToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.secret)
	form.Set("scope", "https://management.azure.com/.default")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
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

	a.cachedToken = tokenResp.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return a.cachedToken, nil
}

// parseAzureMetrics extracts the most recent data point from an Azure
// Monitor Metrics API response.
func parseAzureMetrics(body []byte, agentID, metricName string) (Envelope, error) {
	var parsed struct {
		Value []struct {
			Name struct {
				Value string `json:"value"`
			} `json:"name"`
			Unit       string `json:"unit"`
			Timeseries []struct {
				Data []struct {
					TimeStamp string   `json:"timeStamp"`
					Average   *float64 `json:"average"`
					Total     *float64 `json:"total"`
					Maximum   *float64 `json:"maximum"`
				} `json:"data"`
			} `json:"timeseries"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Envelope{}, fmt.Errorf("parsing azure monitor response: %w", err)
	}
	if len(parsed.Value) == 0 || len(parsed.Value[0].Timeseries) == 0 {
		return Envelope{}, fmt.Errorf("no timeseries in azure monitor response")
	}

	points := parsed.Value[0].Timeseries[0].Data
	if len(points) == 0 {
		return Envelope{}, fmt.Errorf("no data points in azure monitor response window")
	}

	// Azure returns points oldest-first; take the last one.
	latest := points[len(points)-1]
	t, _ := time.Parse(time.RFC3339, latest.TimeStamp)

	var value float64
	switch {
	case latest.Average != nil:
		value = *latest.Average
	case latest.Maximum != nil:
		value = *latest.Maximum
	case latest.Total != nil:
		value = *latest.Total
	}

	return Envelope{
		Kind:      KindMetric,
		AgentID:   agentID,
		Source:    "azure.monitor." + metricName,
		Timestamp: t,
		Labels:    map[string]string{"metric": parsed.Value[0].Name.Value, "unit": parsed.Value[0].Unit},
		Value:     value,
	}, nil
}
