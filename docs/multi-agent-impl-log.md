# マルチエージェント基盤: 実装ログ

[`multi-agent-execution-plan.md`](multi-agent-execution-plan.md) に沿った実装の記録。
やったこと、判断、確認の結果、コミットを、作業ごとに追記する。**あとで見返すための記録**であり、設計は
[`multi-agent-permissions.md`](multi-agent-permissions.md) が正。

- 実装の方針(2026-11-25 にユーザーが指示): 手作業が必要な箇所(GitHub App の作成など)以外は、確認を挟まず進める。
  承認はなるべく求めない。
- 凡例: ✅ 確認できた / ⚠ 未確認・保留 / 🖐 ユーザーの手作業が必要

---

## 現在の状態(2026-11-25 終了時点)

| Phase | 内容 | 実機 | 確認 |
|---|---|---|---|
| 0 | `aap-agents` namespace、PodSecurity、NetworkPolicy、RBAC、クォータ | ✅ 反映済み | 実機で確認 |
| 1 | 作業場(PVC)と Implementer Job、タスク API | ✅ 反映済み | Go のテスト(`-race`)、kind の通しテスト、実機のタスク |
| 2 | patch の回収、ハッシュに縛られた承認、genkan の「タスク」画面 | ✅ 反映済み | 同上、ブラウザ |
| 3 | LLM ゲートウェイ(タスクのトークン、リクエスト数の上限) | ✅ 反映済み(**キーなし**) | 本物の CLI と本物のゲートウェイでローカル確認、実機でキーなしの 503 まで確認 |
| 3 | 出口の FQDN 許可リスト | ❌ **動かず、元に戻した** | 実機で DNS が通らなかった。理由と対処は Phase 3 の節 |
| 4 | Publisher と GitHub 連携(PR 作成) | ✅ 反映済み(**GitHub App なし**) | 本物の git とローカルのベアリポジトリでテスト。実際の GitHub は未確認 |
| 5 | 承認ポリシー(Pod、Job) | ✅ 反映済み | 実機の dry-run、kind、実際のタスク |
| 6 | チャットから依頼(ルールベース) | ✅ 反映済み | 実際のブラウザ(偽の Control Plane) |

**今できること**: genkan のチャットか「タスク」画面から依頼 → エージェントが clone して作業を始める。**キーがないので、今は「API キーが設定されていません」と説明つきで失敗する**。

**🖐 あなたの手作業(順番どおり。手順は [`multi-agent-manual-steps.md`](multi-agent-manual-steps.md))**
1. ゲートウェイに API キーを入れる(ubuntu-26 で 1 コマンド)→ 依頼 → 差分ができ → 承認、まで動く。
2. GitHub App を作る([`multi-agent-github-app.md`](multi-agent-github-app.md))→ 承認した差分が PR になり、チャットに「PR を開いてマージする」ボタンが出る。

**まだ確かめていないこと**
- 実際の Gemini による実装(キーがないため)と、実際の GitHub への push / PR(App がないため)。
- ブラウザでの、却下、やめる、失敗時の表示。
- Publisher の Job が、実機の Pod として動くこと(コードと、本物の git でのテストはある)。

**既知の制限と課題**
- 出口の制限は、「LAN、クラスタ、他の namespace は遮断。公開 HTTPS はどこでも可」まで(FQDN 許可リストは動かなかった)。
- 既存の `TestReleaseStrategyLifecycle` が、まれに落ちる(タスクの変更前から。直していない)。
- 家族向けの `agent-runtime/agents/gemini.sh` にも、`kill` されると `exit 0` になる同じ書き方がある(後段の検査があるので、今は害がない。直すかは別の判断)。
- Publisher は、依存を足す変更(ロックファイル)や、`Dockerfile`、`.github/`、`deploy/` の変更を、既定で拒否する。
- 意図しない混入: あなたの未コミットの変更(`docs/slide-assets/terminal-d1-pending.html`)が、コミット `ecfee12` に入って push された。

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

## 2026-11-25 Phase 5: Admission Policy(実機に反映済み)と、作業場クォータの修正

