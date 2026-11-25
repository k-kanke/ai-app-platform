package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/api"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/config"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/events"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/github"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/orchestrator"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/registry"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var rc *rest.Config
	if cfg.Kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	k := kube.New(cs, cfg)
	broker := events.NewBroker()
	var gh *github.App
	if cfg.TasksEnabled && cfg.GitHubAppID != "" && cfg.GitHubAppKeyFile != "" {
		pemBytes, rerr := os.ReadFile(cfg.GitHubAppKeyFile)
		if rerr != nil {
			return errors.New("cannot read AAP_GITHUB_APP_KEY_FILE: " + rerr.Error())
		}
		key, kerr := github.ParseKey(pemBytes)
		if kerr != nil {
			return kerr
		}
		gh = &github.App{ID: cfg.GitHubAppID, Key: key, APIBase: cfg.GitHubAPIBase}
		log.Info("github app configured: approved tasks will be published as pull requests", "app", cfg.GitHubAppID)
	} else if cfg.TasksEnabled {
		log.Info("no github app configured: approved tasks stay APPROVED (no pull request is opened)")
	}
	orch := orchestrator.New(st, k, broker, orchestrator.Options{
		TokenSecret: cfg.TokenSecret, AgentTimeout: cfg.AgentTimeout + time.Minute, RuntimeTimeout: cfg.RuntimeTimeout, PollInterval: cfg.PollInterval,
		TaskRetention: cfg.TaskRetention, GitHub: gh, LLMGateway: cfg.TasksEnabled && cfg.LLMGatewayURL != "",
		Deploy: cfg.DeployTargets, Registry: registry.Client{}, DeployImageWait: cfg.DeployImageWait, DeployRolloutWait: cfg.DeployRolloutWait,
		TaskWatch: time.Duration(cfg.TaskWatchDays) * 24 * time.Hour,
	}, log)
	var repoRe *regexp.Regexp
	if cfg.TasksEnabled {
		if repoRe, err = regexp.Compile(cfg.TaskRepoPattern); err != nil {
			return errors.New("AAP_TASK_REPO_PATTERN is not a valid regular expression: " + err.Error())
		}
	}
	// Pick up operations that were in flight when the previous process stopped.
	if err := orch.Resume(ctx); err != nil {
		return err
	}

	go orch.RunReconcileLoop(ctx, 5*time.Minute)
	if cfg.TasksEnabled {
		// Repository tasks: pick up unfinished ones, and expire old workspaces and patches.
		if err := orch.ResumeTasks(ctx); err != nil {
			return err
		}
		go orch.RunTaskReconcileLoop(ctx, 10*time.Minute)
		if orch.DeployEnabled() {
			// After a pull request is merged: wait for CI's image and roll the repository's deployment onto it.
			log.Info("deploy step enabled", "targets", len(cfg.DeployTargets))
			go orch.RunDeployLoop(ctx, cfg.DeployPoll)
		}
	}

	srv := &http.Server{
		Addr: cfg.Listen, ReadHeaderTimeout: 10 * time.Second,
		Handler: (&api.Server{St: st, Kube: k, Orch: orch, Broker: broker, BaseCtx: ctx, AppURLTemplate: cfg.AppURLTemplate, DefaultStrategy: cfg.DefaultStrategy,
			TasksEnabled: cfg.TasksEnabled, TaskRepoRe: repoRe, TaskMaxPatch: cfg.TaskMaxPatchBytes, TaskMaxActive: cfg.TaskMaxActive}).Handler(),
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("control plane listening", "addr", cfg.Listen, "namespace", cfg.Namespace)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	orch.Wait()
	return nil
}
