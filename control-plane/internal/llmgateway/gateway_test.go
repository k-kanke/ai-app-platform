package llmgateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type upstream struct {
	*httptest.Server
	gotKey   string
	gotQuery string
	gotPath  string
	gotAuth  string
	calls    int
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls++
		u.gotKey, u.gotQuery, u.gotPath, u.gotAuth = r.Header.Get("x-goog-api-key"), r.URL.RawQuery, r.URL.Path, r.Header.Get("Authorization")
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"a\":1}\n\n"))
			w.(http.Flusher).Flush()
			w.Write([]byte("data: {\"a\":2}\n\n"))
			return
		}
		io.Copy(w, r.Body)
	}))
	t.Cleanup(u.Close)
	return u
}

func setup(t *testing.T, mod func(*Config)) (*Gateway, *httptest.Server, *upstream) {
	t.Helper()
	up := newUpstream(t)
	uu, _ := url.Parse(up.URL)
	cfg := Config{Secret: "s3cret", UpstreamKey: "REAL-KEY", Upstream: uu, ModelRe: regexp.MustCompile(`^gemini-`), PerTask: 3, PerDay: 100, MaxBody: 100}
	if mod != nil {
		mod(&cfg)
	}
	g := New(cfg)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return g, srv, up
}

func call(t *testing.T, srv *httptest.Server, path, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("x-goog-api-key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const gen = "/v1beta/models/gemini-3.8-flash:generateContent"

func tok(task string, ttl time.Duration) string { return Token("s3cret", task, time.Now().Add(ttl)) }

func TestTheRealKeyReplacesTheTaskToken(t *testing.T) {
	g, srv, up := setup(t, nil)
	resp, body := call(t, srv, gen+"?key=STOLEN&alt=sse", tok("abc123", time.Hour), `{"x":1}`)
	if resp.StatusCode != 200 || body != `{"x":1}` {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if up.gotKey != "REAL-KEY" || up.gotAuth != "" || strings.Contains(up.gotQuery, "STOLEN") || !strings.Contains(up.gotQuery, "alt=sse") {
		t.Errorf("upstream saw key=%q auth=%q query=%q", up.gotKey, up.gotAuth, up.gotQuery)
	}
	if up.gotPath != "/v1beta/models/gemini-3.8-flash:generateContent" || g.Usage("abc123") != 1 {
		t.Errorf("path %q usage %d", up.gotPath, g.Usage("abc123"))
	}
}

func TestRejectsBadCredentials(t *testing.T) {
	_, srv, up := setup(t, nil)
	cases := map[string]string{
		"no token":      "",
		"garbage":       "nope",
		"wrong secret":  Token("other", "abc123", time.Now().Add(time.Hour)),
		"expired":       Token("s3cret", "abc123", time.Now().Add(-time.Minute)),
		"tampered task": strings.Replace(tok("abc123", time.Hour), "abc123", "def456", 1),
		"the real key":  "REAL-KEY",
	}
	for name, k := range cases {
		if resp, _ := call(t, srv, gen, k, "{}"); resp.StatusCode != 401 {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	if up.calls != 0 {
		t.Errorf("nothing may reach the provider: %d calls", up.calls)
	}
}

func TestOnlyAllowedCallsAndModels(t *testing.T) {
	_, srv, up := setup(t, nil)
	k := tok("abc123", time.Hour)
	for _, p := range []string{"/v1beta/models", "/v1beta/tunedModels/x:generateContent", "/v1beta/files", "/v1beta/models/gemini-3.8-flash:tuneModel", "/v1beta/cachedContents", "/v1beta/models/gemini-3.8-flash:generateContent/../x"} {
		if resp, _ := call(t, srv, p, k, "{}"); resp.StatusCode != 404 {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
	if resp, _ := call(t, srv, "/v1beta/models/some-other-model:generateContent", k, "{}"); resp.StatusCode != 403 {
		t.Errorf("a model outside the allow list: %d, want 403", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+gen, nil)
	req.Header.Set("x-goog-api-key", k)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 404 {
		t.Errorf("GET on a model call: %d, want 404", resp.StatusCode)
	}
	if up.calls != 0 {
		t.Errorf("nothing may reach the provider: %d calls", up.calls)
	}
}

func TestBudgetsPerTaskAndPerDay(t *testing.T) {
	g, srv, _ := setup(t, func(c *Config) { c.PerTask, c.PerDay = 2, 3 })
	a, b := tok("aaaa", time.Hour), tok("bbbb", time.Hour)
	for i := 0; i < 2; i++ {
		if resp, _ := call(t, srv, gen, a, "{}"); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := call(t, srv, gen, a, "{}"); resp.StatusCode != 429 {
		t.Errorf("a task over its budget: %d, want 429", resp.StatusCode)
	}
	if resp, _ := call(t, srv, gen, b, "{}"); resp.StatusCode != 200 {
		t.Errorf("another task still has its own budget: %d", resp.StatusCode)
	}
	if resp, _ := call(t, srv, gen, tok("cccc", time.Hour), "{}"); resp.StatusCode != 429 {
		t.Errorf("the day's total is used up: %d, want 429", resp.StatusCode)
	}
	if g.Usage("aaaa") != 2 {
		t.Errorf("usage = %d", g.Usage("aaaa"))
	}
}

func TestBudgetResetsTheNextDay(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	_, srv, _ := setup(t, func(c *Config) { c.PerDay = 1; c.PerTask = 0; c.Now = func() time.Time { return now } })
	k := Token("s3cret", "abc123", now.Add(72*time.Hour))
	if resp, _ := call(t, srv, gen, k, "{}"); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := call(t, srv, gen, k, "{}"); resp.StatusCode != 429 {
		t.Fatalf("same day: %d", resp.StatusCode)
	}
	now = now.Add(24 * time.Hour)
	if resp, _ := call(t, srv, gen, k, "{}"); resp.StatusCode != 200 {
		t.Errorf("next day: %d, want 200", resp.StatusCode)
	}
}

func TestBodyLimitAndMissingUpstreamKey(t *testing.T) {
	_, srv, _ := setup(t, nil)
	k := tok("abc123", time.Hour)
	if resp, _ := call(t, srv, gen, k, strings.Repeat("x", 101)); resp.StatusCode != 502 && resp.StatusCode != 413 {
		t.Errorf("an oversized body must not be forwarded: %d", resp.StatusCode)
	}
	_, srv2, up2 := setup(t, func(c *Config) { c.UpstreamKey = "" })
	if resp, _ := call(t, srv2, gen, k, "{}"); resp.StatusCode != 503 || up2.calls != 0 {
		t.Errorf("no provider key: %d calls=%d", resp.StatusCode, up2.calls)
	}
}

func TestStreamingPassesThrough(t *testing.T) {
	_, srv, _ := setup(t, nil)
	resp, body := call(t, srv, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", tok("abc123", time.Hour), "{}")
	if resp.StatusCode != 200 || !strings.Contains(body, `{"a":1}`) || !strings.Contains(body, `{"a":2}`) {
		t.Errorf("stream: %d %q", resp.StatusCode, body)
	}
}
