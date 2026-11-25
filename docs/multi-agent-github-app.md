# GitHub App の作り方(タスクを PR にするための、手作業の手順)

承認されたタスクを PR にするには、GitHub App が要る。**作るのはあなたの手作業**(秘密鍵は私に渡さない)。
設計の背景は [`multi-agent-permissions.md`](multi-agent-permissions.md)、実装の状況は
[`multi-agent-impl-log.md`](multi-agent-impl-log.md)。

> App がなくても、Control Plane は動く。その場合、承認したタスクは `APPROVED` で止まり、PR は作られない。

## 1. App を作る
GitHub → Settings → Developer settings → GitHub Apps → **New GitHub App**

| 項目 | 設定 |
|---|---|
| GitHub App name | 例: `genkan-agent-<あなたの名前>`(全体で一意) |
| Homepage URL | 何でもよい(例: `https://github.com/k-kanke/ai-app-platform`) |
| Webhook | **Active のチェックを外す**(使わない) |
| Repository permissions | **Contents: Read and write**、**Pull requests: Read and write**(Metadata: Read-only は自動)。**他は付けない** |
| Where can this GitHub App be installed? | **Only on this account** |

作成後に控えるもの:
1. **App ID**(App のページの上のほうに出る数字)
2. **Generate a private key** で `.pem` をダウンロードする(一度しか出ない)

## 2. リポジトリに入れる
App のページ → **Install App** → 自分のアカウント → **Only select repositories** → まず `genkan` だけ。
増やすときは、ここに足す。Control Plane 側にも、許可するリポジトリのパターン(`AAP_TASK_REPO_PATTERN`、既定は `k-kanke/*`)がある。

## 3. main のブランチ保護を有効にする(重要)
App には **Contents の書き込み**を与えるので、保護がないと、技術的には `main` にも書ける。
Publisher は `agent/*` にしか push しないが、**保護で二重に止める**。

各リポジトリの Settings → Branches → Add branch ruleset(または Branch protection rule):
- 対象: `main`(default branch)
- **Require a pull request before merging**
- **Restrict deletions**、**Block force pushes**

## 4. Secret を作る(ubuntu-26 で)
秘密鍵はチャットや Git に貼らない。ファイルのまま、Secret にする。

```sh
kubectl -n platform-system create secret generic github-app \
  --from-literal=app-id=<App ID> \
  --from-file=private-key.pem=./<ダウンロードした>.pem
kubectl -n platform-system rollout restart deploy/control-plane
```

作ったら、手元の `.pem` は消す(Secret に入っている)。

## 5. 確かめる
```sh
kubectl -n platform-system logs deploy/control-plane | grep "github app configured"
```
出ていれば有効。genkan の「タスク」から依頼 → 差分を承認 → 数十秒で PR のリンクが出る。
出ていなければ(`no github app configured` が出る場合)、Secret のキー名(`app-id`、`private-key.pem`)を確認する。

## 取り消す
- すぐに止める: App のページ → Install App → **Uninstall**(トークンは 1 時間で切れ、以後は発行できない)。
- 完全に消す: App のページ → Advanced → **Delete GitHub App**。

## 補足: この App が持つ力
- Publisher Job だけが、**1 リポジトリ限定・1 時間**のトークンを受け取る(Contents と Pull requests の書き込みだけ)。
- Implementer(LLM が動く Pod)には、GitHub の認証情報は一切渡らない。
- 秘密鍵は Control Plane だけが読める(Secret を `/var/run/github-app` に読み取り専用で mount)。
