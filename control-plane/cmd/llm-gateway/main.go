// Command llm-gateway runs the LLM gateway (see internal/llmgateway). Configuration is by environment:
//
//	AAP_TOKEN_SECRET        shared with the Control Plane; verifies the per-task tokens (required)
//	GEMINI_API_KEY          the real provider key (optional: without it every call answers 503)
//	LLM_UPSTREAM            default https://generativelanguage.googleapis.com
//	LLM_MODEL_PATTERN       allowed model names, default ^gemini-
//	LLM_MAX_REQUESTS_PER_TASK / LLM_MAX_REQUESTS_PER_DAY / LLM_MAX_BODY_BYTES
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/llmgateway"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func num(k string, d int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return d
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("AAP_TOKEN_SECRET")
	if secret == "" {
		log.Error("AAP_TOKEN_SECRET is required")
		os.Exit(2)
	}
	up, err := url.Parse(env("LLM_UPSTREAM", "https://generativelanguage.googleapis.com"))
	if err != nil || up.Scheme == "" || up.Host == "" {
		log.Error("LLM_UPSTREAM is not a URL")
		os.Exit(2)
	}
	re, err := regexp.Compile(env("LLM_MODEL_PATTERN", `^gemini-`))
	if err != nil {
		log.Error("LLM_MODEL_PATTERN is not a regular expression", "err", err)
		os.Exit(2)
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		log.Warn("no provider key configured: every call will answer 503 until GEMINI_API_KEY is set")
	}
	g := llmgateway.New(llmgateway.Config{
		Secret: secret, UpstreamKey: key, Upstream: up, ModelRe: re, Log: log,
		MaxBody: int64(num("LLM_MAX_BODY_BYTES", 4<<20)), PerTask: num("LLM_MAX_REQUESTS_PER_TASK", 400), PerDay: num("LLM_MAX_REQUESTS_PER_DAY", 3000),
	})
	srv := &http.Server{Addr: env("LLM_LISTEN", ":8080"), Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	log.Info("llm gateway listening", "addr", srv.Addr, "upstream", up.Host)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
