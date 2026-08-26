// Package store persists telemetry in ClickHouse and answers the queries the
// dashboard asks of it.
//
// It talks to ClickHouse over the HTTP interface rather than through a driver.
// That is not a compromise: this repository has no third-party dependencies at
// all — the OTLP protobuf decoder in internal/otlpwire is hand-written for the
// same reason — and ClickHouse's HTTP interface is a first-class protocol that
// accepts INSERT with a FORMAT body and returns SELECT results as JSON. A
// driver would buy connection pooling that net/http already provides and a
// native protocol whose only advantage here would be marginally smaller
// inserts.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a ClickHouse connection. Safe for concurrent use.
type Client struct {
	endpoint string
	database string
	user     string
	password string
	http     *http.Client
}

// Config describes where the database is.
type Config struct {
	// Endpoint is the base URL of the HTTP interface, e.g.
	// http://127.0.0.1:8123. Not the native port — 9000 speaks a different
	// protocol and will simply hang up.
	Endpoint string
	Database string
	User     string
	Password string
	// Timeout bounds a single statement. Inserts are small and frequent;
	// queries are the slow ones, and a query that has not answered in this
	// long is one the dashboard has already given up waiting for.
	Timeout time.Duration
}

const (
	defaultTimeout  = 30 * time.Second
	defaultDatabase = "agenti"
)

func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("store: endpoint is required (the ClickHouse HTTP port, usually 8123)")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("store: bad endpoint %q: %w", cfg.Endpoint, err)
	}
	if cfg.Database == "" {
		cfg.Database = defaultDatabase
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	return &Client{
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		database: cfg.Database,
		user:     cfg.User,
		password: cfg.Password,
		http: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				// Inserts arrive continuously from every agent, so the
				// connections are worth keeping. The default of 2 idle per
				// host would have most inserts paying a fresh TCP and TLS
				// handshake.
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// Ping reports whether the server is reachable and answering.
//
// Deliberately not scoped to the configured database. Selecting it would make
// Ping fail until Migrate has created it — and Migrate runs after the caller
// has waited for Ping to succeed, so a fresh database could never bootstrap.
// The question this answers is "is ClickHouse up", which is a question about
// the server.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.execIn(ctx, "", "SELECT 1", nil, nil, false)
	return err
}

// Migrate creates the schema. Safe to call on every start.
func (c *Client) Migrate(ctx context.Context) error {
	// The database itself first: everything after it is qualified by it, and a
	// missing database is otherwise reported as a syntax error against the
	// first table.
	// Issued with no database selected. Every other statement runs inside
	// c.database, but this one creates it — naming it as the context would ask
	// ClickHouse to resolve a database that does not exist yet, which it
	// rejects before it ever reads the statement.
	if _, err := c.execIn(ctx, "", "CREATE DATABASE IF NOT EXISTS "+quoteIdent(c.database), nil, nil, false); err != nil {
		return fmt.Errorf("store: creating database %s: %w", c.database, err)
	}
	for i, ddl := range migrations() {
		if _, err := c.exec(ctx, ddl, nil); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Insert writes rows to a table using JSONEachRow.
//
// JSONEachRow rather than a binary format because it is self-describing:
// column order in the request does not have to match the table, and a column
// added to the schema later does not break an older writer. At this volume the
// encoding cost is not the bottleneck — the network round trip is, which is
// why the caller batches.
func (c *Client) Insert(ctx context.Context, table string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("store: encoding row for %s: %w", table, err)
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", quoteIdent(c.database), quoteIdent(table))
	if _, err := c.execRaw(ctx, q, &body, true); err != nil {
		return fmt.Errorf("store: inserting %d rows into %s: %w", len(rows), table, err)
	}
	return nil
}

// Query runs a SELECT and decodes the result into out.
//
// The statement must not carry its own FORMAT clause; this appends JSON so the
// shape is predictable. Parameters are passed as ClickHouse query parameters
// ({name:Type} placeholders) rather than interpolated, which is what keeps a
// hostname from a URL out of the SQL text.
func (c *Client) Query(ctx context.Context, sql string, params map[string]string, out any) error {
	raw, err := c.exec(ctx, sql+" FORMAT JSON", params)
	if err != nil {
		return err
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
		Rows int             `json:"rows"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("store: decoding result envelope: %w", err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("store: decoding %d rows: %w", envelope.Rows, err)
	}
	return nil
}

func (c *Client) exec(ctx context.Context, sql string, params map[string]string) ([]byte, error) {
	return c.execParams(ctx, sql, params, nil, false)
}

// execRaw is the single place a request is built, so authentication, database
// selection and error reporting cannot drift between the insert and query
// paths.
//
// inBody distinguishes the two shapes ClickHouse accepts: a statement in the
// query string with the data as the body (inserts), or the whole statement as
// the body (everything else). Sending an INSERT's rows in the query string
// would hit the URL length limit on the first real batch.
func (c *Client) execRaw(ctx context.Context, sql string, body io.Reader, inQueryString bool) ([]byte, error) {
	return c.execParams(ctx, sql, nil, body, inQueryString)
}

func (c *Client) execParams(ctx context.Context, sql string, params map[string]string, body io.Reader, inQueryString bool) ([]byte, error) {
	return c.execIn(ctx, c.database, sql, params, body, inQueryString)
}

// execIn runs a statement with database as the session context. An empty
// database means none is selected, which is required for the statement that
// creates it.
func (c *Client) execIn(ctx context.Context, database, sql string, params map[string]string, body io.Reader, inQueryString bool) ([]byte, error) {
	u, err := url.Parse(c.endpoint + "/")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if database != "" {
		q.Set("database", database)
	}
	// ClickHouse binds {name:Type} placeholders from param_<name>. Passing
	// values this way rather than interpolating them is what keeps a hostname
	// out of a URL from reaching the SQL text.
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	// Ask for errors as a whole response rather than trickled into a partially
	// written 200 body, which is what ClickHouse does by default when a query
	// fails after it has begun streaming.
	q.Set("wait_end_of_query", "1")
	if inQueryString {
		q.Set("query", sql)
	}
	u.RawQuery = q.Encode()

	if !inQueryString {
		body = strings.NewReader(sql)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return nil, err
	}
	// Credentials as headers, not query parameters: a URL ends up in access
	// logs and in error messages, and this one would carry the password.
	if c.user != "" {
		req.Header.Set("X-ClickHouse-User", c.user)
	}
	if c.password != "" {
		req.Header.Set("X-ClickHouse-Key", c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer resp.Body.Close()

	payload, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		// ClickHouse puts a genuinely useful message in the body — the column,
		// the type mismatch, the position in the statement — and discarding it
		// in favour of the status code is how a schema bug becomes "HTTP 400".
		msg := strings.TrimSpace(string(payload))
		if len(msg) > 800 {
			msg = msg[:800] + "…"
		}
		return nil, fmt.Errorf("clickhouse %d: %s", resp.StatusCode, msg)
	}
	if readErr != nil {
		return nil, fmt.Errorf("store: reading response: %w", readErr)
	}
	return payload, nil
}

// quoteIdent wraps a table or database name in backticks and escapes any of
// its own, so an identifier can never end a quoted context early.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
