// Command publisher is the Publisher Job's entrypoint: it publishes one approved task patch as a pull request.
// Configuration comes from environment variables set by the Control Plane (see kube.EnsurePublisherJob).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/publisher"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()

	max, _ := strconv.Atoi(os.Getenv("AAP_MAX_PATCH_BYTES"))
	cfg := publisher.Config{
		Repo: os.Getenv("AAP_REPO"), TaskID: os.Getenv("AAP_TASK_ID"), PatchSHA: os.Getenv("AAP_PATCH_SHA"),
		Token: os.Getenv("AAP_GITHUB_TOKEN"), GitBase: os.Getenv("AAP_GIT_BASE"), APIBase: os.Getenv("AAP_GITHUB_API"),
		PRTitle: os.Getenv("AAP_PR_TITLE"), PRBody: os.Getenv("AAP_PR_BODY"),
		ControlPlaneURL: os.Getenv("AAP_CONTROL_PLANE_URL"), PublisherToken: os.Getenv("AAP_PUBLISHER_TOKEN"),
		MaxPatchBytes: max,
		Log:           func(f string, a ...any) { fmt.Fprintf(os.Stderr, "publisher: "+f+"\n", a...) },
	}
	for name, v := range map[string]string{"AAP_REPO": cfg.Repo, "AAP_TASK_ID": cfg.TaskID, "AAP_PATCH_SHA": cfg.PatchSHA,
		"AAP_GITHUB_TOKEN": cfg.Token, "AAP_CONTROL_PLANE_URL": cfg.ControlPlaneURL, "AAP_PUBLISHER_TOKEN": cfg.PublisherToken, "AAP_PR_TITLE": cfg.PRTitle} {
		if v == "" {
			fmt.Fprintln(os.Stderr, "publisher: missing", name)
			os.Exit(2)
		}
	}
	res, err := publisher.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publisher: FAILED:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "publisher: done: %s %s\n", res.Branch, res.PRURL)
}
