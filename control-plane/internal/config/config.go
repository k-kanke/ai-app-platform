// Package config loads Control Plane settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Listen    string
	DBPath    string
	Namespace string

	// Images run inside generated-app namespaces.
	AgentImage    string
	RuntimeImage  string
	HelperImage   string // trusted platform image for init/snapshot/restore (busybox)
	ImagePullMode string // optional imagePullPolicy override (e.g. IfNotPresent for kind)

	// InternalURL is how Agent Jobs reach this Control Plane.
	InternalURL string
	TokenSecret string

	StorageClass string
	SourceSize   string
	DataSize     string

	Agent              string // which agent script the Agent Job runs: template | gemini | claude
	DefaultStrategy    string // strategy for new apps when a request does not say: inplace | release
	GeminiModel        string // optional model for AAP_AGENT=gemini (empty = CLI default)
	AgentTimeout       time.Duration
	RuntimeTimeout     time.Duration
	PollInterval       time.Duration // how often operations check Jobs / Deployments
	AgentSecretName    string        // optional Secret holding LLM credentials, mounted only into Agent Jobs
	AppURLTemplate     string        // e.g. "http://{id}.apps.home"
	IngressClass       string        // empty = do not create Ingress
	IngressHostPattern string        // e.g. "{id}.apps.home"

	Kubeconfig string // empty = in-cluster

	// Repository tasks (docs/multi-agent-permissions.md). Off unless AAP_TASKS_ENABLED=true.
	TasksEnabled      bool
	TasksNamespace    string        // where task workspaces and Jobs live (not the family-app namespace)
	TaskWorkspaceSize string        // PVC size per task
	TaskRetention     time.Duration // how long a finished task keeps its workspace and patch
	TaskRepoPattern   string        // regular expression a repository ("owner/name") must match
	TaskMaxPatchBytes int           // larger patches are rejected
	TaskMaxActive     int           // tasks running at once
	TaskGitBase       string        // clone URL base; empty = https://github.com (tests point it elsewhere)

	// Publishing a task as a pull request. Off (tasks stop at APPROVED) unless a GitHub App is configured.
	PublisherImage   string
	GitHubAppID      string
	GitHubAppKeyFile string // PEM private key of the GitHub App (a mounted Secret; never in Git)
	GitHubAPIBase    string // empty = https://api.github.com (tests)
}

func Load() (Config, error) {
	c := Config{
		Listen:             env("AAP_LISTEN", ":8080"),
		DBPath:             env("AAP_DB_PATH", "/var/lib/aap/control-plane.db"),
		Namespace:          env("AAP_NAMESPACE", "aap-apps"),
		AgentImage:         env("AAP_AGENT_IMAGE", "aap-agent-runtime:dev"),
		RuntimeImage:       env("AAP_RUNTIME_IMAGE", "aap-app-runtime:dev"),
		HelperImage:        env("AAP_HELPER_IMAGE", "busybox:1.37"),
		ImagePullMode:      env("AAP_IMAGE_PULL_POLICY", ""),
		InternalURL:        env("AAP_INTERNAL_URL", "http://control-plane.platform-system.svc:8080"),
		TokenSecret:        os.Getenv("AAP_TOKEN_SECRET"),
		StorageClass:       os.Getenv("AAP_STORAGE_CLASS"),
		SourceSize:         env("AAP_SOURCE_SIZE", "1Gi"),
		DataSize:           env("AAP_DATA_SIZE", "1Gi"),
		AgentSecretName:    os.Getenv("AAP_AGENT_SECRET_NAME"),
		AppURLTemplate:     os.Getenv("AAP_APP_URL_TEMPLATE"),
		IngressClass:       os.Getenv("AAP_INGRESS_CLASS"),
		IngressHostPattern: os.Getenv("AAP_INGRESS_HOST_PATTERN"),
		Kubeconfig:         os.Getenv("AAP_KUBECONFIG"),
		Agent:              env("AAP_AGENT", "template"),
		GeminiModel:        os.Getenv("AAP_GEMINI_MODEL"),
		DefaultStrategy:    env("AAP_DEFAULT_STRATEGY", "inplace"),
		TasksEnabled:       env("AAP_TASKS_ENABLED", "false") == "true",
		TasksNamespace:     env("AAP_TASKS_NAMESPACE", "aap-agents"),
		TaskWorkspaceSize:  env("AAP_TASK_WORKSPACE_SIZE", "2Gi"),
		TaskRepoPattern:    env("AAP_TASK_REPO_PATTERN", `^k-kanke/[A-Za-z0-9._-]+$`),
		TaskGitBase:        os.Getenv("AAP_TASK_GIT_BASE"),
		PublisherImage:     env("AAP_PUBLISHER_IMAGE", "aap-publisher:dev"),
		GitHubAppID:        os.Getenv("AAP_GITHUB_APP_ID"),
		GitHubAppKeyFile:   os.Getenv("AAP_GITHUB_APP_KEY_FILE"),
		GitHubAPIBase:      os.Getenv("AAP_GITHUB_API_BASE"),
	}
	hours, err := strconv.Atoi(env("AAP_TASK_RETENTION_HOURS", "24"))
	if err != nil || hours <= 0 {
		return c, fmt.Errorf("AAP_TASK_RETENTION_HOURS must be a positive integer")
	}
	c.TaskRetention = time.Duration(hours) * time.Hour
	if c.TaskMaxPatchBytes, err = strconv.Atoi(env("AAP_TASK_MAX_PATCH_BYTES", "1048576")); err != nil || c.TaskMaxPatchBytes <= 0 {
		return c, fmt.Errorf("AAP_TASK_MAX_PATCH_BYTES must be a positive integer")
	}
	if c.TaskMaxActive, err = strconv.Atoi(env("AAP_TASK_MAX_ACTIVE", "2")); err != nil || c.TaskMaxActive <= 0 {
		return c, fmt.Errorf("AAP_TASK_MAX_ACTIVE must be a positive integer")
	}
	mins, err := strconv.Atoi(env("AAP_AGENT_TIMEOUT_MINUTES", "20"))
	if err != nil || mins <= 0 {
		return c, fmt.Errorf("AAP_AGENT_TIMEOUT_MINUTES must be a positive integer")
	}
	c.AgentTimeout = time.Duration(mins) * time.Minute
	secs, err := strconv.Atoi(env("AAP_RUNTIME_TIMEOUT_SECONDS", "300"))
	if err != nil || secs <= 0 {
		return c, fmt.Errorf("AAP_RUNTIME_TIMEOUT_SECONDS must be a positive integer")
	}
	c.RuntimeTimeout = time.Duration(secs) * time.Second
	ms, err := strconv.Atoi(env("AAP_POLL_INTERVAL_MS", "250"))
	if err != nil || ms < 50 {
		return c, fmt.Errorf("AAP_POLL_INTERVAL_MS must be an integer >= 50")
	}
	c.PollInterval = time.Duration(ms) * time.Millisecond
	if c.TokenSecret == "" {
		return c, fmt.Errorf("AAP_TOKEN_SECRET is required (used to sign per-operation agent tokens)")
	}
	return c, nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
