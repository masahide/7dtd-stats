# tsfile（タグ付き時系列ファイル）仕様書 v0.1

**モジュール名:** `github.com/masahide/7dtd-stats/pkg/tsfile`
**ステータス**: Draft / Experimental
**最終更新**: 2025-08-26 (JST)
**対象言語/環境**: Go 1.22+（標準ライブラリのみ）

> 設計の多くは「軽量・依存ゼロ・運用簡便」を最優先にした私見にもとづきます（私の意見です）。本番要件に応じて調整してください。

---

## 1. 目的

- **タグ付きメトリック**を**時刻でパーティションされたファイル**（1 時間単位）に**append-only**で保存する軽量ライブラリを提供する。
- **SQL/TSDB を使わず**に、書き込み・範囲読み・日単位の削除（保管期間管理）・安全な終了（シグナル対応）を実現。

### 非目標（Non-goals）

- 高度なクエリエンジン（JOIN/複雑な条件式/集計クエリの実行）
- レプリケーション/分散合意/クラスタリング
- 強いトランザクション整合性（完全 ACID）

---

## 2. 用語とデータモデル

### 2.1 Point（データ点）

```json
{
  "t": "2025-08-26T04:12:34.567Z", // UTC ISO8601
  "v": 12.34, // 値（float64）
  "tags": {
    // 任意のキー/値（文字列）
    "host": "game01",
    "region": "tokyo"
  }
}
```

- `t`: すべて **UTC** に正規化して保存。
- `v`: 数値（double）。
- `tags`: 任意のラベル集合（`map[string]string`]）。

### 2.2 タグの正規化/ハッシュ

- タグは `k=v` をキーで**昇順**に連結 →`"k1=v1;k2=v2;..."` を **カノニカル文字列**とし、その **SHA-1** の **先頭 8 バイト（16 hex）** を **タグハッシュ**（`tagHash`）とする。
- `tagHash` はディレクトリ名に使う。
- `tagHash/labels.json` に **人間可読なタグ集合**を保存（ハッシュ → 実体の対応が分かる）。

> 先頭 8 バイト採用はパス短縮と衝突確率のバランス上の判断です（私見）。衝突が懸念される場合は 16 バイト（32 hex）へ拡張可能。

---

## 3. オンディスク構造

```
<root>/<series>/<tagHash>/<YYYY>/<MM>/<DD>/<HH>.ndjson.gz
<root>/<series>/<tagHash>/labels.json   // タグ実体
```

- ファイルは **1 時間** 粒度でローテーション。
- 内容は **NDJSON（1 行 1 レコード）** を **gzip** で圧縮。
- gzip は **連結メンバー**を許容（再オープンして追記しても合法）。

---

## 4. 公開 API

### 4.1 型

```go
// データ点
 type Point struct {
     T    time.Time       // UTC
     V    float64
     Tags map[string]string // 省略可
 }

// ルーター（推奨エントリポイント）
 type Router struct { /* ... */ }
```

### 4.2 生成

```go
func NewRouter(root, series string, opts ...WriterOpt) *Router
```

- `root`: ベースディレクトリ（例: `"data"`）
- `series`: 系列名（例: `"metrics"`）
- `opts`: オプション（下記）

#### オプション（WriterOpt）

```go
func WithLocation(loc *time.Location) WriterOpt       // ファイル名時刻のTZ（既定: UTC）
func WithFlushEvery(n int) WriterOpt                  // n件ごとに Flush+Sync（0=無効）
func WithFlushInterval(d time.Duration) WriterOpt     // d間隔で定期 Flush（<=0で無効）
```

> 遅延損失を抑えるなら `WithFlushInterval(1-2s)` 推奨（私見）。

### 4.3 書き込み

```go
func (r *Router) Append(p Point) error
```

- `p.T` は自動で **UTC** へ正規化。
- `p.Tags` から `tagHash` を計算し、該当 writer へ委譲。
- 時刻の **1 時間境界** を跨ぐと自動ローテート。

### 4.4 フラッシュ/クローズ

```go
func (r *Router) Flush() error // すべての writer を Flush+Sync
func (r *Router) Close() error // 定期フラッシュ停止→Flush→Close
```

- `Close()` は、内部の定期フラッシュ goroutine を停止し、すべてのファイルに対して `Flush()+Close()` を実行。

