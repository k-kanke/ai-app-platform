# Agent の選び方と設定

Agent Job が実行するスクリプトは `AAP_AGENT`(Control Plane の環境変数)で選ぶ。

| AAP_AGENT | 中身 | 必要なもの |
|---|---|---|
| `template` | LLM なしのスタブ。テンプレートを置くだけ(変更依頼では何も変えない) | なし |
| `gemini` | Gemini CLI(headless, `--approval-mode yolo`) | image を `INSTALL_GEMINI=true` で build(CI は常に有効)、`GEMINI_API_KEY` |
| `claude` | Claude Code CLI | image を `INSTALL_CLAUDE=true` で build、`ANTHROPIC_API_KEY` |

## Gemini を有効にする手順
1. API キーを Google AI Studio で発行する(チャットや Git には貼らない)。
2. ホームクラスタ(ubuntu-26)で Secret を作る。Agent Job にだけ渡される:
   ```sh
   kubectl -n aap-apps create secret generic agent-credentials \
     --from-literal=GEMINI_API_KEY='<KEY>'
   ```
3. `kubernetes-platform/manifests/ai-app-platform/control-plane.yaml` の `AAP_AGENT` を `gemini` に変えて push。
   モデルは Control Plane の `AAP_GEMINI_MODEL` で指定する(Agent Job に渡される。未指定なら CLI の既定)。
4. Portal から新しいアプリを作って確認する。

## 安全面
- Agent は `yolo`(全ツール自動承認)で動く。守っているのは Pod の側:
  Source PVC だけ mount / Data・SA トークンなし / 非 root / NetworkPolicy で 443 以外と LAN への通信を禁止。
- プロンプトインジェクション(Agent が読むファイルに紛れた指示)で起きうる最悪は、
  そのアプリの Source の破壊と、443 経由での Source の持ち出し。壊れた変更は snapshot から戻る。
- テレメトリ・利用統計・自動更新は `entrypoint.sh` で無効化している。
- **Gemini API の無料枠では、入力が製品改善に使われる場合がある。** 送られるのは依頼文と
  Source だけ(アプリの実データは Agent に渡らない)。気になる場合は課金を有効にする。

## 既知の限界
- Agent の標準出力(失敗の詳細)は Pod のログにだけ残り、Portal には出ない
  (`kubectl -n aap-apps logs job/aap-gen-<id>-agent-<op>`)。Control Plane がログ末尾を
  Operation のエラーに取り込む改善が必要。
- 変更は稼働中の Source を直接書き換える(Stage 2 の Isolated Workspace で解消予定)。

## Gemini のモデルと費用(2026-10 時点、公式の価格表より。$ / 100 万トークン、入力 / 出力)
| モデル | 価格 | メモ |
|---|---|---|
| `gemini-3.8-flash` | 0.75 / 3.75(2027-01 から 1.50 / 7.50) | 開発・エージェント向けの最上位 Flash。**最初の選択** |
| `gemini-3.5-flash-lite` | 0.30 / 2.50 | 低コスト。品質が足りるなら切り替える |
| `gemini-2.5-flash-lite` | 0.10 / 0.40 | 旧世代。最安 |

モデルの変更は `AAP_GEMINI_MODEL` の1行。無料枠(Free tier)もあるが、入力が製品改善に使われうる。
予算の上限と通知は Google 側(AI Studio / Cloud の予算アラート)で必ず設定する。
