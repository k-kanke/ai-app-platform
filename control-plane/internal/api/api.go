// Package api exposes the REST + SSE interface used by the Portal and the
// internal progress endpoint used by Agent Jobs.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/events"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/orchestrator"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

type Server struct {
	St     *store.Store
	Kube   *kube.Client
	Orch   *orchestrator.Orchestrator
	Broker *events.Broker
	// BaseCtx is the lifetime of background operations (cancelled on shutdown).
	BaseCtx        context.Context
	AppURLTemplate string
	// DefaultStrategy is used when a create request does not say ("" = inplace). New apps can opt in
	// to the release strategy with {"strategy":"release"}; existing apps keep their strategy.
	DefaultStrategy string
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	m.HandleFunc("POST /api/v1/apps", s.createApp)
	m.HandleFunc("GET /api/v1/apps", s.listApps)
	m.HandleFunc("GET /api/v1/apps/{id}", s.getApp)
	m.HandleFunc("DELETE /api/v1/apps/{id}", s.deleteApp)
	m.HandleFunc("POST /api/v1/apps/{id}/changes", s.changeApp)
	m.HandleFunc("POST /api/v1/apps/{id}/retry", s.retryApp)
	m.HandleFunc("POST /api/v1/apps/{id}/approve", s.approveApp)
	m.HandleFunc("POST /api/v1/apps/{id}/discard", s.discardApp)
	m.HandleFunc("POST /api/v1/apps/{id}/rollback", s.rollbackApp)
	m.HandleFunc("GET /api/v1/apps/{id}/releases", s.listReleases)
	m.HandleFunc("GET /api/v1/apps/{id}/operations", s.listOps)
	m.HandleFunc("GET /api/v1/apps/{id}/events", s.sse)
	m.HandleFunc("POST /internal/v1/operations/{op}/events", s.agentEvent)
	return m
}

type errBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, errBody{fmt.Sprintf(format, a...)})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 24 {
		s = strings.Trim(s[:24], "-")
	}
	if len(s) < 2 || s[0] < 'a' || s[0] > 'z' {
		s = "app-" + newID()[:6]
	}
	return s
}

type appView struct {
	store.App
	URL            string           `json:"url,omitempty"`
	PreviewURL     string           `json:"previewUrl,omitempty"`     // release strategy: the not-yet-approved version
	CanRestoreData bool             `json:"canRestoreData,omitempty"` // a rollback may also restore the data
	Actual         *kube.Actual     `json:"actual,omitempty"`
	Operation      *store.Operation `json:"operation,omitempty"` // active or most recent
}

func (s *Server) view(ctx context.Context, a store.App, withActual bool) appView {
	v := appView{App: a}
	if a.Phase == store.PhaseReady && s.AppURLTemplate != "" {
		v.URL = strings.ReplaceAll(s.AppURLTemplate, "{id}", a.ID)
	}
	if a.Strategy == store.StrategyRelease && a.DraftState == store.DraftPreviewReady && s.AppURLTemplate != "" {
		v.PreviewURL = strings.ReplaceAll(s.AppURLTemplate, "{id}", a.ID+"-preview")
	}
	v.CanRestoreData = s.canRestoreData(ctx, a)
	if withActual {
		if act, err := s.Kube.Actual(ctx, a.ID); err == nil {
			v.Actual = &act
		}
	}
	if op, err := s.St.ActiveOperation(ctx, a.ID); err == nil {
		v.Operation = &op
	} else if ops, err := s.St.ListOperations(ctx, a.ID); err == nil && len(ops) > 0 {
		v.Operation = &ops[0]
	}
	if v.Operation != nil {
		v.Operation.Detail = "" // raw logs are for operators: GET /operations
	}
	return v
}