### 4.5 範囲スキャン（読み取り）

```go
func ScanRange(root, series string, from, to time.Time, fn func(Point) bool) error
```

- `series` 配下の **すべての tagHash** を対象に、`[from, to]` を 1 時間単位で探索し、NDJSON をストリームデコード。
- `fn` が `false` を返すと早期終了。
- フィルタが必要な場合は、`fn` 内で `p.Tags` を見て判定。

### 4.6 保管期間（削除）ユーティリティ

```go
// JST など任意TZの日境界で日ディレクトリごと削除
func DeleteBeforeDay(root, series string, boundaryDay time.Time, loc *time.Location) error
```

- `boundaryDay` を `loc` で日切りし、**その前日以前**の `YYYY/MM/DD` ディレクトリを再帰削除。
- すべての `tagHash` に対して適用。

---

## 5. 例

### 5.1 書き込み（シグナル安全な終了）

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
defer stop()

r := tsfile.NewRouter("data", "metrics",
    tsfile.WithLocation(time.UTC),
    tsfile.WithFlushEvery(1000),
    tsfile.WithFlushInterval(2*time.Second),
)
defer r.Close()

t := time.NewTicker(500 * time.Millisecond)
defer t.Stop()
for {
    select {
    case <-ctx.Done():
        _ = r.Flush() // 任意
        return
    case <-t.C:
        _ = r.Append(tsfile.Point{
            T: time.Now(), V: rand.Float64(),
            Tags: map[string]string{"host":"game01","region":"tokyo"},
        })
    }
}
```

### 5.2 読み取り（タグフィルタ）

```go
from := time.Now().Add(-time.Hour)
_ = tsfile.ScanRange("data", "metrics", from, time.Now(), func(p tsfile.Point) bool {
    if p.Tags["region"] != "tokyo" { return true }
    // ...集計処理...
    return true
})
```

### 5.3 日次削除（JST 境界）

```go
jst, _ := time.LoadLocation("Asia/Tokyo")
keepDays := 30
_ = tsfile.DeleteBeforeDay("data", "metrics", time.Now().In(jst).AddDate(0,0,-keepDays), jst)
```

### 5.4 systemd 推奨設定

```ini
[Service]
Type=simple
ExecStart=/usr/local/bin/yourapp
Restart=on-failure
KillSignal=SIGTERM
TimeoutStopSec=15s
```

---

## 6. 整合性と耐障害性

- **Append-only**: 追記のみ。上書き/削除は行わない（削除はディレクトリ単位）。
- **Flush/Sync**: `Flush()` は `bufio.Writer` と `gzip.Writer` をフラッシュ後、`fsync` を実施。
- **定期フラッシュ**: `WithFlushInterval()` により、数秒おきに自動フラッシュ。電源断時の損失を低減。
- **SIGKILL 非対応**: `SIGKILL` は捕捉不可。損失最小化のため **短いフラッシュ間隔**を推奨（私見）。
- **gzip 連結メンバー**: ファイル再オープン → 追記でも gzip として合法。リーダーは連結を順に展開。

---

## 7. 並行性と運用ガイド

- **スレッド安全**: `Router.Append` は複数 goroutine から呼んで良い。
- **プロセス間**: **同一 `series`・同一タグ集合** を複数プロセスで**同時に書かない**（推奨）。必要ならファイルロックの導入を検討。
- **ファイル数**: 1 タグ集合につき **24/日**、**\~720/月**。タグのカーディナリティ増大に注意。
- **時刻順序**: ファイル内は挿入順。厳密な昇順を保証しない。必要なら後処理でソート。
- **重複**: ライブラリは重複排除しない。必要に応じて `(t, tags)` キーなどで重複排除。

---

## 8. パフォーマンス指針（目安）

- **圧縮**: `gzip.BestSpeed` 採用。書き込み CPU を抑えつつサイズを削減（私見でのバランス選択）。
- **バッファ**: `bufio` 既定 1MB。I/O 負荷に応じて調整可能。
- **フラッシュ**: レイテンシ重視 → 短間隔、スループット重視 → 件数/間隔を大きめに。

> ワークロード差が大きいため TPS の数値保証は行いません。必要ならベンチマークの雛形を提供します。

---

## 9. エラーハンドリング

- 書き込みエラーは `Append()` が返却。上位でリトライ/再初期化を判断。
- スキャン時、ファイルが存在しない場合はスキップ。
- 破損 gzip/JSON はエラーとして返却（デフォルト動作）。許容する場合は拡張版 `ScanRange` を別途提供可能。

---

## 10. 互換性/拡張性

- **スキーマ v1**: `{"t","v","tags"}`。将来フィールド追加は **後方互換**を意図。
- **タグハッシュ長**は将来拡張可能（既存との混在許容）。
- **代替バックエンド**: Parquet/zstd 版や SQLite/TimescaleDB への移行時も、スキーマとレイアウトの概念は再利用可能。

---

## 11. セキュリティ/権限

- アプリ実行ユーザが `root` 以下の読み書き権限を持つこと。
- ラベル情報（`labels.json`）に機微情報を載せない運用を推奨（私見）。

---

## 12. テスト/検証

- ユニットテスト観点

  - タグ正規化/ハッシュ安定性
  - 時間ローテーションの境界挙動
  - 定期フラッシュの動作
  - ScanRange の範囲一致/早期終了
  - DeleteBeforeDay の日境界（JST/UTC 差）

- 耐障害テスト

  - 異常終了（`SIGTERM` 中断）時のデータ消失上限

---

## 13. よくある質問（FAQ）

- **Q: JSON ではなくバイナリが良いのでは？**
  A: 可搬性/デバッグ容易さを優先して NDJSON を採用（私見）。圧縮下でのサイズ効率は十分実用的。

- **Q: 1 時間ではなく 1 分単位にしたい**
  A: `writer` 実装の「丸め粒度」を差し替え可能。粒度が細かいほどファイル数が増える点に注意。

- **Q: 追記中にクラッシュしたら gzip は壊れない？**
  A: 直近メンバーが壊れる可能性はあるが、先頭からのデータは取り出せる。短間隔フラッシュで影響を限定（私見）。

---

## 付録 A: 実装スケッチ（要約）

- `Router.Append` → `Tags.Hash()` → `writer` をタグごとにキャッシュ。
- `writer` は `curHour` を持ち、跨いだら `rotate()`。
- `rotate()` は `OpenFile(O_CREATE|O_WRONLY|O_APPEND)` → `gzip.NewWriterLevel(BestSpeed)` → `bufio.NewWriter(1MB)`。
- `Flush()` は `bufio.Flush()` → `gzip.Flush()` → `fsync()`。
- `WithFlushInterval` で goroutine を使い定期フラッシュ。
- `Close()` は定期フラッシュ停止後、全 writer をクローズ。

### 使用例:

```go
package main

