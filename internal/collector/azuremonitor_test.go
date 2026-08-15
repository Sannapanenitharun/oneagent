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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAzureMonitorCollector_Poll(t *testing.T) {
	var tokenHit, metricsHit bool

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHit = true
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing token form: %v", err)
		}
		if r.FormValue("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", r.FormValue("grant_type"))
		}
		if r.FormValue("client_secret") != "test-secret" {
			t.Errorf("client_secret not passed through correctly")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-azure-token",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	metricsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metricsHit = true
		if got := r.Header.Get("Authorization"); got != "Bearer fake-azure-token" {
			t.Errorf("Authorization = %q, want Bearer fake-azure-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{
					"name": map[string]string{"value": "Percentage CPU"},
					"unit": "Percent",
					"timeseries": []map[string]any{
						{
							"data": []map[string]any{
								{"timeStamp": "2026-08-12T04:00:00Z", "average": 12.0},
								{"timeStamp": "2026-08-12T04:01:00Z", "average": 38.4},
							},
						},
					},
				},
			},
		})
	}))
	defer metricsServer.Close()

	am := NewAzureMonitorCollector(AzureMonitorConfig{
		AgentID:        "test-agent",
		TenantID:       "test-tenant",
		ClientID:       "test-client",
		ClientSecret:   "test-secret",
		SubscriptionID: "test-sub",
		ResourceID:     "/subscriptions/test-sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1",
		MetricName:     "Percentage CPU",
		Interval:       60 * time.Second,
	})
	am.tokenEndpoint = tokenServer.URL
	am.managementBase = metricsServer.URL

	env, err := am.poll(context.Background())
	if err != nil {
		t.Fatalf("poll returned error: %v", err)
	}

	if !tokenHit || !metricsHit {
		t.Errorf("expected both endpoints hit: token=%v metrics=%v", tokenHit, metricsHit)
	}
	// Azure returns points oldest-first — the collector should pick the
	// LAST point (38.4), not the first (12.0).
	if env.Value != 38.4 {
		t.Errorf("expected latest value 38.4, got %v", env.Value)
	}
	if env.Labels["unit"] != "Percent" {
		t.Errorf("unit label missing/wrong: %+v", env.Labels)
	}

	// Cached token should be reused on a second poll.
	tokenHit = false
	if _, err := am.poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if tokenHit {
		t.Error("second poll re-fetched token instead of using cache")
	}
}

func TestParseAzureMetrics_EmptyTimeseries(t *testing.T) {
	_, err := parseAzureMetrics([]byte(`{"value":[{"name":{"value":"x"},"timeseries":[]}]}`), "agent", "x")
	if err == nil {
		t.Error("expected error on empty timeseries, got nil")
	}
}
