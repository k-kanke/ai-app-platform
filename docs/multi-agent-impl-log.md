# マルチエージェント基盤: 実装ログ

[`multi-agent-execution-plan.md`](multi-agent-execution-plan.md) に沿った実装の記録。
やったこと、判断、確認の結果、コミットを、作業ごとに追記する。**あとで見返すための記録**であり、設計は
[`multi-agent-permissions.md`](multi-agent-permissions.md) が正。

- 実装の方針(2026-11-25 にユーザーが指示): 手作業が必要な箇所(GitHub App の作成など)以外は、確認を挟まず進める。
  承認はなるべく求めない。
- 凡例: ✅ 確認できた / ⚠ 未確認・保留 / 🖐 ユーザーの手作業が必要

---

## 2026-11-25 Phase 0: 下準備

**やったこと**(リポジトリ `kubernetes-platform`、ブランチ `master`、コミット `8865c7a`)
- `aap-agents` namespace を追加(PodSecurity `restricted` を enforce / audit / warn)。
- NetworkPolicy: `default-deny` と `implementer-egress`(DNS、Control Plane の 8080、HTTPS。RFC1918 などは除外)。
  Control Plane 側の `control-plane-ingress` に、`aap-agents` の `aap.dev/role=implementer` からの許可を追加。
- RBAC: Control Plane の ServiceAccount に、`aap-agents` の PVC / Job / Pod / Pod ログを扱う Role を付与(**Secret は付けない**)。
- `ResourceQuota`(Pod 6、CPU 要求 3、メモリ要求 4Gi、PVC 8、容量 16Gi、Job 12)と `LimitRange`(PVC は 1 つ 4Gi まで)。

**判断**
- Argo CD は `master` を `automated` + `prune` で同期するので、push すると実クラスタに反映される。
  変更は新しい namespace と許可の追加だけで、既存のものは書き換えていない。
- `kubernetes-platform` のデフォルトブランチは `main` ではなく `master`。

**確認**
- ✅ `kubectl kustomize` が通り、実クラスタの API でサーバー側 dry-run が通った(namespace 未作成による `NotFound` だけ想定どおり)。
- ✅ Argo CD で Synced。`aap-agents` に、NetworkPolicy 2、ResourceQuota、LimitRange、Role、RoleBinding が作られた。
- ✅ `aap-agents` で特権 Pod と root の Pod を作ろうとすると、PodSecurity `restricted` に拒否された。
- ✅ Control Plane の ServiceAccount は、`aap-agents` で Job の作成は可能、Secret の作成は不可。

## 2026-11-25 Phase 1 + Phase 2(サーバー側): 作業場 + Implementer、patch と承認 ①

**やったこと**(リポジトリ `ai-app-platform`)
- `control-plane/internal/patch/`: `git diff --binary` を解析して、変更ファイル・行数・危険なパス(`.git/`、`.github/`、
  ロックファイル、`Dockerfile*`、`deploy/`、`k8s/`、CODEOWNERS、シンボリックリンク)を検出する。`.git` の中や
  リポジトリの外を指すパスは `Blocked`(公開不可)にする。
- `store/tasks.go`: `tasks` / `task_patches` テーブル。状態 `PENDING → RUNNING → AWAITING_APPROVAL → APPROVED …`。
  承認は**パッチの sha256 が一致したときだけ**成功する(compare-and-set)。承認後は patch を差し替えられない。
- `kube/task.go`: タスクごとの PVC(`local-path`、2Gi)と Implementer Job。init container で `git clone`、
  main でエージェント。SA トークンなし、Secret / hostPath の volume なし、非 root、`readOnlyRootFilesystem`、
  `/tmp` は emptyDir。ラベル `aap.dev/role=implementer`(NetworkPolicy の対象)。
- `orchestrator/task_flow.go`: 手順(作業場 → Job → 待機 → patch の確認 → 承認待ち)。冪等で、再起動後に再開する。
  保持期間(既定 24 時間)を過ぎたタスクの作業場と patch を削除して `EXPIRED` にする(10 分ごと)。
- `api/tasks.go`: `POST/GET /api/v1/tasks`、`/patch`、`/approve`、`/reject`、`/events`(SSE)。
  Implementer 用に `POST /internal/v1/tasks/{id}/events` と `/patch`(**タスク専用トークン**。サイズ上限 1 MiB)。
  機能は `AAP_TASKS_ENABLED=true` のときだけ有効(既定は無効)。
- `agent-runtime/`: `task-entrypoint.sh`(エージェント実行 → `git diff --cached --binary` → アップロード)、
  `agents/task-gemini.sh`、LLM を使わないスタブ `agents/task-template.sh`、`AGENTS.task.md`
  (指示は `$HOME/.gemini/GEMINI.md` に置き、リポジトリを汚さない)。
- `diagnose.ClassifyTask`: clone の失敗やテストの失敗など、タスク向けの失敗の説明。
- `hack/e2e-tasks-kind.sh`: kind での通しテスト。

**判断**
- イベントは、既存の `events` テーブルと SSE を `task:<id>` のキーで流用した(新しい仕組みを作らない)。
- 空の差分は `NO_CHANGES`。patch が `Blocked` のときは、承認待ちにせず `FAILED` にする。
- タスクの同時実行は 2 まで(`AAP_TASK_MAX_ACTIVE`)。超えると 429(同じ `Idempotency-Key` の再送は除く)。
- 既存の `gemini.sh`(家族向けアプリ)には触れず、`task-gemini.sh` に重複を許した。家族向けの流れを壊さないため。
- 作業場の `.git` を使わない設計(設計メモ §5.3)は、patch だけを Publisher に渡す形で Phase 4 に持ち越し。

**確認**
- ✅ `go test -race ./...` がすべて通る。新しいテスト: `patch` 3、`store` 4、`diagnose` 1、`api`(タスク)10。
  内容: 承認がハッシュに縛られる、承認後の差し替え拒否、トークン検証、サイズ上限、冪等、容量、再起動後の再開、
  期限切れの削除、却下、機能が既定で無効。
- ✅ kind(`aap-dev`)で `hack/e2e-tasks-kind.sh` が通った: GitHub から `k-kanke/genkan` を実際に clone →
  スタブのエージェントが README を変更 → patch がアップロードされ `AWAITING_APPROVAL` → 誤ったハッシュは 409、
  正しいハッシュで `APPROVED`。`NO_CHANGE` は `NO_CHANGES`、`FAIL_TASK` は `FAILED` と説明つき。
  Job の SA トークンなし・Secret volume なし・読み取り専用ルート、PVC の存在、完了後も作業場が残ることも確認。
  `restricted` の PodSecurity の下で警告なしに動いた。
- ⚠ 既存の `hack/e2e-kind.sh`(家族向けアプリ)は再実行していない。`golang:1.26-alpine` の pull が止まるため
  (`dev-log` の既知の現象)。変更は `JobStatus` の分割と SSE の関数の切り出しだけで、既存の Go テストは通っている。
- ⚠ 実際の LLM(Gemini)での実行は未確認。API キーの扱いは Phase 3 で変える。

**詰まったこと**
- kind の通しテストが、`golang:1.26-alpine` の pull で 10 分以上止まった。スクリプトに、ベースイメージが
  ローカルにないときは**ホストで Go をクロスコンパイルして、ローカルにある alpine に載せる**経路を足した。
- git の実際のメッセージ(`repository '…' not found`)に、最初の正規表現が合っていなかった。テストで見つけて直した。

