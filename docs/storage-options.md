# 複数ノードでのデータの扱い方: 選択肢の調査(2026-10-18)

worker が 2 台になり、`local-path` のデータが 1 つのノードに縛られることが分かった(経緯は [`overview.md`](overview.md) §5)。
この文書は、ノードをまたぐデータの扱い方の選択肢を、**実験の前に**調べた結果。実験の結果は [`measurements/log.md`](measurements/log.md) に別に記録する。

> **根拠の凡例**: ✅ 公式ドキュメントや実機で確認 / ⚠ 記憶・二次情報(要確認) / 🔜 実験で確かめる

---

## 1. この基盤の事情(選択の前提)

| 事情 | 内容 |
|---|---|
| 今のストレージ | `local-path`(`WaitForFirstConsumer`)。PVC は最初に使った Pod のノードに固定される(✅ 実機で確認) |
| 共有の要否 | Agent / お試し版 / 本番 / helper Job は、同じ PVC を使う。今は「同じノードに寄る」ことで成立している |
| SQLite | Control Plane の DB(WAL モード)は PVC の上。アプリのデータも `/data` のファイル(SQLite を使うアプリもありうる) |
| 規模 | worker 2 台。メモリは worker-01 が約 5.3 GB(空き約 1.9 GB)、worker-02 が約 3.3 GB(空き約 2.3 GB) |
| 許容できる停止 | 家族向け。数時間の停止は許容できる。データは毎晩のバックアップ(R2)で守る |
| 目的 | 「共有」(別ノードから同じデータを見る)と「冗長化」(ノードが落ちても別ノードで再開)は別の目的 |

## 2. 環境の現状(✅ 2026-10-18 に読み取りで確認)

| 項目 | worker-01 | worker-02 |
|---|---|---|
| カーネル | 7.0.0-38 | 7.0.0-38 |
| CPU / メモリ | 4 / 5.3 GB | 4 / 3.3 GB |
| ルートの空き | 19 GB | 25 GB |
| `open-iscsi` | 導入済み(`iscsid` は inactive) | 同左 |
| `cryptsetup` | 導入済み | 同左 |
| `nfs-common` | **未導入** | **未導入** |
| カーネルモジュール | `iscsi_tcp` `nfs` `nfsd` `dm_crypt` が利用可 | 同左 |

Kubernetes は v1.36.4。

## 3. 選択肢

| 選択肢 | 目的 | 性質 |
|---|---|---|
| `local-path`(今) | - | **基準**。1 ノードのディスク。共有も冗長化もできない |
| NFS + `nfs-subdir-external-provisioner` | 共有 | 1 台の NFS サーバーを全ノードが使う。RWX 可。サーバーが単一障害点 |
| Longhorn | 冗長化(と、RWX) | 複製するブロックストレージ。通常は RWO。RWX は中で NFS を使う |
| オブジェクトストレージ(Garage) | 設計の変更 | 共有ボリュームをやめ、アプリから S3 の API で使う |
| Rook/Ceph | 大規模・堅牢性 | 本構成には重く、**試さない**(§3.4) |

### 3.1 NFS + provisioner

- ✅ Helm で入る。`nfs.server` と `nfs.path` を指定するだけ。現行は v4.0.2。
- ✅ ノードに `nfs-common` が必要。**NFS サーバーが別途要る**(NAS があればそれ、なければ VM やノードに `nfs-kernel-server`)。
- ✅ 制限: **容量は強制されない**、リサイズ非対応、ディレクトリの既定は `root:root` の `0777`。
- 🔜 性能、SQLite の挙動(§4)、サーバー停止時の Pod の挙動。

### 3.2 Longhorn

- ✅ 最新は 1.13.0。**Kubernetes 1.34 以上**が条件で、本環境(1.36.4)は満たす。
- ✅ ノードに `open-iscsi`(`iscsid` が動くこと)と `nfs-common`、`cryptsetup`、`dmsetup` が要る。カーネルモジュールは `iscsi_tcp`。V1 のファイルシステムは ext4 / XFS。
- ✅ 環境チェックは `longhornctl check preflight`。
- ⚠ **メモリの必要量は、公式のインストール手順のページに明記がない**。worker-02(3.3 GB)で足りるかは実測が必要。
- ✅ 通常のボリュームは RWO(複数ノードから同時には使えない)。RWX は share manager の Pod が NFS で公開する。
- 🔜 常駐のメモリ・CPU、ノード障害時の再開、書き込み性能。

