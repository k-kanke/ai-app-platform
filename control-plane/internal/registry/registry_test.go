package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fake(t *testing.T, digest string, status int) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:k-kanke/genkan:pull" {
				t.Errorf("scope = %q", r.URL.Query().Get("scope"))
			}
			w.Write([]byte(`{"token":"anon"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/k-kanke/genkan/manifests/"):
			if r.Method != http.MethodHead || r.Header.Get("Authorization") != "Bearer anon" || !strings.Contains(r.Header.Get("Accept"), "image.index") {
				t.Errorf("manifest request: %s auth=%q accept=%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Accept"))
			}
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestDigestOfATag(t *testing.T) {
	srv, seen := fake(t, "sha256:abc123", 200)
	d, err := Client{Base: srv.URL}.Digest(context.Background(), "ghcr.io/k-kanke/genkan", "latest")
	if err != nil || d != "sha256:abc123" {
		t.Fatalf("digest = %q err = %v", d, err)
	}
	if len(*seen) != 2 || !strings.HasSuffix((*seen)[1], "/manifests/latest") {
		t.Errorf("requests: %v", *seen)
	}
}

func TestErrorsAreErrors(t *testing.T) {
	srv, _ := fake(t, "", 404)
	if _, err := (Client{Base: srv.URL}).Digest(context.Background(), "ghcr.io/k-kanke/genkan", "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("want a 404 error, got %v", err)
	}
	srv2, _ := fake(t, "not-a-digest", 200)
	if _, err := (Client{Base: srv2.URL}).Digest(context.Background(), "ghcr.io/k-kanke/genkan", "latest"); err == nil {
		t.Error("a missing digest header must be an error")
	}
}

func TestRejectsOddReferences(t *testing.T) {
	c := Client{Base: "http://127.0.0.1:1"} // never reached
	for _, bad := range [][2]string{{"ghcr.io/Bad Name", "latest"}, {"ghcr.io/k-kanke/genkan", ""}, {"ghcr.io/k-kanke/genkan", "a/b"}, {"ghcr.io/../etc", "latest"}, {"ghcr.io/a/../b", "latest"}} {
		if _, err := c.Digest(context.Background(), bad[0], bad[1]); err == nil || strings.Contains(err.Error(), "connect") {
			t.Errorf("%v must be rejected before any request: %v", bad, err)
		}
	}
	if Repository("ghcr.io/k-kanke/genkan") != "k-kanke/genkan" || Repository("k-kanke/genkan") != "k-kanke/genkan" {
		t.Error("Repository")
	}
}