**やったこと**(`kubernetes-platform`、`master`)
- `aap-agents-admission.yaml`: ValidatingAdmissionPolicy 2 つ。
  - Pod: 役割ラベル(`implementer` / `publisher`)が必須、SA トークンなし、volume は PVC と emptyDir だけ(Secret・ConfigMap・hostPath・projected は不可)、
    host の namespace なし、`secretKeyRef` 禁止、Secret の参照は `agent-credentials` の `envFrom` を **implementer だけ**に許可(Phase 3 でなくす暫定措置)、
    cpu と memory の limits が必須。
  - Job: `activeDeadlineSeconds` が 1〜3600 秒、`backoffLimit: 0`。
- Publisher の egress(`publisher-egress`)、Control Plane の ingress に publisher を追加、GitHub App の Secret(`github-app`、省略可)の mount、
  Control Plane に `AAP_PUBLISHER_IMAGE`。Secret がなければ「GitHub App 未設定」で起動し、承認したタスクは `APPROVED` で止まる。

**確認(実機)**
- ✅ CI がすべて成功(`publisher` のイメージを含む)。Control Plane は新しいイメージで起動した。
- ✅ サーバー側 dry-run で、適合する Pod は通り、役割ラベルなし・未知の役割・SA トークンあり・Secret の volume・`secretKeyRef`・
  別の Secret の `envFrom`・publisher からの `agent-credentials`・`hostNetwork` は拒否された。implementer からの `agent-credentials` は通った。
- ✅ 実際のタスクを流すと、Control Plane が作る Job は、ポリシーを通ってエージェントの段階まで進んだ(Gemini の Secret が無いので、そこで失敗するのは想定どおり)。
- ✅ kind でも、通しテストにポリシーの検査を足して通った(適合する Pod は通る、Secret の volume は拒否、hostNetwork は拒否)。
- ⚠ 「リソース上限なし」の Pod は通る。LimitRange が先にデフォルトの上限を入れるため、実質的に上限は必ず付く。ポリシーは保険として残した。

**見つけた問題と修正: 作業場が PVC のクォータを使い切る**
- kind で通しテストを繰り返すと、新しいタスクが「準備の途中で問題が起きました」で失敗した。原因は `ResourceQuota`(PVC 8 個)で、
  保持期間 24 時間のままだと **1 日に 8 タスクで頭打ち**になる。本番でも同じことが起きる。
- 修正: Implementer が成功した時点で、patch は Control Plane に保存されるので、**作業場をすぐ解放する**
  (Publisher は DB の patch だけを使うため、誰も読まない)。**失敗したタスクの作業場だけ**、デバッグ用に保持期間まで残す。
  クォータ超過のときは、分かりやすい文言を返す。クォータも広げた(PVC 12、容量 24Gi、Job 30)。
- 設計メモ §5.1 と実行計画 §0 に反映した。

**詰まったこと(テスト)**
- `set -o pipefail` の下で `kubectl … | grep -q` は、`grep` が早く終了すると SIGPIPE でパイプ全体が失敗扱いになる。出力を変数に取ってから判定するようにした。
- PVC の削除は非同期(PVC 保護のファイナライザが外れるまで `Terminating`)。「存在しない、または削除中」を解放済みとみなす。

## 2026-11-25 Phase 3: LLM ゲートウェイと、出口の許可リスト(コードとテスト。キーは手作業待ち)

**やったこと**
- `internal/llmgateway` と `cmd/llm-gateway`: API キーを持つ**唯一の場所**。Implementer は、キーの代わりに**タスク専用の短命なトークン**
  (`<task>.<期限>.<hmac>`)を送る。ゲートウェイはトークンを検証し、許可した呼び出しだけ(`generateContent` / `streamGenerateContent` /
  `countTokens` / `embedContent` / `batchEmbedContents`、`gemini-*` のモデルだけ、POST だけ)を通し、**タスクごと(400)と 1 日(3000)の
  リクエスト上限**を守り、本物のキーに差し替えて転送する。Implementer が付けた `key=` クエリや `Authorization` は捨てる。ストリーミングはそのまま通す。
