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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// genTestServiceAccountKey generates a fresh RSA keypair and returns the
// GCP-service-account-shaped JSON key bytes plus the matching public key,
// so the test can verify signatures independently rather than trusting
// the code under test to check its own work.
func genTestServiceAccountKey(t *testing.T, tokenURI string) ([]byte, *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshaling PKCS8: %v", err)
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	key := gcpServiceAccountKey{
		Type:        "service_account",
		ProjectID:   "test-project",
		PrivateKey:  string(pemBlock),
		ClientEmail: "test-agent@test-project.iam.gserviceaccount.com",
		TokenURI:    tokenURI,
	}
	b, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshaling test key: %v", err)
	}
	return b, &priv.PublicKey
}

func TestBuildSignedJWT_VerifiesAgainstPublicKey(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	jwt, err := buildSignedJWT("test@example.iam.gserviceaccount.com", "https://oauth2.googleapis.com/token",
		"https://www.googleapis.com/auth/monitoring.read", priv, time.Now())
	if err != nil {
		t.Fatalf("buildSignedJWT: %v", err)
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3-part JWT, got %d parts", len(parts))
	}

	// Independently verify the signature against the public key — this is
	// the actual correctness check, not just "did it produce 3 dots".
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	hashed := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&priv.PublicKey, crypto.SHA256, hashed[:], sig); err != nil {
		t.Errorf("JWT signature does not verify against the signing key's public key: %v", err)
	}

	// Confirm claims round-trip correctly.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshaling claims: %v", err)
	}
	if claims["iss"] != "test@example.iam.gserviceaccount.com" {
		t.Errorf("iss claim = %v, want test@example.iam.gserviceaccount.com", claims["iss"])
	}
	if claims["aud"] != "https://oauth2.googleapis.com/token" {
		t.Errorf("aud claim = %v", claims["aud"])
	}
}

func TestGCPMonitoringCollector_Poll(t *testing.T) {
	var tokenServerHit, monitoringServerHit bool

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenServerHit = true
		if err := r.ParseForm(); err != nil {
			t.Errorf("token request form parse: %v", err)
		}
		if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("unexpected grant_type: %s", r.FormValue("grant_type"))
		}
		if r.FormValue("assertion") == "" {
			t.Error("token request missing JWT assertion")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token-abc123",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	monitoringServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		monitoringServerHit = true
		if got := r.Header.Get("Authorization"); got != "Bearer fake-token-abc123" {
			t.Errorf("Authorization header = %q, want Bearer fake-token-abc123", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timeSeries": []map[string]any{
				{
					"resource": map[string]any{
						"type":   "gce_instance",
						"labels": map[string]string{"instance_id": "1234567890"},
					},
					"points": []map[string]any{
						{
							"interval": map[string]any{"endTime": "2026-08-12T04:05:00Z"},
							"value":    map[string]any{"doubleValue": 72.5},
						},
					},
				},
			},
		})
	}))
	defer monitoringServer.Close()

	keyBytes, _ := genTestServiceAccountKey(t, tokenServer.URL)

	gm, err := NewGCPMonitoringCollector(GCPMonitoringConfig{
		AgentID:           "test-agent",
		ProjectID:         "test-project",
		MetricType:        "compute.googleapis.com/instance/cpu/utilization",
		ServiceAccountKey: keyBytes,
		Interval:          60 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewGCPMonitoringCollector: %v", err)
	}
	gm.monitoringBaseURL = monitoringServer.URL

	env, err := gm.poll(context.Background())
	if err != nil {
		t.Fatalf("poll returned error: %v", err)
	}

	if !tokenServerHit {
		t.Error("token endpoint was never called")
	}
	if !monitoringServerHit {
		t.Error("monitoring endpoint was never called")
	}
	if env.Value != 72.5 {
		t.Errorf("expected value 72.5, got %v", env.Value)
	}
	if env.Labels["instance_id"] != "1234567890" {
		t.Errorf("resource labels not propagated: %+v", env.Labels)
	}

	// Second poll within the token's lifetime should reuse the cached
	// token, not hit the token endpoint again.
	tokenServerHit = false
	if _, err := gm.poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if tokenServerHit {
		t.Error("second poll re-fetched the token instead of using the cache")
	}
}