### 3.3 オブジェクトストレージ: MinIO は使えず、Garage を候補にする

- ✅(二次情報)**MinIO のコミュニティ版は、2025 年 10 月にビルド済みのバイナリと Docker イメージの配布を止め、同年 12 月に maintenance mode、2026 年にリポジトリが archived になった**。時期は情報源でずれる。使うなら自分でビルドするか、フォークを使う。
- ✅ Garage: 公式の Helm chart がある。PVC を使い、生のディスクは不要。複数ノードのレイアウトはデプロイ後に CLI で手動設定する。例ではレプリケーション係数 2。単一ノード用の設定もある。
- ⚠ Garage のメモリ・CPU の必要量は、公式のページに明記がなかった。
- 🔜 小さなアプリのデータ(JSON / ファイル)を S3 の API に置き換える手間。Agent への指示とテンプレートをどう変えるか。

### 3.4 Rook/Ceph を試さない理由(⚠ 記憶に基づく。実験では確認しない)

3 ノード以上と、生のディスク、ノードあたり数 GB のメモリが前提。本構成(worker 2 台、メモリ 3〜5 GB)に合わない。

## 4. SQLite との相性

- ✅ SQLite の公式ドキュメント(WAL の説明)に「WAL does not work over a network filesystem」とある。理由は、WAL が同じホストのプロセス間で共有メモリを使うため。
- 推論(🔜 実験で確かめる):

| ストレージ | SQLite の置き場として |
|---|---|
| `local-path` / Longhorn(RWO) | ローカルのディスクと同じ形。問題ないはず |
| NFS | ロックが弱い。WAL は公式に非対応。壊れる・`database is locked` になる可能性が高い |
| Garage | ファイルシステムではない。SQLite のファイルは置けない(DB は別の置き場が要る) |

- 含意: **ストレージの選択が、アプリ側の設計を縛る**。

## 5. 実験の計画

比較の基準は `local-path`。観点は次の 4 つ。

| 観点 | 測り方 |
|---|---|
| 書き込みの性能 | `fio`(小さい同期書き込み。SQLite のような用途を想定) |
| ノード障害時の挙動 | ノードを止め、Pod が別ノードで再開するか。データは残るか |
| メモリの常駐コスト | 各方式を入れたあとの `kubectl top`(ノード・Pod) |
| SQLite との相性 | 各ストレージに SQLite(WAL)を置き、複数のプロセスから書き込み、整合性を確認する |

- 実験の場所: **本物のクラスタには入れない**(家族のアプリが動いている)。使い捨ての VM に別のクラスタを作る。🔜 未決定。
- 既存の道具: `hack/bench-storage.sh`(ext4 / XFS 向けのコピーとクォータの測定)。

## 6. 未確認・未決定

- Longhorn と Garage の実際のメモリ使用量。
- 自宅に NAS があるか(なければ NFS サーバーを VM に立てる)。
- 実験用の VM の用意(Proxmox)。
- MinIO の archived の時期(情報源でずれる)。

## 出典

- [Longhorn の導入要件](https://longhorn.io/docs/latest/deploy/install/)
- [nfs-subdir-external-provisioner](https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner)
- [Garage: Kubernetes](https://garagehq.deuxfleurs.fr/documentation/cookbook/kubernetes/)
- [SQLite: WAL](https://www.sqlite.org/wal.html)
- MinIO の経緯(二次情報): [MinIO Is Dead, Long Live MinIO](https://blog.vonng.com/en/db/minio-resurrect/)、[MinIO in Maintenance Mode](https://ayedo.de/en/posts/minio-im-maintenance-mode/)、[MinIO's community edition is archived](https://stormdevelopments.ca/blog/minio-s-community-edition-is-archived-what-still-runs-in-2026/)