func decode(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<10)
	return json.NewDecoder(r.Body).Decode(dst)
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var in struct{ ID, Name, Prompt, Strategy string }
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = s.DefaultStrategy
	}
	if strategy == "" {
		strategy = store.StrategyInplace
	}
	if strategy != store.StrategyInplace && strategy != store.StrategyRelease {
		writeErr(w, 400, "unknown strategy %q (inplace | release)", strategy)
		return
	}
	in.Prompt, in.Name = strings.TrimSpace(in.Prompt), strings.TrimSpace(in.Name)
	if in.Prompt == "" {
		writeErr(w, 400, "prompt is required")
		return
	}
	if in.Name == "" {
		in.Name = in.ID
	}
	if in.Name == "" {
		writeErr(w, 400, "name is required")
		return
	}
	id := in.ID
	if id == "" {
		id = slug(in.Name)
	}
	if !kube.ValidAppID(id) {
		writeErr(w, 400, "invalid app id %q (lowercase letters, digits, '-', 2-30 chars)", id)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	ctx := r.Context()

	// Retried request with the same key: return the original result.
	if key != "" {
		if op, err := s.St.OperationByKey(ctx, id, key); err == nil {
			a, _ := s.St.GetApp(ctx, id)
			writeJSON(w, 200, map[string]any{"app": s.view(ctx, a, false), "operation": op, "duplicate": true})
			return
		}
	}

	app, err := s.St.CreateAppWithStrategy(ctx, id, in.Name, strategy)
	if errors.Is(err, store.ErrConflict) {
		existing, _ := s.St.GetApp(ctx, id)
		if existing.Phase != store.PhaseDeleted {
			writeErr(w, 409, "app %q already exists", id)
			return
		}
		if err := s.St.RecreateApp(ctx, id, in.Name, strategy); err != nil {
			writeErr(w, 500, "%v", err)
			return
		}
		app, _ = s.St.GetApp(ctx, id)
	} else if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	op, _, err := s.St.CreateOperation(ctx, store.Operation{ID: newID(), AppID: id, Kind: store.KindCreate, Prompt: in.Prompt, IdempotencyKey: key})
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	s.Orch.Emit(s.BaseCtx, id, op.ID, store.PhaseQueued, "依頼を受け付けました")
	s.Orch.Submit(s.BaseCtx, op)
	writeJSON(w, 202, map[string]any{"app": s.view(ctx, app, false), "operation": op})
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.St.ListApps(r.Context())
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	out := make([]appView, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.view(r.Context(), a, false))
	}
	writeJSON(w, 200, map[string]any{"apps": out})
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	a, err := s.St.GetApp(r.Context(), r.PathValue("id"))
	if err != nil || a.Phase == store.PhaseDeleted {
		writeErr(w, 404, "app not found")
		return
	}
	writeJSON(w, 200, s.view(r.Context(), a, true))
}

func (s *Server) changeApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var in struct{ Prompt string }
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Prompt) == "" {
		writeErr(w, 400, "prompt is required")
		return
	}
	a, err := s.St.GetApp(ctx, id)
	if err != nil || a.Phase == store.PhaseDeleted {
		writeErr(w, 404, "app not found")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		if op, err := s.St.OperationByKey(ctx, id, key); err == nil {
			writeJSON(w, 200, map[string]any{"operation": op, "duplicate": true})
			return
		}
	}
	if _, err := s.St.ActiveOperation(ctx, id); err == nil {
		writeErr(w, 409, "別の作業が進行中です。終わるまでお待ちください")
		return
	}
	if a.Phase != store.PhaseReady && !(a.Strategy == store.StrategyRelease && a.Phase == store.PhasePreview) {
		writeErr(w, 409, "app is %s; it can only be changed when READY", a.Phase)
		return
	}
	op, created, err := s.St.CreateOperation(ctx, store.Operation{ID: newID(), AppID: id, Kind: store.KindModify, Prompt: strings.TrimSpace(in.Prompt), IdempotencyKey: key})
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	if created {
		s.Orch.Emit(s.BaseCtx, id, op.ID, store.PhaseQueued, "変更の依頼を受け付けました")
		s.Orch.Submit(s.BaseCtx, op)
	}
	writeJSON(w, 202, map[string]any{"operation": op})
}

