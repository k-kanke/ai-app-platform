// Package registry reads what an image tag currently points to, from a public OCI registry (ghcr.io), without credentials.
//
// The deploy step of a task uses it to know when CI has pushed a new image: the digest of the tag is compared with the
// digest the pods report as their imageID. For a tag that points to an image index (what buildx produces) both are the
// index digest, so the values are directly comparable (checked against the live cluster).
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client talks to a registry. Base is "https://ghcr.io" in production; tests point it at a fake.
type Client struct {
	Base string
	HTTP *http.Client
}

func (c Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return "https://ghcr.io"
}

func (c Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

var repoRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,200}$`)

// Repository returns "owner/name" from "ghcr.io/owner/name" (the registry host is dropped; this client has one registry).
func Repository(image string) string {
	if i := strings.Index(image, "/"); i > 0 && strings.ContainsAny(image[:i], ".:") {
		return image[i+1:]
	}
	return image
}

// Digest returns the digest ("sha256:...") the tag of image currently points to.
func (c Client) Digest(ctx context.Context, image, tag string) (string, error) {
	repo := Repository(image)
	if !repoRe.MatchString(repo) || strings.Contains(repo, "..") || tag == "" || strings.ContainsAny(tag, "/ ?#") {
		return "", fmt.Errorf("invalid image reference %s:%s", image, tag)
	}
	tok, err := c.token(ctx, repo)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, c.base()+"/v2/"+repo+"/manifests/"+url.PathEscape(tag), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"}, ", "))
	resp, err := c.httpc().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry answered %d for %s:%s", resp.StatusCode, repo, tag)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(d, "sha256:") {
		return "", fmt.Errorf("registry sent no digest for %s:%s", repo, tag)
	}
	return d, nil
}

// token fetches an anonymous pull token (public repositories only).
func (c Client) token(ctx context.Context, repo string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/token?scope=repository:"+url.QueryEscape(repo)+":pull", nil)
	resp, err := c.httpc().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token answered %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("registry sent no token")
	}
	return out.Token, nil
}