- Gemini CLI が、`GOOGLE_GEMINI_BASE_URL` で向き先を変えられると確認した(実機と同じ 0.62.0 のソースを読んだ。「gateway」という認証方式があり、
  `GEMINI_API_KEY` を `x-goog-api-key` で送る)。Control Plane は Job に、この 2 つの環境変数を渡す。**共有の Secret(`agent-credentials`)は、もう渡さない**。
- 実機の manifest(`kubernetes-platform`): ゲートウェイの Deployment と Service、呼べるのは `aap-agents` の Implementer だけという NetworkPolicy、
  **Cilium の FQDN 許可リスト**(`CiliumNetworkPolicy`): Implementer は Control Plane、ゲートウェイ、`github.com`、npm / Go / PyPI の
  レジストリだけ。Publisher は `github.com` と `api.github.com` だけ。ゲートウェイは `generativelanguage.googleapis.com` だけ。
  Cilium は許可を足し合わせるので、「LAN 以外の HTTPS はどこでも」という旧い NetworkPolicy は**削除**した(`default-deny` は残す)。
- 承認ポリシーを最終形に引き締めた: `aap-agents` の Pod は、**Secret を一切参照できない**(`envFrom` もだめ)。
- 失敗の説明: ゲートウェイにキーがないとき、リクエスト上限を使い切ったとき。

**判断**
- ゲートウェイは 1 レプリカ(上限をメモリで数えるため)。再起動で、1 日の数え直しが起きる。個人の基盤では許容した。
- モデルは `gemini-*` だけを許可し、モデルごとの許可リストにはしなかった。CLI は内部で複数のモデルを使うので、厳しくすると壊れやすい。
  費用はリクエスト数の上限で抑える。
- kind には Cilium がないので、通しテストでは `CiliumNetworkPolicy` を除き、ゲートウェイは止める(本物のキーがないため)。

**確認(ここまで)**
- ✅ `go test -race ./...` がすべて通る。ゲートウェイのテスト: 本物のキーへの差し替え、偽装した `key=` の除去、不正なトークン(なし、署名違い、期限切れ、
  別タスクへの書き換え、本物のキーそのもの)の拒否、許可外の呼び出しとモデルの拒否、タスクごと・日ごとの上限、日付が変わると戻る、
  本体サイズの上限、キー未設定の 503、ストリーミング。Control Plane 側: Implementer のキーが自分のタスクの有効なトークンで、期限が切れ、
  他のタスクには使えず、共有 Secret は mount されない。
- ✅ kind の通しテストが、引き締めた承認ポリシーの下でも通る。
- ⚠ 実機への反映と、FQDN ポリシーの動作は、この後の節に記録する。

**🖐 あなたの手作業が必要(未実施)**
- ゲートウェイに API キーを入れる。手順は [`multi-agent-manual-steps.md`](multi-agent-manual-steps.md) の 1。
  (以前の記録にあった `aap-agents` への Secret のコピーは、**不要になった**。)

## 2026-11-25 Phase 3 の実機反映: 見つかった問題が 2 つ(Cilium の FQDN、Gemini CLI の認証方式)

**反映したもの**: `llm-gateway`(実機で Running、キーなしで 503 を返す状態)、Control Plane の `AAP_LLM_GATEWAY_URL`、
承認ポリシーの最終形(Secret の参照を一切禁止)。Implementer は共有の Secret ではなく、タスク専用のトークンを受け取る。

**問題 1: Cilium の FQDN 許可リストが、このクラスタでは動かなかった(元に戻した)**
- 症状: `CiliumNetworkPolicy` の `toFQDNs` を入れると、Pod の DNS 引きが全てタイムアウトした(許可した名前でも)。
  Cilium の DNS プロキシのインスタンスは作られ、ポリシーの拒否の記録も出ていない。