// retryApp starts a failed create over, from an empty source, optionally with a reworded request.
func (s *Server) retryApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	a, err := s.St.GetApp(ctx, id)
	if err != nil || a.Phase == store.PhaseDeleted {
		writeErr(w, 404, "app not found")
		return
	}
	if a.Phase != store.PhaseFailed {
		writeErr(w, 409, "app is %s; only a failed app can be retried", a.Phase)
		return
	}
	if _, err := s.St.ActiveOperation(ctx, id); err == nil {
		writeErr(w, 409, "別の作業が進行中です")
		return
	}
	var in struct{ Prompt string }
	_ = decode(r, &in) // body is optional
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		ops, err := s.St.ListOperations(ctx, id)
		if err != nil {
			writeErr(w, 500, "%v", err)
			return
		}
		for _, o := range ops { // newest first
			if o.Kind == store.KindCreate {
				prompt = o.Prompt
				break
			}
		}
	}
	if prompt == "" {
		writeErr(w, 409, "no original request to retry")
		return
	}
	op, created, err := s.St.CreateOperation(ctx, store.Operation{ID: newID(), AppID: id, Kind: store.KindCreate, Prompt: prompt, Purge: true, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	if created {
		s.Orch.Emit(s.BaseCtx, id, op.ID, store.PhaseQueued, "もう一度作ります")
		s.Orch.Submit(s.BaseCtx, op)
	}
	writeJSON(w, 202, map[string]any{"operation": op})
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.St.GetApp(ctx, id); err != nil {
		writeErr(w, 404, "app not found")
		return
	}
	if _, err := s.St.ActiveOperation(ctx, id); err == nil {
		writeErr(w, 409, "別の作業が進行中です")
		return
	}
	purge, _ := strconv.ParseBool(r.URL.Query().Get("purge"))
	op, _, err := s.St.CreateOperation(ctx, store.Operation{ID: newID(), AppID: id, Kind: store.KindDelete, Prompt: "delete", Purge: purge, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	s.Orch.Submit(s.BaseCtx, op)
	writeJSON(w, 202, map[string]any{"operation": op})
}

func (s *Server) listOps(w http.ResponseWriter, r *http.Request) {
	ops, err := s.St.ListOperations(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"operations": ops})
}

// agentEvent receives progress from an Agent Job. It is advisory only: it can
// move the visible phase to GENERATING/TESTING but never completes anything.
func (s *Server) agentEvent(w http.ResponseWriter, r *http.Request) {
	opID := r.PathValue("op")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.Orch.ValidToken(opID, tok) {
		writeErr(w, 401, "unauthorized")
		return
	}
	op, err := s.St.GetOperation(r.Context(), opID)
	if err != nil || op.State != store.OpRunning || (op.Kind != store.KindCreate && op.Kind != store.KindModify) {
		writeErr(w, 409, "operation is not accepting progress")
		return
	}
	var in struct{ Phase, Message string }
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	phase := ""
	if in.Phase == store.PhaseGenerating || in.Phase == store.PhaseTesting {
		phase = in.Phase
	}
	msg := in.Message
	if len(msg) > 500 {
		msg = msg[:500]
	}
	s.Orch.Emit(r.Context(), op.AppID, op.ID, phase, msg)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.St.GetApp(r.Context(), id); err != nil {
		writeErr(w, 404, "app not found")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	last, _ := strconv.ParseInt(firstNonEmpty(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe before replay so no event falls into the gap; dedupe by id.
	ch, cancel := s.Broker.Subscribe(id)
	defer cancel()
	send := func(e store.Event) {
		if e.ID <= last {
			return
		}
		last = e.ID
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "id: %d\nevent: progress\ndata: %s\n\n", e.ID, b)
		fl.Flush()
	}
	missed, _ := s.St.EventsAfter(r.Context(), id, last)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	for _, e := range missed {
		send(e)
	}
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			send(e)
		case <-hb.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// releaseApp loads an app for a release-strategy action and refuses anything else.
func (s *Server) releaseApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	a, err := s.St.GetApp(r.Context(), r.PathValue("id"))
	if err != nil || a.Phase == store.PhaseDeleted {
		writeErr(w, 404, "app not found")
		return a, false
	}
	if a.Strategy != store.StrategyRelease {
		writeErr(w, 409, "this app uses the %q strategy; releases do not apply", a.Strategy)
		return a, false
	}
	if _, err := s.St.ActiveOperation(r.Context(), a.ID); err == nil {
		writeErr(w, 409, "別の作業が進行中です。終わるまでお待ちください")
		return a, false
	}
	return a, true
}

// startOp records and starts a release-strategy operation.
func (s *Server) startOp(w http.ResponseWriter, r *http.Request, a store.App, kind, param, queuedMsg string) {
	op, created, err := s.St.CreateOperation(r.Context(), store.Operation{ID: newID(), AppID: a.ID, Kind: kind, Prompt: param, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	if created {
		s.Orch.Emit(s.BaseCtx, a.ID, op.ID, store.PhaseQueued, queuedMsg)
		s.Orch.Submit(s.BaseCtx, op)
	}
	writeJSON(w, 202, map[string]any{"operation": op})
}

// approveApp turns the previewed draft into the next immutable release and makes it live.
func (s *Server) approveApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.releaseApp(w, r)
	if !ok {
		return
	}
	if a.DraftState != store.DraftPreviewReady {
		writeErr(w, 409, "承認できるお試し版がありません(状態: %q)", a.DraftState)
		return
	}
	latest, err := s.St.LatestReleaseNumber(r.Context(), a.ID)
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	s.startOp(w, r, a, store.KindApprove, strconv.Itoa(latest+1), "承認を受け付けました")
}

// discardApp throws the draft and its preview away (back to the live release).
func (s *Server) discardApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.releaseApp(w, r)
	if !ok {
		return
	}
	if a.LiveRelease == 0 {
		writeErr(w, 409, "本番に反映された版がまだないので、破棄ではなく削除してください")
		return
	}
	s.startOp(w, r, a, store.KindDiscard, "", "お試し版の破棄を受け付けました")
}

// rollbackApp puts production back on the previous release; {"withData":true} also restores the
// data snapshot taken just before the current release went live.
func (s *Server) rollbackApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.releaseApp(w, r)
	if !ok {
		return
	}
	if a.PrevRelease < 1 {
		writeErr(w, 409, "戻せる前の版がありません(現在の版: %d)", a.LiveRelease)
		return
	}
	var in struct {
		WithData bool `json:"withData"`
	}
	_ = decode(r, &in)
	if in.WithData && !s.canRestoreData(r.Context(), a) {
		writeErr(w, 409, "この戻し方では、データは戻せません(戻す先の版に合うデータの保存がありません)")
		return
	}
	param := "withData=0"
	if in.WithData {
		param = "withData=1"
	}
	s.startOp(w, r, a, store.KindRollback, param, "前の版に戻す依頼を受け付けました")
}

// canRestoreData: the data snapshot of the live release belongs to the release it replaced, which
// is only the rollback target right after that approval.
func (s *Server) canRestoreData(ctx context.Context, a store.App) bool {
	if a.Strategy != store.StrategyRelease || a.LiveRelease < 1 || a.PrevRelease < 1 {
		return false
	}
	cur, err := s.St.GetRelease(ctx, a.ID, a.LiveRelease)
	return err == nil && cur.DataSnapshot && cur.Replaced == a.PrevRelease
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	a, err := s.St.GetApp(r.Context(), r.PathValue("id"))
	if err != nil || a.Phase == store.PhaseDeleted {
		writeErr(w, 404, "app not found")
		return
	}
	rel, err := s.St.ListReleases(r.Context(), a.ID)
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"liveRelease": a.LiveRelease, "releases": rel})
}
