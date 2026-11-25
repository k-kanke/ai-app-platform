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

## 2026-11-25 Phase 1 + Phase 2 を実クラスタ(自宅)に反映

**やったこと**
- `kubernetes-platform`(`master`、コミット `ed2991c`): Control Plane に `AAP_TASKS_ENABLED=true` と
  `AAP_TASKS_NAMESPACE=aap-agents` を追加。`control-plane-ingress` に genkan(`app=genkan`)からの許可を追加。
- `genkan`(コミット `fde4369` と、`conf` の emptyDir を足した修正): 「タスク」タブ(依頼フォーム、履歴、
  詳細、差分の表示、承認・却下)。nginx が `/cp/v1/tasks` だけを Control Plane の `/api/v1/tasks` へ中継する
  (`internal` 系と他の API は 404、GET / POST 以外は 403)。
- nginx の設定は、公式イメージのテンプレート機能(`/etc/nginx/templates/`)で生成する。リゾルバは起動時に
  `/etc/resolv.conf` から埋まるので、Control Plane の Service を起動時に引けなくても genkan は落ちない。

**詰まったこと**
- 波括弧を含む正規表現(`{1,32}`)を引用符で囲まないと、nginx が起動しなかった。ローカルで本番と同じイメージを動かして気づいた。
- genkan は `readOnlyRootFilesystem` で動くため、起動時の `default.conf` の生成に失敗して CrashLoop になった。
  `/etc/nginx/conf.d` に emptyDir を足して解消。古い Pod が動き続けたので、公開は止まらなかった。
- `finishedAt` が未完了のときも `0001-01-01…` と出ていた。`omitzero` に変更。

**確認(実機)**
- ✅ genkan の中から `/cp/v1/tasks` が使える。`/cp/internal/…` と `/cp/v1/apps` は 404。
- ✅ 実際にタスクを作成: `aap-agents` に PVC(`local-path`、worker-02 に束縛)と Job ができ、clone まで進んだ
  (GitHub への HTTPS、init container、`restricted` の PodSecurity、Control Plane への進捗の通信が通った)。
- ✅ Gemini の API キーが無いため、エージェントの手前で失敗し、「AIを使うための鍵に問題があります」と説明された(想定どおり)。

**🖐 あなたの手作業が必要(未実施)**
- Gemini の API キーの Secret を `aap-agents` にも作る。自動モードの分類器に、Secret の複製を拒否されたため、
  私は実行していない(キーの扱いは、あなたの明示的な許可が要る操作として扱った)。実行するコマンド(ubuntu-26 で):
  ```sh
  kubectl -n aap-apps get secret agent-credentials -o json \
    | python3 -c 'import sys,json; s=json.load(sys.stdin); print(json.dumps({"apiVersion":"v1","kind":"Secret","metadata":{"name":"agent-credentials","namespace":"aap-agents"},"type":s.get("type","Opaque"),"data":s["data"]}))' \
    | kubectl apply -f -
  ```
  これは Phase 3(LLM ゲートウェイ)までの暫定措置。ゲートウェイができたら、このコピーは削除する。

## 2026-11-25 Phase 4: Publisher と GitHub 連携(コードとテスト。実機は GitHub App 待ち)

**やったこと**(`ai-app-platform`)
- `internal/github`: GitHub App の JWT(RS256、9 分)から、**1 リポジトリ限定・Contents と Pull requests の書き込みだけ**の
  installation token を発行する。インストールはリポジトリから引く(ID を持たない)。PR の作成(**通常の PR**、draft にしない)と、
  既存の PR の検索。
- `internal/publisher` と `cmd/publisher`: Publisher Job の本体(LLM なし、固定コード)。承認された patch を Control Plane から
  取得 → **sha256 を承認時のハッシュと照合** → 方針の検査(保護パス、バイナリ、実行権限の変更、シンボリックリンクは**拒否**)→
  **新しい clone** に `git apply --index` → 変更されたファイルが承認済みの一覧と**完全に一致**することを確認 → `agent/<task>` に commit →
  push(`main` へは push しない、force しない)→ PR を作成 → Control Plane に報告。作業場の `.git` は使わない(設計メモ §5.3)。
  トークンは HTTP ヘッダでだけ渡し(リモート URL や設定に書かない)、エラー文からも除く。リトライは自分のブランチと PR を再利用する。
- `kube/publisher.go`: Publisher の Job(`aap.dev/role=publisher`、作業場なし、SA トークンなし、読み取り専用ルート)。
- Orchestrator: 承認 → `APPROVED → PUBLISHING`(compare-and-set で二重起動しない)→ トークン発行 → Job → 完了確認。
  再起動後は `PUBLISHING` のタスクを再開する。GitHub App が未設定なら `APPROVED` のまま(PR は作らない)。
- API: Publisher 専用の内部エンドポイント(承認済み patch の取得、PR の報告)。**Implementer とは別のトークン**。
  報告された PR の URL は、そのタスクのリポジトリの PR でなければ拒否する。
- `diagnose.ClassifyPublish`、`Dockerfile.publisher`(git が要るので alpine)、CI にイメージを追加。
- `docs/multi-agent-github-app.md`: GitHub App の作り方(あなたの手作業)。

**判断**
- 保護パスの変更(`.github/`、ロックファイル、`Dockerfile*`、`deploy/`、`k8s/`、CODEOWNERS)は、承認画面では警告にとどめ、
  **Publisher では既定で拒否**にした(設計メモ §5.3 どおり)。依存を足す変更はロックファイルを触るので拒否される。
  緩めたくなったら、許可リストを足す(未決事項)。
- Publisher のトークンは、Job の環境変数に直接入れた(Secret を作る権限を Control Plane に足さないため)。トークンは 1 時間で
  切れ、Job は保持期間で消える。Secret にする案は、Phase 5 の Admission Policy と一緒に再検討する。
- 承認画面の警告より、Publisher の拒否のほうが厳しい。人が承認しても、拒否されると `FAILED` と理由が出る。

**確認**
- ✅ `go test -race ./...` がすべて通る。`publisher` のテストは**本物の git** とローカルのベアリポジトリ(偽の GitHub API と
  偽の Control Plane)で動く: 通常の PR と専用ブランチができる、`main` は変わらない、ハッシュ不一致の拒否、
  6 種類の危険な変更(ワークフロー、ロックファイル、Dockerfile、シンボリックリンク、実行権限、バイナリ)の拒否で
  何も push されず PR も作られない、`main` が先に進んだときの明確なエラー、リトライで PR が重複しない、トークンがエラーに出ない。
- ✅ API のテスト: 承認で Publisher が起動し、リポジトリ限定のトークンが 1 回だけ発行される、Implementer のトークンでは
  公開用の patch を読めない、Implementer の Job に GitHub の認証情報がない、不正な PR の URL・ブランチの拒否、
  失敗の説明、二重承認で Publisher が 1 つだけ、再起動後の再開、App 未設定なら `APPROVED` のまま。
- ⚠ 実際の GitHub への push と PR は未確認(GitHub App がまだないため)。`Dockerfile.publisher` は、CI でのビルド結果で確認する。

**🖐 あなたの手作業が必要(未実施)**
- GitHub App の作成、リポジトリへのインストール、`main` のブランチ保護、Secret の作成。手順は
  [`multi-agent-github-app.md`](multi-agent-github-app.md)。

