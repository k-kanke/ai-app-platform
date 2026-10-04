# 公開経路: Cloudflare Tunnel + Access / 管理経路: Tailscale

| 経路 | 手段 | 対象 |
|---|---|---|
| 家族向け(公開) | Cloudflare Tunnel + **Cloudflare Access** | `portal.kanke-aap-gen.com`、`<app-id>.kanke-aap-gen.com` |
| 管理 | Tailscale / SSH | kubectl、Argo CD、Grafana(`*.home` はそのまま LAN/Tailscale 内) |

```
家族のスマホ ─HTTPS→ Cloudflare(Access で本人確認) ←─ cloudflared(クラスタ内, 外向き接続のみ)
                                                         └→ ingress-nginx ─→ Portal / 各アプリ
```

Portal にも生成アプリにもログイン機能はない。**認証は Cloudflare Access だけ**なので、Access を先に作り、
そのあとでトンネルの経路を有効にする(保護されていない時間を作らない)。

## 手順(Cloudflare ダッシュボード側)

### 1. Access を先に作る
Zero Trust → Access → Applications → Add an application → **Self-hosted**
- Application domain(2つ追加): `portal.kanke-aap-gen.com` と `*.kanke-aap-gen.com`
- Policy: **Allow** / Include → Emails → 家族のメールアドレス
- Login method: One-time PIN(メールに届くコード)
- Session duration: 1 month(母が毎回ログインしなくて済む)

### 2. Tunnel を作る
Zero Trust → Networks → Tunnels → Create → **Cloudflared**、名前は `home`。
表示される **token は、チャットや Git に貼らない**。次の Secret にだけ入れる(ubuntu-26 で):

```sh
kubectl create namespace cloudflared   # Argo が先に作っていれば不要
kubectl -n cloudflared create secret generic cloudflared-token --from-literal=token='<TOKEN>'
```

### 3. 経路(Public Hostname)を2つ追加
どちらも Service は `HTTP` / `ingress-nginx-controller.ingress-nginx.svc.cluster.local:80`
- `portal.kanke-aap-gen.com`
- `*.kanke-aap-gen.com`

ワイルドカードの DNS が自動で作られない場合は、DNS → Records に
`CNAME  *  <TUNNEL_ID>.cfargotunnel.com`(Proxied)を追加する。

## 確認
シークレットウィンドウで `https://portal.kanke-aap-gen.com` を開き、**ログイン(メールコード)画面が出る**こと。

```sh
curl -sI https://portal.kanke-aap-gen.com | head -3     # 302 で cloudflareaccess.com へ
curl -sI https://anything.kanke-aap-gen.com | head -3   # 同じく 302
```

200 が返る場合は Access が効いていない。すぐトンネルの経路を外すこと。

## 注意
- 予約語(`portal` `www` `api` `admin` `argocd` `grafana` 他)はアプリ ID に使えない。
- ホスト名は1階層(`meal.kanke-aap-gen.com`)。2階層は無料の証明書の対象外。
- Control Plane の API は公開しない(Portal 経由のみ)。
- 既存のアプリ(公開設定前に作ったもの)の Ingress は、次にそのアプリを変更したときに作られる。
