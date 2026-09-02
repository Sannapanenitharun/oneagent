package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-i/agent/internal/collector"
	"github.com/agent-i/agent/internal/config"
	"github.com/agent-i/agent/internal/dashboard"
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

// applyConfig must keep doing two things it already did — apply what it can,
// log what it cannot — and now also publish the skipped set to the dashboard.
// This exercises the wiring end to end rather than testing the store alone.
func TestApplyConfig_PublishesSkippedSettingsAndStillAppliesTheRest(t *testing.T) {
	old := baseConfig()
	d := &Daemon{
		cfg:  old,
		dash: dashboard.NewStore("host-001", "v1", time.Minute, 100),
	}
	aggTicker := time.NewTicker(time.Hour)
	defer aggTicker.Stop()
	spanTicker := time.NewTicker(time.Hour)
	defer spanTicker.Stop()

	// One setting that cannot be applied live (agent_id) alongside one that
	// can (logs.paths). The live one must still take effect.
	next := baseConfig()
	next.AgentID = "renamed-host"
	next.Logs.Paths = []string{"/var/log/nginx/*.log"}

	d.applyConfig(next, aggTicker, spanTicker)

	pending := d.dash.Snapshot().ReloadPendingRestart
	found := false
	for _, p := range pending {
		if p == "agent_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("agent_id missing from reload_pending_restart: %v", pending)
	}
	// logs.paths is applied live, so it must NOT be reported as needing one.
	for _, p := range pending {
		if p == "logs.paths" {
			t.Errorf("logs.paths reported as restart-required, but it reloads live: %v", pending)
		}
	}
	// The whole config is adopted regardless — a skipped setting must not
	// block the reloadable ones from being applied.
	if d.cfg.Logs.Paths[0] != "/var/log/nginx/*.log" {
		t.Errorf("live-reloadable setting was not applied: %v", d.cfg.Logs.Paths)
	}

	// A subsequent clean reload clears the set.
	clean := baseConfig()
	clean.AgentID = "renamed-host"
	clean.Logs.Paths = []string{"/var/log/nginx/*.log"}
	d.applyConfig(clean, aggTicker, spanTicker)
	if got := d.dash.Snapshot().ReloadPendingRestart; len(got) != 0 {
		t.Errorf("pending = %v after a reload with no skipped settings, want none", got)
	}
}

// The dashboard is optional. A reload on an agent without one must not panic.
func TestApplyConfig_NoDashboardIsSafe(t *testing.T) {
	d := &Daemon{cfg: baseConfig()} // dash is nil
	aggTicker := time.NewTicker(time.Hour)
	defer aggTicker.Stop()
	spanTicker := time.NewTicker(time.Hour)
	defer spanTicker.Stop()

	next := baseConfig()
	next.AgentID = "renamed-host"
	d.applyConfig(next, aggTicker, spanTicker) // must not panic
}

// A tailed line is only committed once something downstream has accounted for
// it. The daemon is one of the two places that happens — the aggregator absorbs
// access-log requests into summaries, and those envelopes never reach the
// exporter, so without this they would never advance their file offset and
// every restart would re-read them.
func TestRetire_CommitsTailedEnvelopes(t *testing.T) {
	reg, err := collector.NewOffsetRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatalf("NewOffsetRegistry: %v", err)
	}
	d := &Daemon{cfg: baseConfig(), tailRegistry: reg}

	d.retire(collector.Envelope{
		Kind:   collector.KindAPICall,
		Source: "/var/log/nginx/access.log",
		Labels: map[string]string{
			collector.LabelTailID:  "dev:42",
			collector.LabelTailEnd: "2048",
		},
	})

	offset, _, ok := reg.Lookup("dev:42")
	if !ok {
		t.Fatal("absorbed envelope did not advance the registry")
	}
	if offset != 2048 {
		t.Errorf("offset = %d, want 2048", offset)
	}
}

func TestRetire_IsSafeWithoutATailRegistry(t *testing.T) {
	// Metrics-only agents never construct a registry; retire still runs on
	// every absorbed and exported envelope.
	d := &Daemon{cfg: baseConfig()} // tailRegistry is nil
	d.retire(collector.Envelope{Kind: collector.KindMetric, Source: "host.cpu.used_pct"})
	d.retire(collector.Envelope{
		Kind:   collector.KindLog,
		Source: "/a.log",
		Labels: map[string]string{collector.LabelTailID: "dev:1", collector.LabelTailEnd: "10"},
	})
}

