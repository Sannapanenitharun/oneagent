package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agent-i/agent/internal/awssig"
)

// CloudWatchCollector polls AWS CloudWatch's GetMetricData API on an
// interval. Uses the "query" protocol (form-encoded POST body, SigV4
// signed) directly against the CloudWatch endpoint rather than the AWS
// SDK for Go — the SDK's dependency tree resolves through
// proxy.golang.org, which this sandbox can't reach; the query protocol
// itself is a stable, documented wire format that only needs net/http.
//
// VERIFICATION STATUS: the SigV4 signing logic (awssig package) is
// verified against AWS's own published test vector — see
// internal/awssig/sigv4_test.go. The actual GetMetricData request/response
// handling below is written to the documented API shape but has NOT been
// exercised against a live AWS account: this sandbox has no network path
// to *.amazonaws.com and no AWS credentials to test with. Treat this as
// "correctly implements the documented protocol" rather than "confirmed
// working in production" until it's been run against a real account.
type CloudWatchCollector struct {
	agentID   string
	region    string
	namespace string
	metric    string
	stat      string // e.g. "Average", "Maximum"
	dimName   string
	dimValue  string
	interval  time.Duration
	creds     awssig.Credentials
	client    *http.Client
	stop      chan struct{}

	// endpoint defaults to the real CloudWatch endpoint for the configured
	// region; overridable (endpointForTest) so tests can point it at a
	// local httptest server instead of *.amazonaws.com.
	endpoint string
}

