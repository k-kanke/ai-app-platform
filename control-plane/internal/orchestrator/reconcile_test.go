package orchestrator_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/config"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/events"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/orchestrator"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

func setup(t *testing.T, ingress bool) (*orchestrator.Orchestrator, *store.Store, *fake.Clientset) {
	t.Helper()
	st, _ := store.Open(":memory:")
	cs := fake.NewSimpleClientset()
	cfg := config.Config{Namespace: "aap-apps"}
	if ingress {
		cfg.IngressClass, cfg.IngressHostPattern = "nginx", "{id}.example.test"
	}
	o := orchestrator.New(st, kube.New(cs, cfg), events.NewBroker(), orchestrator.Options{TokenSecret: "x"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return o, st, cs
}

func ingresses(t *testing.T, cs *fake.Clientset) []string {
	t.Helper()
	l, err := cs.NetworkingV1().Ingresses("aap-apps").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range l.Items {
		out = append(out, i.Name+"="+i.Spec.Rules[0].Host)
	}
	return out
}

func TestReconcileCreatesMissingIngressForReadyApps(t *testing.T) {
	ctx := context.Background()
	o, st, cs := setup(t, true)
	st.CreateApp(ctx, "meal", "m")
	st.SetAppPhase(ctx, "meal", store.PhaseReady)
	st.CreateApp(ctx, "building", "b") // still QUEUED: must be left alone
	st.CreateApp(ctx, "busy", "b")
	st.SetAppPhase(ctx, "busy", store.PhaseReady)
	st.CreateOperation(ctx, store.Operation{ID: "op1", AppID: "busy", Kind: store.KindModify, Prompt: "p"}) // active op

	if err := o.ReconcileIngresses(ctx); err != nil {
		t.Fatal(err)
	}
	got := ingresses(t, cs)
	if len(got) != 1 || got[0] != "aap-gen-meal=meal.example.test" {
		t.Fatalf("want only meal's ingress, got %v", got)
	}

	// Idempotent, and an Ingress deleted by hand comes back.
	if err := o.ReconcileIngresses(ctx); err != nil || len(ingresses(t, cs)) != 1 {
		t.Fatalf("second run should be a no-op: %v %v", err, ingresses(t, cs))
	}
	cs.NetworkingV1().Ingresses("aap-apps").Delete(ctx, "aap-gen-meal", metav1.DeleteOptions{})
	o.ReconcileIngresses(ctx)
	if len(ingresses(t, cs)) != 1 {
		t.Fatal("deleted ingress was not restored")
	}
}

func TestReconcileDoesNothingWhenIngressDisabled(t *testing.T) {
	ctx := context.Background()
	o, st, cs := setup(t, false)
	st.CreateApp(ctx, "meal", "m")
	st.SetAppPhase(ctx, "meal", store.PhaseReady)
	if err := o.ReconcileIngresses(ctx); err != nil || len(ingresses(t, cs)) != 0 {
		t.Fatalf("expected no-op, got %v %v", err, ingresses(t, cs))
	}
}