- 原因の見立て(未確認): このクラスタの Cilium は 1.18.2 で `kube-proxy-replacement=false`、`bpf-lb-sock=false`
  (Service の変換は kube-proxy)。kube-dns の Service IP(`10.96.0.10`)宛ての DNS を許可する書き方も足したが、直らなかった。
- 対応: 実機で確認してすぐに戻した(旧い NetworkPolicy に復元し、Implementer からゲートウェイ(8080)への通信を足した)。
  FQDN 版は `kubernetes-platform/manifests/ai-app-platform/experimental/cilium-fqdn-policies.yaml` に、理由と一緒に残した(適用しない)。
- **残る制限**: Implementer と Publisher は、公開インターネットの HTTPS(443)に出られる(LAN、クラスタ、他の namespace は遮断のまま)。
  実機のテスト Pod で確認した: github.com と npm は届く、ゲートウェイと Control Plane は届く、`192.168.0.1`、kube-apiserver、
  portal、monitoring は遮断。**情報の持ち出しは、まだ防げていない**(API キーは持っていないので、盗まれるものは小さい)。
  防ぐには、Cilium の kube-proxy 置き換え(またはソケット LB)を有効にしてから、FQDN 版を再試行する。
- 副産物: FQDN が使えなくても、キーが Pod にない(ゲートウェイ)ことと、リクエスト数の上限は有効。

**問題 2: Gemini CLI が、ゲートウェイの設定だと認証方式エラーで止まった**
- 症状(実機のタスク): `Invalid auth method selected.`。ゲートウェイには何も届かなかった。
- 原因: `GOOGLE_GEMINI_BASE_URL` を設定すると、CLI(0.62.0)は認証方式を `gateway` と自動選択するが、ヘッドレスの検証
  (`validateAuthMethod`)が `gateway` を知らず、弾く。CLI のソースで確認した。
- 修正: `task-entrypoint.sh` が書く `settings.json` に `security.auth.selectedType: "gemini-api-key"` を足した。
  この方式でも、`GEMINI_API_KEY`(ここではタスクのトークン)が `GOOGLE_GEMINI_BASE_URL` へ送られる。
- ローカルで再現して直した。`hack/check-gemini-gateway.sh`(**本物の Gemini CLI** を、Implementer と同じ起動で、偽のゲートウェイに向ける)を
  回帰テストとして残した。`WITH_GATEWAY=1` を付けると、**本物のゲートウェイのバイナリ**を CLI と偽の LLM の間に挟む。

**確認(ローカル、本物の CLI と本物のゲートウェイ)**
- ✅ 認証方式を固定すると、CLI は `POST /v1beta/models/<model>:(stream)generateContent`(`?alt=sse` 付き)を、キーの位置にトークンを入れて送る。
  使うモデルは `gemini-3.1-pro-preview` と `gemini-3.5-flash-lite`(`^gemini-` に合う)。許可していない呼び出しは 1 つもなかった。
- ✅ ゲートウェイを挟むと、偽の LLM に届いた 6 回の呼び出しは、すべて**本物のキー**で、タスクのトークンは漏れなかった。SSE も通った。
- ⚠ モデルの既定は `gemini-3.1-pro-preview`(`AAP_GEMINI_MODEL` を渡さない場合)。実機では Control Plane の `AAP_GEMINI_MODEL=gemini-3.8-flash`
  が渡る。費用は、リクエスト数の上限(1 タスク 400、1 日 3000)と、Google 側の予算アラートで抑える。

**詰まったこと(テスト)**
- CI で、**既存の** `TestReleaseStrategyLifecycle` が落ちた。タスク機能を入れる前のコミットでも、40 回回すと別のアサーションで落ちることを確認した
  (偽の kubelet のタイミングが原因とみられる)。私の変更が原因ではない。タイムアウトを広げても直ったという証拠がなかったので、その変更は戻した。
  失敗したジョブを再実行して通した。**このフレーキーは直していない**(別の課題)。
- `git add -A docs` で、**あなたの未コミットの変更**(`docs/slide-assets/terminal-d1-pending.html` の色分け 6 行)が、私のコミット `ecfee12` に
  混ざって push された。内容は無害だが、意図しない混入だった。履歴は書き換えていない。以降は、パスを明示して `git add` している。

