# バックアップと復元

## 何を、どこへ
- 対象: worker の `/opt/local-path-provisioner` 全体
  (Control Plane の SQLite、各アプリの Source PVC / Data PVC。ディレクトリ名は `pvc-<uid>_<namespace>_<pvc名>`)
- 保存先: Cloudflare R2 の `kanke-aap-gen-backup/restic`(restic で暗号化)
- 実行: 毎日 03:00 JST の CronJob(`backup/restic-backup`)
- 保存期間: 毎日 7 / 毎週 4 / 毎月 6。毎回 `restic check`(構造 + データ 5%)

## 最初の設定(1回だけ)
```sh
# 1) パスワードを作り、必ずパスワードマネージャに保存する。失うとバックアップは二度と開けない。
openssl rand -base64 32

# 2) Secret(ubuntu-26 で。値は Git にもチャットにも出さない)
kubectl create namespace backup   # Argo が先に作っていれば不要
kubectl -n backup create secret generic restic-r2 \
  --from-literal=access-key-id='<R2_ACCESS_KEY_ID>' \
  --from-literal=secret-access-key='<R2_SECRET_ACCESS_KEY>' \
  --from-literal=restic-password='<上で作ったパスワード>'

# 3) 初回を手動で実行して確認
kubectl -n backup create job --from=cronjob/restic-backup first-run
kubectl -n backup logs -f job/first-run      # "backup ok" が出れば成功
```

## 状態の確認
```sh
kubectl -n backup get cronjob,job
kubectl -n backup logs job/<最新のjob>
```
失敗した Job が残っていたら、バックアップが取れていない。ログを見て原因を取り除く。

## 復元の練習(必ず一度やる)
本番に一切書かない。最新のスナップショットを一時領域に戻して中身を表示する。
```sh
kubectl apply -f manifests/backup/restore-drill/restore-job.yaml     # kubernetes-platform リポジトリ
kubectl -n backup logs -f job/restic-restore-drill
kubectl -n backup delete job restic-restore-drill
```
`control-plane.db` と各アプリの `meals.json` などが見えれば、バックアップは使える。

## 実際に復元する(1つのアプリのデータが壊れた場合)
1. 書き込む側を止める(アプリのランタイムを 0 にする):
   `kubectl -n aap-apps scale deploy/aap-gen-<id> --replicas=0`
   (Control Plane が変更中でないことも確認する)
2. 戻す対象のディレクトリ名を確認する(スナップショット内のパス):
   `restic ls latest | grep aap-gen-<id>-data`
3. 復元用 Job を手で作る(`restore-job.yaml` を元に、`/opt/local-path-provisioner` を
   `/data` に **書き込み可で** mount し、`restic restore <snapshot-id> --target / --include /data/<pvc-dir>`)。
   他のアプリのディレクトリを巻き込まないよう、必ず `--include` で絞る。
4. ランタイムを戻す: `kubectl -n aap-apps scale deploy/aap-gen-<id> --replicas=1`
5. アプリを開いて、データが戻ったことを確認する。

SQLite(Control Plane の台帳)を戻すときは、Control Plane を止めてから置き換え、起動後に
`GET /api/v1/apps` で一覧が戻ったことを確認する。

## 既知の限界
- SQLite は稼働中のファイルをコピーしている。書き込みの瞬間に重なると不整合になりうる
  (WAL のおかげでまれ)。改善案: Control Plane に `VACUUM INTO` で安全なダンプを出させる。
- 家の外に置いているのは R2 だけ。R2 のアカウントを失うと、バックアップも失う。
- 失敗を自動で通知する仕組みは未設定(Alertmanager の通知先が未設定のため)。
  当面は、週に一度 `kubectl -n backup get job` を見る。
