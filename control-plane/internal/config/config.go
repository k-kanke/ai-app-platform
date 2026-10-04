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

	Agent              string // which agent script the Agent Job runs: template | claude
	AgentTimeout       time.Duration
	RuntimeTimeout     time.Duration
	AgentSecretName    string // optional Secret holding LLM credentials, mounted only into Agent Jobs
	AppURLTemplate     string // e.g. "http://{id}.apps.home"
	IngressClass       string // empty = do not create Ingress
	IngressHostPattern string // e.g. "{id}.apps.home"

	Kubeconfig string // empty = in-cluster
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
