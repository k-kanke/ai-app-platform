# 計測ログ

まとめで使う数字の**出典**。数字は必ずここに記録してから使う。

## ルール

1. **追記専用**: 過去の記録は書き換えない。誤りが分かったら、新しい記録で訂正し、元の記録に「→ M-xxx で訂正」と書く。
2. 1 件の記録に必ず入れる: **日時 / 何を測ったか / 方法(コマンド) / 環境 / 結果 / 生データのパス / 注意(限界)**。
3. **n(回数)を書く**。n が小さいものは「ベースライン」「観察」と書き、統計とは呼ばない。
4. 環境が違う値を並べるときは、環境を併記する(例: Docker の Linux VM と、実際のワーカーは別物)。
5. 生データ(JSON など)は `data/` に置き、集計は再現できるコマンドで出す。

## 凡例
- ✅ 測定済み / ⚠ 限界つき(環境・n が小さい) / 🔜 計画中

## 再現コマンド
```sh
# 修正サイクルの遅延(ホームクラスタから取得、読み取りのみ)
hack/latency-report.py --ssh ubuntu-26 --save docs/measurements/data/<名前>.json --label "<条件>"
# 保存済みの生データから再集計
hack/latency-report.py --from docs/measurements/data/<名前>.json [--exclude app-2ef57e] [--md]
# ストレージ(Docker の特権コンテナ。手順は hack/bench-storage.sh の冒頭)
hack/bench-storage.sh
```

一覧は [`log.md`](log.md)。