import (
	"context"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/tsfile"
)

func main() {
	// SIGINT/SIGTERM/SIGQUIT でキャンセルされる ctx
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	r := tsfile.NewRouter("data", "metrics",
		tsfile.WithLocation(time.UTC),
		tsfile.WithFlushEvery(1000),          // 1000件ごとに Flush+Sync
		tsfile.WithFlushInterval(2*time.Second), // 2秒ごとに定期 Flush
	)
	// 正常終了でも異常終了（panic recover後など）でも確実に Close
	defer r.Close()

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			// 最後に明示フラッシュ（任意、Close でも行われる）
			_ = r.Flush()
			return
		case <-t.C:
			_ = r.Append(tsfile.Point{
				T: time.Now(),
				V: rand.Float64(),
				Tags: tsfile.Tags{
					"host":   "game01",
					"region": "tokyo",
				},
			})
		}
	}
}
```

systemd 側のおすすめ設定（任意）:

```ini
[Service]
Type=simple
ExecStart=/usr/local/bin/yourapp
Restart=on-failure
KillSignal=SIGTERM
TimeoutStopSec=15s   # Closeが終わる余裕
```

### 運用メモ（私見）

- 同一シリーズ・同一タグ集合を複数プロセスで同時に書かないのが安全。どうしても必要ならファイルロックを追加。
- SIGKILL/電源断では直近数秒〜数件が失われる可能性があります。FlushInterval を短めに、fsync は flushSync() 内で実施済み。
- 削除は日単位なら root/<series>/<taghash>/<YYYY>/<MM>/<DD> を丸ごと削除で OK（JST 境界で消すなら計算にだけ JST を使う）。
