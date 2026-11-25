// Package github is the small slice of the GitHub API the platform needs: minting a short-lived,
// repository-scoped installation token from a GitHub App, and opening a pull request.
//
// The App's private key stays in the Control Plane. Implementer pods never see a GitHub credential;
// only the Publisher Job receives one token (1 hour, one repository, contents + pull requests).
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultAPI = "https://api.github.com"

// App authenticates as a GitHub App.
type App struct {
	ID      string
	Key     *rsa.PrivateKey
	APIBase string // "" = https://api.github.com
	HTTP    *http.Client
	Now     func() time.Time
}

// ParseKey reads an RSA private key in PEM (PKCS#1 or PKCS#8), as GitHub provides it.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("github app key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not an RSA key")
	}
	return rk, nil
}

func (a *App) api() string {
	if a.APIBase != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return DefaultAPI
}
func (a *App) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}
func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// JWT returns the App's own short-lived JWT (RS256, 9 minutes; GitHub allows at most 10).
func (a *App) JWT() (string, error) {
	now := a.now()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.ID})
	signing := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

func (a *App) do(ctx context.Context, method, path, bearer string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.api()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300]
	}
	return fmt.Sprintf("github api: %d %s", e.Status, b)
}

// Token is an installation access token.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// InstallationToken mints a token for repo ("owner/name") limited to that repository and to the
// permissions a publisher needs. The installation is looked up from the repository, so the platform
// stores no installation id; an App that is not installed on the repository fails here (404).
func (a *App) InstallationToken(ctx context.Context, repo string) (Token, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return Token{}, fmt.Errorf("invalid repository %q", repo)
	}
	jwt, err := a.JWT()
	if err != nil {
		return Token{}, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.do(ctx, "GET", "/repos/"+owner+"/"+name+"/installation", jwt, nil, &inst); err != nil {
		return Token{}, fmt.Errorf("find installation of %s: %w", repo, err)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	req := map[string]any{
		"repositories": []string{name},
		"permissions":  map[string]string{"contents": "write", "pull_requests": "write"},
	}
	if err := a.do(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), jwt, req, &out); err != nil {
		return Token{}, fmt.Errorf("create installation token for %s: %w", repo, err)
	}
	if out.Token == "" {
		return Token{}, errors.New("github returned an empty token")
	}
	return Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// Client calls the API with an installation token (used by the Publisher).
type Client struct {
	Token   string
	APIBase string
	HTTP    *http.Client
}

func (c Client) app() *App { return &App{APIBase: c.APIBase, HTTP: c.HTTP} }

// DefaultBranch returns the repository's default branch.
func (c Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.app().do(ctx, "GET", "/repos/"+repo, c.Token, nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", errors.New("repository has no default branch")
	}
	return out.DefaultBranch, nil
}

// PullRequest is the part of a created PR the platform keeps.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// CreatePR opens a regular (non-draft) pull request from head into base.
func (c Client) CreatePR(ctx context.Context, repo, title, body, head, base string) (PullRequest, error) {
	var pr PullRequest
	err := c.app().do(ctx, "POST", "/repos/"+repo+"/pulls", c.Token, map[string]any{
		"title": title, "body": body, "head": head, "base": base, "draft": false,
	}, &pr)
	return pr, err
}

// FindOpenPR returns the open PR from head (a branch of the same repository), if any. It lets a retried
// publish reuse the PR it already opened instead of failing on "a pull request already exists".
func (c Client) FindOpenPR(ctx context.Context, repo, head string) (PullRequest, bool, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var list []PullRequest
	if err := c.app().do(ctx, "GET", "/repos/"+repo+"/pulls?state=open&head="+owner+":"+head, c.Token, nil, &list); err != nil {
		return PullRequest{}, false, err
	}
	if len(list) == 0 {
		return PullRequest{}, false, nil
	}
	return list[0], true, nil
}

// PRStatus is what a pull request is doing now.
type PRStatus struct {
	State  string `json:"state"`  // "open" or "closed"
	Merged bool   `json:"merged"` // closed by a merge
	// MergeCommitSHA is the commit on the base branch that a merge created (for a squash or rebase merge, the resulting commit).
	MergeCommitSHA string `json:"merge_commit_sha"`
}

// PRStatus reads a pull request's state (a merge makes it closed and merged).
func (c Client) PRStatus(ctx context.Context, repo string, number int) (PRStatus, error) {
	var st PRStatus
	err := c.app().do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number), c.Token, nil, &st)
	return st, err
}
