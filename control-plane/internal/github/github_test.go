package github

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

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func TestParseKeyBothFormats(t *testing.T) {
	k, p1 := testKey(t)
	if got, err := ParseKey(p1); err != nil || got.N.Cmp(k.N) != 0 {
		t.Fatalf("pkcs1: %v", err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if got, err := ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil || got.N.Cmp(k.N) != 0 {
		t.Fatalf("pkcs8: %v", err)
	}
	if _, err := ParseKey([]byte("not pem")); err == nil {
		t.Fatal("garbage must fail")
	}
}

func verifyJWT(t *testing.T, tok string, pub *rsa.PublicKey, wantIss string, now time.Time) {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts: %d", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("bad signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	json.Unmarshal(raw, &c)
	if c.Iss != wantIss || c.Exp-now.Unix() > 600 || c.Exp <= now.Unix() || c.Iat > now.Unix() {
		t.Fatalf("claims: %+v", c)
	}
}

func TestInstallationTokenIsScopedToTheRepository(t *testing.T) {
	key, _ := testKey(t)
	now := time.Now()
	var tokenReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/k-kanke/genkan/installation":
			verifyJWT(t, bearer, &key.PublicKey, "12345", now)
			w.Write([]byte(`{"id": 777}`))
		case "POST /app/installations/777/access_tokens":
			verifyJWT(t, bearer, &key.PublicKey, "12345", now)
			json.NewDecoder(r.Body).Decode(&tokenReq)
			w.WriteHeader(201)
			w.Write([]byte(`{"token":"ghs_scoped","expires_at":"2026-11-25T06:00:00Z"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a := &App{ID: "12345", Key: key, APIBase: srv.URL, Now: func() time.Time { return now }}
	tok, err := a.InstallationToken(context.Background(), "k-kanke/genkan")
	if err != nil || tok.Value != "ghs_scoped" || tok.ExpiresAt.IsZero() {
		t.Fatalf("token: %+v %v", tok, err)
	}
	repos := tokenReq["repositories"].([]any)
	perms := tokenReq["permissions"].(map[string]any)
	if len(repos) != 1 || repos[0] != "genkan" || perms["contents"] != "write" || perms["pull_requests"] != "write" || len(perms) != 2 {
		t.Fatalf("token request must name one repository and only two permissions: %v", tokenReq)
	}
}

func TestAppNotInstalledIsAnError(t *testing.T) {
	key, _ := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	a := &App{ID: "1", Key: key, APIBase: srv.URL}
	if _, err := a.InstallationToken(context.Background(), "k-kanke/other"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404 error, got %v", err)
	}
	if _, err := a.InstallationToken(context.Background(), "no-slash"); err == nil {
		t.Fatal("invalid repo must fail")
	}
}

func TestPullRequestCalls(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_x" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/k-kanke/genkan":
			w.Write([]byte(`{"default_branch":"main"}`))
		case "POST /repos/k-kanke/genkan/pulls":
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(201)
			w.Write([]byte(`{"number":42,"html_url":"https://github.com/k-kanke/genkan/pull/42"}`))
		case "GET /repos/k-kanke/genkan/pulls":
			if r.URL.Query().Get("head") != "k-kanke:agent/t1" || r.URL.Query().Get("state") != "open" {
				t.Errorf("query = %v", r.URL.Query())
			}
			w.Write([]byte(`[{"number":7,"html_url":"https://github.com/k-kanke/genkan/pull/7"}]`))
		}
	}))
	defer srv.Close()
	c := Client{Token: "ghs_x", APIBase: srv.URL}
	ctx := context.Background()
	if b, err := c.DefaultBranch(ctx, "k-kanke/genkan"); err != nil || b != "main" {
		t.Fatalf("default branch: %q %v", b, err)
	}
	pr, err := c.CreatePR(ctx, "k-kanke/genkan", "title", "body", "agent/t1", "main")
	if err != nil || pr.Number != 42 {
		t.Fatalf("create: %+v %v", pr, err)
	}
	if created["draft"] != false || created["head"] != "agent/t1" || created["base"] != "main" {
		t.Errorf("a regular (non-draft) PR into the default branch: %v", created)
	}
	if got, ok, err := c.FindOpenPR(ctx, "k-kanke/genkan", "agent/t1"); err != nil || !ok || got.Number != 7 {
		t.Fatalf("find: %+v %v %v", got, ok, err)
	}
}