// CloudWatchConfig carries the pieces needed to poll one metric stream.
// Credentials are passed in already-resolved (the daemon wiring layer
// reads them from environment variables — see config.go — so this
// collector never touches env vars or secret files directly).
type CloudWatchConfig struct {
	AgentID         string
	Region          string
	Namespace       string
	MetricName      string
	Statistic       string
	DimensionName   string
	DimensionValue  string
	Interval        time.Duration
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func NewCloudWatchCollector(cfg CloudWatchConfig) *CloudWatchCollector {
	stat := cfg.Statistic
	if stat == "" {
		stat = "Average"
	}
	return &CloudWatchCollector{
		agentID:   cfg.AgentID,
		region:    cfg.Region,
		namespace: cfg.Namespace,
		metric:    cfg.MetricName,
		stat:      stat,
		dimName:   cfg.DimensionName,
		dimValue:  cfg.DimensionValue,
		interval:  cfg.Interval,
		creds: awssig.Credentials{
			AccessKeyID:     cfg.AccessKeyID,
			SecretAccessKey: cfg.SecretAccessKey,
			SessionToken:    cfg.SessionToken,
		},
		client:   &http.Client{Timeout: 10 * time.Second},
		stop:     make(chan struct{}),
		endpoint: fmt.Sprintf("https://monitoring.%s.amazonaws.com/", cfg.Region),
	}
}

// endpointForTest overrides the CloudWatch endpoint URL — test-only seam,
// not exposed via CloudWatchConfig so production callers can't hit the
// wrong host by accident.
func (c *CloudWatchCollector) endpointForTest(url string) { c.endpoint = url }

func (c *CloudWatchCollector) Name() string { return "aws.cloudwatch" }

func (c *CloudWatchCollector) Start(ctx context.Context, out chan<- Envelope) error {
	if c.creds.AccessKeyID == "" || c.creds.SecretAccessKey == "" {
		return fmt.Errorf("cloudwatch collector: AWS credentials not set")
	}
	ticker := time.NewTicker(c.interval)
	go func() {
		defer ticker.Stop()
		plog := &pollLogger{name: c.Name()}
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stop:
				return
			case <-ticker.C:
				// One failed poll (throttling, a transient blip) must not kill
				// the collector goroutine, but it must not vanish either —
				// pollLogger reports it without flooding the journal.
				env, err := c.poll(ctx)
				if err != nil {
					plog.fail(err)
					continue
				}
				plog.ok()
				select {
				case out <- env:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return nil
}

func (c *CloudWatchCollector) Stop() error {
	close(c.stop)
	return nil
}

// poll issues one GetMetricStatistics call covering the last two collection
// intervals (so a single missed/late datapoint doesn't produce an empty
// result) and returns the most recent datapoint as an Envelope.
func (c *CloudWatchCollector) poll(ctx context.Context) (Envelope, error) {
	endTime := time.Now().UTC()
	startTime := endTime.Add(-2 * c.interval)

	form := url.Values{}
	form.Set("Action", "GetMetricStatistics")
	form.Set("Version", "2010-08-01")
	form.Set("Namespace", c.namespace)
	form.Set("MetricName", c.metric)
	form.Set("StartTime", startTime.Format(time.RFC3339))
	form.Set("EndTime", endTime.Format(time.RFC3339))
	form.Set("Period", strconv.Itoa(int(c.interval.Seconds())))
	form.Set("Statistics.member.1", c.stat)
	if c.dimName != "" {
		form.Set("Dimensions.member.1.Name", c.dimName)
		form.Set("Dimensions.member.1.Value", c.dimValue)
	}

	body := []byte(form.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return Envelope{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	awssig.SignRequest(req, body, c.creds, c.region, "monitoring", time.Now().UTC())

	resp, err := c.client.Do(req)
	if err != nil {
		return Envelope{}, fmt.Errorf("cloudwatch request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Envelope{}, err
	}
	if resp.StatusCode >= 300 {
		return Envelope{}, fmt.Errorf("cloudwatch http %d: %s", resp.StatusCode, string(respBody))
	}

	dp, err := latestDatapoint(respBody)
	if err != nil {
		return Envelope{}, err
	}

	labels := map[string]string{"namespace": c.namespace, "metric": c.metric, "statistic": c.stat}
	if c.dimName != "" {
		labels[c.dimName] = c.dimValue
	}

	return Envelope{
		Kind:      KindMetric,
		AgentID:   c.agentID,
		Source:    "aws.cloudwatch." + c.namespace + "." + c.metric,
		Timestamp: dp.timestamp,
		Labels:    labels,
		Value:     dp.value,
	}, nil
}

type datapoint struct {
	timestamp time.Time
	value     float64
}

// latestDatapoint parses CloudWatch's GetMetricStatisticsResponse (the
// classic query-protocol XML API also supports a JSON response body when
// Accept: application/json is sent) and returns the most recent point.
func latestDatapoint(body []byte) (datapoint, error) {
	var parsed struct {
		GetMetricStatisticsResult struct {
			Datapoints []struct {
				Timestamp string  `json:"Timestamp"`
				Average   float64 `json:"Average"`
				Maximum   float64 `json:"Maximum"`
				Minimum   float64 `json:"Minimum"`
				Sum       float64 `json:"Sum"`
			} `json:"Datapoints"`
		} `json:"GetMetricStatisticsResult"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return datapoint{}, fmt.Errorf("parsing cloudwatch response: %w", err)
	}
	points := parsed.GetMetricStatisticsResult.Datapoints
	if len(points) == 0 {
		return datapoint{}, fmt.Errorf("no datapoints in cloudwatch response window")
	}

	latest := points[0]
	latestTime, _ := time.Parse(time.RFC3339, latest.Timestamp)
	for _, p := range points[1:] {
		t, err := time.Parse(time.RFC3339, p.Timestamp)
		if err == nil && t.After(latestTime) {
			latest, latestTime = p, t
		}
	}

	value := latest.Average
	switch {
	case latest.Maximum != 0 && latest.Average == 0:
		value = latest.Maximum
	case latest.Sum != 0 && latest.Average == 0 && latest.Maximum == 0:
		value = latest.Sum
	}

	return datapoint{timestamp: latestTime, value: value}, nil
}
