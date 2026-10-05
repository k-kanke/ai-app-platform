# Development Log

実装中に起きた問題・判断を記録する(plan-2 §14)。発表の材料にするため、後付けにしない。

## 2026-10-04 - 最初の実装で決めたこと(Stage 1)

### Context
plan-2 の Stage 1(Generic Runtime + Source PVC + Data PVC)を end-to-end で通す最初の実装。

### 決めたこと / 理由
- **Namespace は1つ(`aap-apps`)**。App ごとの Namespace は ResourceQuota 等が必要になってからでよい。ownership は `aap-gen-` prefix + labels(`aap.dev/app-id`)で表す。Control Plane の Role も `aap-apps` に限定できる。
- **Source PVC は `current/` と `snapshots/` を subPath で分け、Agent には `current/` だけ mount**。Agent は snapshot を壊せない。snapshot / restore / 初期化は root の短命な helper Job(busybox)が行う。
- **Agent の完了判定は Job status**。progress event(HTTP POST)は UI 用の参考情報で、フェーズを GENERATING/TESTING に動かすことしかできない。
- **Kubernetes の状態は poll で見る(Watch ではない)**。Control Plane 再起動後にそのまま続きから動けるため。Watch は必要になったら導入する。
- **Operation は Kubernetes を呼ぶ前に SQLite へ保存(write-ahead)**。resource 名は app id / operation id から決定的に作り、`AlreadyExists` は成功扱い。再起動後は未完了 Operation を最初から再実行するだけで済む。
- **delete は PVC を残す**(`?purge=true` のときだけ消す)。家族のデータを誤って消さないため。
- **スタブ Agent(`AAP_AGENT=template`)を用意**。LLM なしでプラットフォームの流れを検証できる。実 Agent(`claude`)は未検証(API キー未設定)。

## 2026-10-04 - helper image の初回 pull が 7 分止まった

### Context
kind 上で最初の e2e。`create` の最初の helper Job(busybox:1.37)。

### Problem
Job の所要時間が 7m44s。イベント上は `Pulling` のまま長時間止まり、`Pulled ... in 4.6s` と出たのはその後。2 つめの App では既存 image で即完了。

### Why
registry への初回アクセスの停滞(ネットワーク由来)。Control Plane のバグではない。

### Design Question
- helper image を Control Plane の image に同梱するか、事前に pull / ミラーしておくべきか。自宅サーバーでは外部 registry の遅延が「アプリ作成が終わらない」に直結する。
- helper の timeout(10分)ぎりぎりだった。pull 待ちを失敗扱いにするかどうか。

## 2026-10-04 - "壊す変更" が runtime ではなく Agent のテストで止まった

### Context
e2e シナリオ5(`BREAK_APP`: スタブ Agent が `server.js` を壊す)。

### Actual State
Agent Job は `npm test` で失敗 → Operation FAILED → restore Job が走り、元のソースに戻った。App は READY のまま、データも無傷。

### Design Question
想定していた「テストを通るが起動しない」ケース(runtime が Ready にならず restore する経路)は、このシナリオでは通っていない。ユニットテスト(fake)では確認済みだが、実クラスタでは未確認。テストを持たない App での確認を追加する。

## 2026-10-05 - 公開設定の前に作ったアプリに Ingress がなかった

### Context
Cloudflare Tunnel で公開するため、アプリごとの Ingress(`{id}.kanke-aap-gen.com`)を Control Plane が作る設定を追加した。Ingress の作成は `EnsureRuntime`(create / modify のとき)だけで行っていた。

### Problem
設定追加の前に作ったアプリ(`app-2ef57e`)は、一度変更するまで Ingress が作られず、Portal の「開く」の先が存在しない。

### Why
Ingress を「作成時の副作用」としてしか扱っておらず、「あるべき状態」として継続的に確認していなかった。設定変更や Ingress の手動削除でも同じ状態のずれが起こる。

### Temporary Fix
`ReconcileIngresses`: 起動時と5分おきに、READY のアプリ(進行中の操作があるものは除く)の Ingress を確認し、なければ作る。作成のみで、既存の Ingress は更新しない。

### Design Question
これは小さな「望ましい状態へ収束させるループ」で、plan-2 §12 の Reconciliation の最初の実例。現状は Ingress だけだが、Runtime / Service の消失(Experiment C)にも同じ形が要りそうか。ループを種類ごとに増やすのか、一つの Reconciler にまとめるのかが次の論点。

## 2026-10-05 - バックアップの初回が AccessDenied

### Context
restic で worker の `/opt/local-path-provisioner` を Cloudflare R2 に毎晩バックアップする CronJob を追加。初回を手動実行した。

### Problem
`Stat(<config/>) failed: Access Denied`。R2 のキー(32 桁 / 64 桁の 16 進数)の形式は正しかった。

### Why
manifest のバケット名を、案内に書いた名前(`kanke-aap-backup`)にしていたが、実際に作られたバケットは `kanke-aap-gen-backup`。存在しないバケットや権限のないバケットは、R2 ではどちらも `403 AccessDenied` になり、原因が「キー」か「名前」か区別できなかった。Secret の値は見ずに、形式の確認と SigV4 での `list` の HTTP ステータス(実在する名前だと 200)で切り分けた。

### Result
名前を直して初回成功(`restic check` も通過)。復元の練習(最新スナップショットを使い捨ての領域に戻す Job)も成功し、Control Plane の SQLite(`-wal` / `-shm` 含む)、各アプリの Source / Data、`meals.json` が戻ることを確認した。

### Design Question
外部サービスの名前(バケット名など)を、ドキュメントの案内と実物で二重に持つと食い違う。manifest の値を確認する手順(`list` で 200 か)を最初のチェックに入れる。

## 未検証・既知の穴(Stage 1)
- 実 LLM Agent(claude.sh)の動作、Agent の egress 制限(NetworkPolicy)の実効性(kind の CNI で未検証)
- 稼働中の App が使っている `current/` を Agent が直接書き換える(Stage 2 の Isolated Workspace で解消予定)。変更中は短時間、生成途中のコードが見えうる
- バックアップは R2 に毎晩取得済み(`docs/backup-restore.md`)。失敗時の自動通知と、SQLite の安全なダンプは未実装
- Control Plane の認証なし(Portal 経由 + NetworkPolicy のみ)。Tailscale 等の前提
