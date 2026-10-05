# ai-app-platform

家族が自然言語で「欲しいもの」を伝えるだけで、小さなアプリを作り・使い・変更できる、自宅 Kubernetes 上の AI Application Platform。

設計: [`plans/plan-2.md`](plans/plan-2.md)(旧案: `plans/plan.md`) / 開発記録: [`docs/dev-log.md`](docs/dev-log.md)

## 構成

| ディレクトリ | 役割 |
|---|---|
| `control-plane/` | Go。REST + SSE API、SQLite(Platform State)、Kubernetes API 経由で App のライフサイクルを管理 |
| `agent-runtime/` | Agent Job の image。Source Workspace だけを編集する。`AAP_AGENT=template`(スタブ) / `gemini`(Gemini CLI) / `claude`(Claude Code) |
| `app-runtime/` | 生成された Source を実行する汎用 Runtime image(`npm start`) |
| `portal/` | スマホ向け UI(Next.js) |
| `hack/e2e-kind.sh` | 使い捨て kind クラスタでの end-to-end 検証 |

静的な Platform 自体の manifest は `kubernetes-platform/manifests/ai-app-platform/`(Argo CD)。生成アプリのリファレンスは `aap-gen-meal`。

## 開発

```sh
cd control-plane && go test -race ./...
./hack/e2e-kind.sh          # kind-aap-dev を作って Stage 1 の全シナリオを実行(他の context には触れない)
```

## API(v1)

```
POST   /api/v1/apps                 {id?, name, prompt}   → 202  (Idempotency-Key 対応)
GET    /api/v1/apps
GET    /api/v1/apps/{id}            DB の意図 + Kubernetes の実状態
POST   /api/v1/apps/{id}/changes    {prompt}              → 202
DELETE /api/v1/apps/{id}[?purge=true]
GET    /api/v1/apps/{id}/operations
GET    /api/v1/apps/{id}/events     SSE (Last-Event-ID で再開)
```
