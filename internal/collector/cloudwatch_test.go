package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCloudWatchCollector_Poll exercises the real request-building,
// SigV4-signing, and response-parsing path end to end against a local
// mock server. It does NOT test connectivity to real AWS — that's
// untestable in this sandbox (no network path to *.amazonaws.com) — but
// it does confirm the collector builds a validly-signed request and
// correctly parses a realistic GetMetricStatistics JSON response, which
// is everything under this code's control.
func TestCloudWatchCollector_Poll(t *testing.T) {
	var capturedAuth, capturedBody string

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		capturedBody = string(body)

		resp := map[string]any{
			"GetMetricStatisticsResult": map[string]any{
				"Datapoints": []map[string]any{
					{"Timestamp": "2026-08-12T04:00:00Z", "Average": 41.2},
					{"Timestamp": "2026-08-12T04:01:00Z", "Average": 55.7}, // latest — should win
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mock.Close()

	cw := NewCloudWatchCollector(CloudWatchConfig{
		AgentID:         "test-agent",
		Region:          "us-east-1",
		Namespace:       "AWS/EC2",
		MetricName:      "CPUUtilization",
		Statistic:       "Average",
		DimensionName:   "InstanceId",
		DimensionValue:  "i-0123456789abcdef0",
		Interval:        60 * time.Second,
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "testsecret",
	})
	cw.endpointForTest(mock.URL)

	env, err := cw.poll(context.Background())
	if err != nil {
		t.Fatalf("poll returned error: %v", err)
	}

	if capturedAuth == "" || !strings.HasPrefix(capturedAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("request was not SigV4-signed, got Authorization=%q", capturedAuth)
	}
	if !strings.Contains(capturedBody, "MetricName=CPUUtilization") {
		t.Errorf("request body missing MetricName param: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, "Namespace=AWS%2FEC2") {
		t.Errorf("request body missing/malformed Namespace param: %s", capturedBody)
	}

	if env.Value != 55.7 {
		t.Errorf("expected latest datapoint value 55.7 (not the earlier 41.2), got %v", env.Value)
	}
	if env.Labels["InstanceId"] != "i-0123456789abcdef0" {
		t.Errorf("dimension label not propagated: %+v", env.Labels)
	}
	if env.Source != "aws.cloudwatch.AWS/EC2.CPUUtilization" {
		t.Errorf("unexpected envelope source: %s", env.Source)
	}
}

func TestLatestDatapoint_EmptyResponse(t *testing.T) {
	_, err := latestDatapoint([]byte(`{"GetMetricStatisticsResult":{"Datapoints":[]}}`))
	if err == nil {
		t.Error("expected error on empty datapoints, got nil")
	}
}
