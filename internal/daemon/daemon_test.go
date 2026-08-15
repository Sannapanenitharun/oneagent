package daemon

import (
	"testing"
	"time"

	"github.com/oneagent/agent/internal/config"
)

func baseConfig() *config.Config {
	return &config.Config{
		AgentID:  "host-001",
		Interval: 15 * time.Second,
		Logs: config.LogsConfig{
			Enabled: true,
			Paths:   []string{"/var/log/app/*.log"},
		},
		Traces: config.TracesConfig{
			Enabled:    true,
			ListenAddr: "127.0.0.1:4319",
		},
		Exporter: config.ExporterConfig{
			Type:     "otlp_http",
			Endpoint: "http://backend:4318",
		},
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestRestartRequired_IdenticalConfigsNeedNothing(t *testing.T) {
	if got := restartRequired(baseConfig(), baseConfig()); len(got) != 0 {
		t.Errorf("identical configs reported %v as needing a restart", got)
	}
}

// TestRestartRequired_FlagsStructuralChanges: these cannot be applied to a
// running agent, and reporting them is the point — silently ignoring an edited
// listen address would leave someone convinced it had taken effect.
func TestRestartRequired_FlagsStructuralChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"agent id", func(c *config.Config) { c.AgentID = "host-002" }, "agent_id"},
		{"interval", func(c *config.Config) { c.Interval = time.Minute }, "interval"},
		{"listen addr", func(c *config.Config) { c.Traces.ListenAddr = "0.0.0.0:4319" }, "traces.listen_addr"},
		{"traces off", func(c *config.Config) { c.Traces.Enabled = false }, "traces.enabled"},
		{"exporter endpoint", func(c *config.Config) { c.Exporter.Endpoint = "http://other:4318" }, "exporter"},
		{"exporter type", func(c *config.Config) { c.Exporter.Type = "stdout" }, "exporter"},
		{"logs disabled", func(c *config.Config) { c.Logs.Enabled = false }, "logs.enabled"},
		{"registry path", func(c *config.Config) { c.Tailing.RegistryPath = "/tmp/other.json" }, "tailing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := baseConfig()
			updated := baseConfig()
			tc.mutate(updated)
			got := restartRequired(old, updated)
			if !contains(got, tc.want) {
				t.Errorf("changing %s reported %v, expected it to include %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestRestartRequired_AllowsReloadableChanges is the other half: the settings
// people actually tune must NOT be reported as needing a restart, or the
// warning becomes noise everyone learns to ignore.
func TestRestartRequired_AllowsReloadableChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"log paths", func(c *config.Config) { c.Logs.Paths = []string{"/var/log/other/*.log"} }},
		{"aggregation toggle", func(c *config.Config) { c.Aggregation.Enabled = true }},
		{"aggregation interval", func(c *config.Config) { c.Aggregation.Interval = 30 * time.Second }},
		{"sampling rate", func(c *config.Config) { c.Traces.Sampling.Rate = 0.5 }},
		{"sampling toggle", func(c *config.Config) { c.Traces.Sampling.Enabled = true }},
		{"trace stats toggle", func(c *config.Config) { c.Traces.Stats.Enabled = true }},
		{"slow threshold", func(c *config.Config) { c.Traces.Sampling.SlowThresholdMs = 500 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := baseConfig()
			updated := baseConfig()
			tc.mutate(updated)
			if got := restartRequired(old, updated); len(got) != 0 {
				t.Errorf("changing %s should be reloadable, but reported %v", tc.name, got)
			}
		})
	}
}

func TestEqualStrings(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
		{[]string{"a", "b"}, []string{"b", "a"}, false},
	}
	for _, c := range cases {
		if got := equalStrings(c.a, c.b); got != c.want {
			t.Errorf("equalStrings(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
