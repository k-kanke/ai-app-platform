package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/api"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/config"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/events"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/orchestrator"
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
	orch := orchestrator.New(st, k, broker, orchestrator.Options{
		TokenSecret: cfg.TokenSecret, AgentTimeout: cfg.AgentTimeout + time.Minute, RuntimeTimeout: cfg.RuntimeTimeout,
	}, log)
	// Pick up operations that were in flight when the previous process stopped.
	if err := orch.Resume(ctx); err != nil {
		return err
	}

	srv := &http.Server{
		Addr: cfg.Listen, ReadHeaderTimeout: 10 * time.Second,
		Handler: (&api.Server{St: st, Kube: k, Orch: orch, Broker: broker, BaseCtx: ctx, AppURLTemplate: cfg.AppURLTemplate}).Handler(),
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