## 2026-11-25 Phase 6: チャットから依頼(ルールベース)と、実機で見つけたバグ

**やったこと**(`genkan`)
- 「指令」タブ(チャット)で、メッセージにリポジトリ名(`k-kanke/genkan` や、名前だけの `genkan`、`ai-app-platform`、`kubernetes-platform`)があれば、
  **本物のタスク**として扱う。URL を含む文や、リポジトリ名がない文は、これまでの会話(モック)のまま。
  LLM は使わない(ルールベース。`src/chat/intent.ts`)。LLM による判断は、ゲートウェイのキーが入ってから、必要なら足す。
- 流れ: ① 「このリポジトリで、この内容で作業を始めてよいですか?」と確認(**1 つ目の確認**)→ 「依頼する」 → ② チャットの中に**タスクカード**:
  作業中(進捗の 1 行つき)→ 承認待ち(変更ファイル、行数、**危険なファイルの警告**、「差分を見る」「却下」「承認」。承認は「この差分を承認しますか?」の
  **2 つ目の確認**)→ PR 作成済み(**「PR #n を開いてマージする」ボタン**。GitHub の PR ページを開き、マージは GitHub 上で行う)。
- 会話の並びは、ブラウザに保存(開き直しても残る)。タスクの状態そのものは Control Plane にある。
- 完了したタスクは、ポーリングをやめる。状態が変わってカードが伸びたら、画面の見える位置へ寄せる。
- `npm test`: 依頼の読み取りのテスト(11 ケース: 名前だけ、`owner/name`、URL は対象外、`genkanai` のような似た名前は対象外 など)。

**確認(実際のブラウザ、スマホ幅)**
- ✅ 偽の Control Plane を後ろに置いたローカルのプレビューで、Chrome を操作して通した:
  入力 → 確認の吹き出しと「依頼する / やめる」→ 依頼 → タスクカード(作業中、進捗の文)→ 承認待ち(2 ファイル、+14/-3、ロックファイルの警告が黄色)→
  承認 → 「この差分を承認しますか?」→ 承認する → 「PR 作成済み」と「PR #7 を開いてマージする」ボタン。
- ⚠ 実機の genkan には反映済みだが、実機で最後(PR)まで通すには、手作業の 1 と 2 が要る(`multi-agent-manual-steps.md`)。
- ⚠ 「却下」「やめる」、エラー表示、タスクカードの失敗時の表示は、ブラウザでは通していない(コードは書いた)。

**実機で見つけたバグ: エージェントが失敗しているのに「変更なし(成功)」になった**
- 症状: ゲートウェイにキーがない状態で、タスクが `NO_CHANGES` で終わった。Pod のログには `status: 503`(「the gateway has no provider key configured」)の
  失敗が並び、最後に `the API keeps failing with 5xx, stopping the agent` と出ていた。
- 原因: `task-gemini.sh` の見張り役が、5xx が続くと CLI を `kill` する。CLI は SIGTERM で **`exit 0`** で終わるので、失敗として伝わらず、
  差分もなく、「変更なし」と報告された。同じ書き方は家族向けの `gemini.sh` にもあるが、そちらは後段の検査(`package.json` の有無など)が失敗を拾うので隠れていた。
- 修正: 見張り役が kill するときに目印(ファイル)を残し、`task-gemini.sh` は、目印があれば **exit 70(失敗)**で終わる。
  家族向けの `gemini.sh` には触っていない(後段の検査があるので、今は害がない。同じ直しを入れるかは別の判断)。
- 回帰テスト: `FAIL_LLM=1 hack/check-gemini-gateway.sh`(偽の LLM が 503 を返し続ける)。**旧いスクリプトでは `== no changes` で失敗し(実機と同じ症状)、
  修正後は通る**ことを確認した。
- 実機: ゲートウェイを通る経路は、この時点で動いている(本物の CLI が、ゲートウェイに届き、トークンの検証、モデルの確認を通って、キーがないので 503 が返る)。

