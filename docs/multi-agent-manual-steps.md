# あなたの手作業チェックリスト

自動で進められない(鍵や、GitHub 側の設定が要る)作業の一覧。**上から順に、必要なものだけ**やればよい。
進み具合は [`multi-agent-impl-log.md`](multi-agent-impl-log.md)。

| # | 作業 | これをやるとできること | やらないと |
|---|---|---|---|
| 1 | LLM ゲートウェイに API キーを入れる | Implementer(エージェント)が動く | タスクが「API キーが設定されていません」で失敗する |
| 2 | GitHub App を作る | 承認した差分が PR になる | 承認したタスクは `承認済み` で止まる(PR は作られない) |

> 先に **1** をやると、「依頼 → 差分ができる → 承認」まで動く。**2** をやると、その先の PR まで動く。

---

## 1. LLM ゲートウェイに API キーを入れる

Gemini の API キーは、**ゲートウェイだけ**が持つ。エージェントの Pod には、タスク専用の短命なトークンだけが渡る。
すでに家族向けアプリ用の Secret(`aap-apps/agent-credentials`)にキーがあるので、それをゲートウェイ用の Secret に**クラスタの中でコピー**する
(キーは画面に出ない)。ubuntu-26 で:

```sh
kubectl -n aap-apps get secret agent-credentials -o json \
  | python3 -c 'import sys,json; d=json.load(sys.stdin)["data"]["GEMINI_API_KEY"]; print(json.dumps({"apiVersion":"v1","kind":"Secret","metadata":{"name":"llm-gateway-credentials","namespace":"platform-system"},"data":{"GEMINI_API_KEY":d}}))' \
  | kubectl apply -f -
kubectl -n platform-system rollout restart deploy/llm-gateway
```

確かめる:
```sh
kubectl -n platform-system logs deploy/llm-gateway | tail -3     # "no provider key configured" が出ていなければ OK
```
genkan の「タスク」から依頼して、差分ができれば成功。

- 上限: 1 タスクあたり 400 リクエスト、1 日あたり 3000 リクエスト(ゲートウェイの環境変数 `LLM_MAX_REQUESTS_PER_TASK` / `LLM_MAX_REQUESTS_PER_DAY`)。
- Google 側の予算アラート(AI Studio / Cloud)も、必ず設定する(`AGENTS.md` の費用の節)。
- 以前の記録にあった「`aap-agents` に `agent-credentials` をコピーする」手順は**不要になった**(ゲートウェイ方式に置き換わったため。
  そのコピーを作っていた場合は、消してよい: `kubectl -n aap-agents delete secret agent-credentials`)。

## 2. GitHub App を作る

[`multi-agent-github-app.md`](multi-agent-github-app.md) の手順どおり。要点: App の作成(Contents と Pull requests の書き込み)→ `genkan`
にインストール → `main` のブランチ保護 → Secret `platform-system/github-app`(`app-id` と `private-key.pem`)の作成 → Control Plane の再起動。
