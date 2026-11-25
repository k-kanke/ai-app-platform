// Package llmgateway is the only place that holds the LLM API key.
//
// Implementer pods send their requests here with a short-lived token of their own task in place of an API key
// (Gemini CLI: GOOGLE_GEMINI_BASE_URL + GEMINI_API_KEY). The gateway checks the token, allows only the generate /
// count / embed calls on allowed models, enforces a request budget per task and per day, swaps in the real key,
// and forwards to the provider. A compromised Implementer therefore has no key to steal and a bounded budget to spend.
// docs/multi-agent-permissions.md §3 (point 3).
package llmgateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token returns the credential an Implementer uses in place of an API key: "<task>.<expiry unix>.<hmac>".
// The Control Plane mints it with the same secret the gateway verifies it with.
func Token(secret, taskID string, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	return taskID + "." + exp + "." + sign(secret, taskID, exp)
}

func sign(secret, taskID, exp string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("llm:" + taskID + "." + exp))
	return hex.EncodeToString(m.Sum(nil))
}

var taskIDRe = regexp.MustCompile(`^[a-f0-9]{1,32}$`)

// Verify checks a token and returns its task id.
func Verify(secret, token string, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !taskIDRe.MatchString(parts[0]) {
		return "", fmt.Errorf("malformed token")
	}
	if !hmac.Equal([]byte(sign(secret, parts[0], parts[1])), []byte(parts[2])) {
		return "", fmt.Errorf("bad signature")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() >= exp {
		return "", fmt.Errorf("token expired")
	}
	return parts[0], nil
}

type Config struct {
	Secret      string   // verifies task tokens (the Control Plane's token secret)
	UpstreamKey string   // the real provider API key; empty = every call answers 503
	Upstream    *url.URL // e.g. https://generativelanguage.googleapis.com
	ModelRe     *regexp.Regexp
	MaxBody     int64 // bytes per request
	PerTask     int   // requests one task may make in total
	PerDay      int   // requests across all tasks per UTC day
	Now         func() time.Time
	Log         *slog.Logger
	Transport   http.RoundTripper
}

type Gateway struct {
	cfg   Config
	proxy *httputil.ReverseProxy

	mu    sync.Mutex
	tasks map[string]int
	day   string
	today int
}

// Allowed calls: model-scoped generate / stream / count / embed. Nothing else (no model listing, tuning, files, ...).
var pathRe = regexp.MustCompile(`^/v1(?:beta)?/models/([A-Za-z0-9._-]+):(generateContent|streamGenerateContent|countTokens|embedContent|batchEmbedContents)$`)

func New(cfg Config) *Gateway {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 4 << 20
	}
	g := &Gateway{cfg: cfg, tasks: map[string]int{}}
	g.proxy = &httputil.ReverseProxy{
		FlushInterval: -1, // server-sent events (streamGenerateContent) pass through as they arrive
		Transport:     cfg.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Set("x-goog-api-key", cfg.UpstreamKey)
			q := pr.Out.URL.Query()
			q.Del("key") // a key in the query string would be the Implementer's, never forward it
			pr.Out.URL.RawQuery = q.Encode()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			cfg.Log.Error("upstream error", "path", r.URL.Path, "err", err.Error())
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	return g
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/", g.serve)
	return mux
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("x-goog-api-key")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	task, err := Verify(g.cfg.Secret, token, g.cfg.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m := pathRe.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost {
		http.Error(w, "this call is not allowed", http.StatusNotFound)
		return
	}
	if g.cfg.ModelRe != nil && !g.cfg.ModelRe.MatchString(m[1]) {
		http.Error(w, "this model is not allowed", http.StatusForbidden)
		return
	}
	if g.cfg.UpstreamKey == "" {
		http.Error(w, "the gateway has no provider key configured", http.StatusServiceUnavailable)
		return
	}
	if !g.spend(task) {
		g.cfg.Log.Warn("budget exhausted", "task", task)
		http.Error(w, "request budget exhausted", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxBody)
	g.cfg.Log.Info("llm request", "task", task, "model", m[1], "call", m[2])
	g.proxy.ServeHTTP(w, r)
}

// spend counts one request against the task's and the day's budget. It refuses once either is used up.
func (g *Gateway) spend(task string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if day := g.cfg.Now().UTC().Format("2006-01-02"); day != g.day {
		g.day, g.today = day, 0
	}
	if g.cfg.PerDay > 0 && g.today >= g.cfg.PerDay {
		return false
	}
	if g.cfg.PerTask > 0 && g.tasks[task] >= g.cfg.PerTask {
		return false
	}
	g.today++
	g.tasks[task]++
	return true
}

// Usage reports requests made by a task (for tests and diagnostics).
func (g *Gateway) Usage(task string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tasks[task]
}