// mergeHostAttributes is what decides whether a late IMDS answer is allowed to
// change what this host says about itself, so its precedence rules are pinned
// here rather than left to the caller to get right.
func TestMergeHostAttributes_AddsWhatWasMissing(t *testing.T) {
	have := map[string]string{"os.type": "linux", "os.name": "Ubuntu"}
	discovered := map[string]string{
		"host.id":          "i-0123456789abcdef0",
		"host.type":        "t3.medium",
		"cloud.account.id": "123456789012",
	}

	merged, added := mergeHostAttributes(have, discovered)

	if merged["host.id"] != "i-0123456789abcdef0" {
		t.Errorf("host.id not added: %v", merged)
	}
	if merged["os.name"] != "Ubuntu" {
		t.Errorf("existing OS description lost: %v", merged)
	}
	if len(added) != 3 {
		t.Errorf("added = %v, want the three discovered keys", added)
	}
}

// A later probe that disagrees means a partial read far more often than it
// means the machine changed, and overwriting good data with it is the worse
// outcome.
func TestMergeHostAttributes_ExistingValuesWin(t *testing.T) {
	have := map[string]string{"host.name": "prod-web-1", "host.id": "i-original"}
	discovered := map[string]string{"host.name": "something-else", "host.id": "i-different"}

	merged, added := mergeHostAttributes(have, discovered)

	if merged["host.name"] != "prod-web-1" || merged["host.id"] != "i-original" {
		t.Fatalf("a later probe overwrote known values: %v", merged)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want nothing — none of it was new", added)
	}
}

// An empty discovered value must not count as an answer, or a probe that
// half-succeeded would look like it had contributed something.
func TestMergeHostAttributes_IgnoresEmptyValues(t *testing.T) {
	merged, added := mergeHostAttributes(
		map[string]string{"os.type": "linux"},
		map[string]string{"host.name": "", "host.id": "i-abc"},
	)
	if _, ok := merged["host.name"]; ok {
		t.Errorf("an empty value became an attribute: %v", merged)
	}
	if len(added) != 1 || added[0] != "host.id" {
		t.Errorf("added = %v, want [host.id]", added)
	}
}

func TestMergeHostAttributes_EmptyInputs(t *testing.T) {
	merged, added := mergeHostAttributes(nil, nil)
	if len(merged) != 0 || len(added) != 0 {
		t.Fatalf("invented something from nothing: %v / %v", merged, added)
	}
	merged, added = mergeHostAttributes(nil, map[string]string{"host.id": "i-abc"})
	if merged["host.id"] != "i-abc" || len(added) != 1 {
		t.Fatalf("a nil starting set should still accept a discovery: %v / %v", merged, added)
	}
}

