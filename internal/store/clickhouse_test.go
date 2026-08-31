package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// These run against a real ClickHouse rather than a fake.
//
// A fake would be testing the fake: everything this package does is generate
// SQL and interpret what comes back, and the parts that break are the parts a
// mock cannot have an opinion about — whether a Map column accepts a JSON
// object, whether a parameter placeholder actually binds, whether a DDL is
// really idempotent. Skipped when no server is configured so the suite stays
// runnable without one.
//
//	docker run -d -p 18123:8123 -e CLICKHOUSE_DB=agenti \
//	  -e CLICKHOUSE_USER=agenti -e CLICKHOUSE_PASSWORD=devpass \
//	  clickhouse/clickhouse-server:24-alpine
//	AGENTI_TEST_CLICKHOUSE=http://localhost:18123 go test ./internal/store/
const endpointEnv = "AGENTI_TEST_CLICKHOUSE"

func testClient(t *testing.T) *Client {
	t.Helper()
	endpoint := os.Getenv(endpointEnv)
	if endpoint == "" {
		t.Skipf("%s not set — skipping ClickHouse integration tests", endpointEnv)
	}
	// A database per test run, so a failed run cannot leave rows that make the
	// next one pass or fail for the wrong reason.
	db := fmt.Sprintf("agenti_test_%d", time.Now().UnixNano())
	c, err := New(Config{
		Endpoint: endpoint,
		Database: db,
		User:     envOr("AGENTI_TEST_CLICKHOUSE_USER", "agenti"),
		Password: envOr("AGENTI_TEST_CLICKHOUSE_PASSWORD", "devpass"),
		Timeout:  20 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = c.execRaw(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(db), nil, false)
	})
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestMigrate_IsIdempotent(t *testing.T) {
	c := testClient(t)
	// Already migrated once in testClient; a second and third pass must be
	// harmless, because it runs on every start of every server process.
	for i := 0; i < 2; i++ {
		if err := c.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate pass %d: %v", i+2, err)
		}
	}
}

func TestMigrate_CreatesEverySignalTable(t *testing.T) {
	c := testClient(t)
	var got []struct {
		Name string `json:"name"`
	}
	if err := c.Query(context.Background(), "SELECT name FROM system.tables WHERE database = currentDatabase() ORDER BY name", nil, &got); err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	names := make(map[string]bool, len(got))
	for _, r := range got {
		names[r.Name] = true
	}
	for _, want := range []string{"metrics", "logs", "spans", "hosts"} {
		if !names[want] {
			t.Errorf("table %q missing after migration (have %v)", want, names)
		}
	}
}

func TestInsertAndQuery_RoundTripsAttributes(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")

	err := c.Insert(ctx, "metrics", []map[string]any{{
		"timestamp": now, "name": "system.cpu.time", "host_id": "i-0abc",
		"service": "host", "value": 12.5, "is_monotonic": 1,
		"attributes": map[string]string{"state": "user", "cpu": "cpu-total"},
	}})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var got []struct {
		Name  string `json:"name"`
		State string `json:"state"`
		CPU   string `json:"cpu"`
	}
	err = c.Query(ctx, "SELECT name, attributes['state'] AS state, attributes['cpu'] AS cpu FROM metrics", nil, &got)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].State != "user" || got[0].CPU != "cpu-total" {
		t.Errorf("attributes did not round trip: %+v", got[0])
	}
}

// A parameter that is accepted and then ignored is worse than one that errors:
// the query still returns rows, just the wrong ones. The negative case is what
// proves the binding actually reached the server.
func TestQuery_ParametersActuallyBind(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")

	for _, host := range []string{"i-aaa", "i-bbb"} {
		err := c.Insert(ctx, "metrics", []map[string]any{{
			"timestamp": now, "name": "m", "host_id": host, "service": "host",
			"value": 1, "is_monotonic": 0, "attributes": map[string]string{},
		}})
		if err != nil {
			t.Fatalf("Insert(%s): %v", host, err)
		}
	}

	var matched []struct {
		HostID string `json:"host_id"`
	}
	if err := c.Query(ctx, "SELECT host_id FROM metrics WHERE host_id = {h:String}", map[string]string{"h": "i-aaa"}, &matched); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(matched) != 1 || matched[0].HostID != "i-aaa" {
		t.Fatalf("got %+v, want exactly the one row for i-aaa — the parameter was not applied", matched)
	}

	var none []struct {
		HostID string `json:"host_id"`
	}
	if err := c.Query(ctx, "SELECT host_id FROM metrics WHERE host_id = {h:String}", map[string]string{"h": "absent"}, &none); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("got %d rows for a host that does not exist — the parameter is being ignored", len(none))
	}
}

// A value that would end a quoted string early must be data, not syntax.
func TestQuery_ParameterCannotEscapeIntoSQL(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	if err := c.Insert(ctx, "metrics", []map[string]any{{
		"timestamp": now, "name": "m", "host_id": "i-aaa", "service": "host",
		"value": 1, "is_monotonic": 0, "attributes": map[string]string{},
	}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var got []struct {
		HostID string `json:"host_id"`
	}
	hostile := "' OR 1=1 --"
	if err := c.Query(ctx, "SELECT host_id FROM metrics WHERE host_id = {h:String}", map[string]string{"h": hostile}, &got); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a quote in a parameter matched %d rows — it was interpolated, not bound", len(got))
	}
}

func TestInsert_EmptyIsANoOp(t *testing.T) {
	c := testClient(t)
	if err := c.Insert(context.Background(), "metrics", nil); err != nil {
		t.Fatalf("inserting nothing should not be an error: %v", err)
	}
}

// ClickHouse puts the column, the type and the position in the response body.
// Dropping it in favour of the status code is how a schema bug becomes an
// unactionable "HTTP 400".
func TestExec_SurfacesClickHouseError(t *testing.T) {
	c := testClient(t)
	err := c.Query(context.Background(), "SELECT no_such_column FROM metrics", nil, nil)
	if err == nil {
		t.Fatal("selecting a missing column succeeded")
	}
	if !strings.Contains(err.Error(), "no_such_column") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func TestNew_RequiresAnEndpoint(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New succeeded with no endpoint")
	}
}

func TestQuoteIdent_EscapesBackticks(t *testing.T) {
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("quoteIdent = %s, want an escaped backtick", got)
	}
}