// The re-probe must not run when there is nothing it could learn, or every
// correctly-identified EC2 host would spend two minutes asking IMDS questions
// it already has the answers to.
func TestRefreshHostAttributes_ReturnsWhenAlreadyComplete(t *testing.T) {
	d := &Daemon{
		cfg:       &config.Config{},
		hostAttrs: map[string]string{"host.id": "i-abc", "host.name": "prod-web-1"},
	}

	done := make(chan struct{})
	go func() {
		d.refreshHostAttributes(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshHostAttributes kept probing for a host that is already fully identified")
	}
}

// Detection turned off means turned off, including for the retry.
func TestRefreshHostAttributes_RespectsDetectionDisabled(t *testing.T) {
	off := false
	d := &Daemon{
		cfg:       &config.Config{EC2Metadata: config.EC2MetadataConfig{Enabled: &off}},
		hostAttrs: map[string]string{},
	}

	done := make(chan struct{})
	go func() {
		d.refreshHostAttributes(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshHostAttributes probed despite ec2_metadata.enabled being false")
	}
}

// A cancelled context has to stop the retry schedule, or shutdown would wait
// out the remaining backoff.
func TestRefreshHostAttributes_StopsOnContextCancel(t *testing.T) {
	d := &Daemon{
		cfg:       &config.Config{EC2Metadata: config.EC2MetadataConfig{Timeout: 50 * time.Millisecond}},
		hostAttrs: map[string]string{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.refreshHostAttributes(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshHostAttributes ignored context cancellation")
	}
}

// The startup banner reports what the agent calls itself, so it has to read
// the resolved name rather than the configured one. Those differ on every host
// that names itself from its hostname or its EC2 Name tag — the default, and
// so the common case: a real host logged "agent_id=" while reporting as
// "teleport", on the one line written specifically to say what it is.
//
// resolveAgentID's own fallback order is covered in agentid_test.go; what this
// pins is that the resolved answer is reachable from outside the package at
// all, which is what the banner needs.
func TestAgentID_ExposesTheResolvedName(t *testing.T) {
	d := &Daemon{agentID: resolveAgentID("", map[string]string{"host.name": "teleport"})}

	if d.AgentID() != "teleport" {
		t.Fatalf("AgentID() = %q, want the resolved name", d.AgentID())
	}
	if d.AgentID() == "" {
		t.Fatal("an empty configured id must not surface as an empty reported id")
	}
}

// Declared attributes fill in what the host cannot know about itself.
func TestMergeDeclaredAttributes_AddsWhatIsNotDetected(t *testing.T) {
	attrs := map[string]string{"host.id": "i-0abc", "os.name": "Ubuntu"}
	got := mergeDeclaredAttributes(attrs, map[string]string{
		"env": "prod", "team": "platform", "owner": "payments",
	})
	for k, want := range map[string]string{
		"env": "prod", "team": "platform", "owner": "payments",
		"host.id": "i-0abc", "os.name": "Ubuntu",
	} {
		if got[k] != want {
			t.Errorf("attrs[%q] = %q, want %q", k, got[k], want)
		}
	}
}

// The rule that protects host identity: a config copied from another machine
// must not be able to relabel this one, because host.id is the join key
// between an instance and every signal it has ever sent.
func TestMergeDeclaredAttributes_DetectedWins(t *testing.T) {
	attrs := map[string]string{"host.id": "i-0real", "cloud.region": "us-east-1"}
	got := mergeDeclaredAttributes(attrs, map[string]string{
		"host.id": "i-0stale", "cloud.region": "eu-west-2", "env": "prod",
	})
	if got["host.id"] != "i-0real" {
		t.Errorf("host.id = %q, want the detected value to win", got["host.id"])
	}
	if got["cloud.region"] != "us-east-1" {
		t.Errorf("cloud.region = %q, want the detected value to win", got["cloud.region"])
	}
	if got["env"] != "prod" {
		t.Errorf("env = %q — a non-conflicting key must still be applied", got["env"])
	}
}

func TestMergeDeclaredAttributes_SkipsEmpties(t *testing.T) {
	got := mergeDeclaredAttributes(map[string]string{}, map[string]string{"": "x", "team": ""})
	if len(got) != 0 {
		t.Errorf("got %v, want empty keys and empty values both ignored", got)
	}
}

// Nothing declared must leave the detected set exactly as it was.
func TestMergeDeclaredAttributes_NilIsANoOp(t *testing.T) {
	attrs := map[string]string{"host.id": "i-0abc"}
	got := mergeDeclaredAttributes(attrs, nil)
	if len(got) != 1 || got["host.id"] != "i-0abc" {
		t.Errorf("got %v, want the detected set untouched", got)
	}
}

// Both settings are read once at startup, so a reload that changed them
// silently would leave every signal wearing the old identity while the config
// on disk said otherwise.
func TestRestartRequired_CoversAttributionSettings(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Containers:         config.ContainersConfig{AppLabel: "com.agent-i.app"},
			ResourceAttributes: map[string]string{"env": "prod"},
		}
	}

	if got := restartRequired(base(), base()); len(got) != 0 {
		t.Errorf("identical configs reported %v as needing a restart", got)
	}

	changedLabel := base()
	changedLabel.Containers.AppLabel = "com.example.app"
	if !listsSetting(restartRequired(base(), changedLabel), "containers.app_label") {
		t.Error("a changed app_label was not reported — it would look applied and do nothing")
	}

	changedAttrs := base()
	changedAttrs.ResourceAttributes = map[string]string{"env": "staging"}
	if !listsSetting(restartRequired(base(), changedAttrs), "resource_attributes") {
		t.Error("changed resource_attributes were not reported")
	}

	addedAttr := base()
	addedAttr.ResourceAttributes = map[string]string{"env": "prod", "team": "platform"}
	if !listsSetting(restartRequired(base(), addedAttr), "resource_attributes") {
		t.Error("an added resource attribute was not reported")
	}

	removedAll := base()
	removedAll.ResourceAttributes = nil
	if !listsSetting(restartRequired(base(), removedAll), "resource_attributes") {
		t.Error("removing every resource attribute was not reported")
	}
}

func listsSetting(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
