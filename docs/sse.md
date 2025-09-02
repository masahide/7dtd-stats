# SSE（Server-Sent Events）実装仕様 v0.1

最終更新: 2025-09-02 (JST)

本書は、本リポジトリの Go サービスにおける SSE 配信の仕様とパッケージ API（`pkg/sse`）をまとめたものです。フロント（Svelte/Leaflet）からのリアルタイム購読、バックエンド（Poller 等）からのイベント送出の両面を対象にしています。

---

## 1. 目的 / スコープ

- クライアントへ軽量な一方向ストリーミングを提供（位置更新・イベント通知）。
- 低遅延・低コスト・ブラウザ互換性重視（WebSocket ではなく SSE）。
- 切断時の再接続・簡易リプレイ（直近 N 件）・定期 ping（keep-alive）。

非目標: 双方向通信、厳密な欠損補填（ベストエフォートのリプレイ）。

---

## 2. エンドポイント

- パス: `GET /sse/live`
- クエリ:
  - `topics`: カンマ区切り（例: `pos,events`）。指定時、その `event:` 名のみ配信。未指定は全イベント。
  - `last_event_id`: 数値。`Last-Event-ID` ヘッダの代替（互換のため）。
- リクエストヘッダ（推奨）:
  - `Accept: text/event-stream`
  - `Last-Event-ID: <int>` 再接続時の追送開始 ID。
- レスポンスヘッダ:
  - `Content-Type: text/event-stream`
  - `Cache-Control: no-cache`
  - `Connection: keep-alive`
  - `X-Accel-Buffering: no`（Nginx 等のバッファ無効化ヒント）

---

## 3. イベントフォーマット

SSE 標準に従います。1 イベントは空行で区切られます。

```
event: <name>         # 任意（未指定時は 'message' として扱われる）
id: <int>             # 連番 ID（リプレイの基準）
data: <line1>
data: <line2>         # 複数行可（UTF-8）

```

keep-alive のため、定期的にコメント行を送ります。

```
:ping

```

---

## 4. 既定動作（サーバ実装）

- ping: 既定 15s 間隔で `:ping` コメントを送信。
- リプレイ: 直近 `N` 件（既定 256 件）をリングバッファに保持。
- Last-Event-ID: ヘッダまたは `last_event_id` が与えられた場合、より新しい ID のイベントをリプレイ送出。
- バックプレッシャ: クライアント送信バッファが満杯のときはドロップ（接続全体は維持）。
- 切断: クライアント切断/サーバ停止でクリーンにクローズ。サーバ停止時は新規接続は `503`。
- フィルタ: `topics` を指定した場合、その `event:` 名に一致するもののみ送出。

注意: リプレイはベストエフォートです。長期断や大量イベントでリングを越えた場合は欠損があり得ます（再接続後に最新に追従する用途を想定）。

---

## 5. 想定イベント種別（例）

- `event: pos` 位置更新（プレイヤー等）
  - `data:` は JSON 例
    ```json
    {"pid":"P:steam:...","x":123.45,"z":-67.8,"t":"2025-09-02T12:34:56.789Z"}
    ```
- `event: events` 汎用イベント
  - `data:` は JSON 例
    ```json
    {"kind":"player_connect","pid":"P:steam:...","t":"2025-09-02T12:34:56.789Z"}
    ```

イベント名・JSON スキーマは最小限で開始し、需要に応じて拡張します。

---

## 6. クライアント実装例（ブラウザ）

```js
const es = new EventSource('/sse/live?topics=pos,events');

es.addEventListener('pos', (e) => {
  const msg = JSON.parse(e.data); // { pid, x, z, t }
  // TODO: Leaflet の polyline へ追記
});

es.addEventListener('events', (e) => {
  const ev = JSON.parse(e.data); // { kind, pid, t }
  // TODO: 通知/ログなど
});

es.onmessage = (e) => {
  // event: 未指定のメッセージ用（現状は未使用）
};

es.onerror = () => {
  // 自動再接続に任せる。Last-Event-ID はブラウザが保持。
};
```

手動確認: `curl -N http://localhost:8081/sse/live`（15 秒ごとに `:ping` が流れます）。

---

## 7. Go サーバ統合（概要）

- エントリポイント: `cmd/server/main.go`
- マウント:
  ```go
  hub := sse.NewHub(
    sse.WithReplay(256),
    sse.WithPingInterval(15*time.Second),
    sse.WithClientBuffer(64),
  )
  go hub.Run()
  defer hub.Close()

  mux.Handle("/sse/live", http.HandlerFunc(hub.ServeHTTP))
  ```
- 送出（将来の Poller から）:
  ```go
  payload := []byte(`{"pid":"P:...","x":123.4,"z":-56.7,"t":"2025-09-02T12:34:56Z"}`)
  hub.Broadcast("pos", payload)
  ```

---

## 8. `pkg/sse` パッケージ API

- 型
  - `type Event struct { ID int64; Name string; Data []byte }`
  - `type Hub struct { ... }`
- 生成/起動
  - `func NewHub(opts ...Option) *Hub`
  - `func (*Hub) Run()` / `func (*Hub) Close()`
- 配信
  - `func (*Hub) ServeHTTP(w http.ResponseWriter, r *http.Request)`
  - `func (*Hub) Broadcast(name string, data []byte) Event`
- オプション
  - `WithReplay(n int)`（既定 256）: 直近リプレイ件数
  - `WithPingInterval(d time.Duration)`（既定 15s）: ping 間隔
  - `WithClientBuffer(n int)`（既定 32）: クライアント送信バッファ（溢れたらドロップ）
  - `WithWriteTimeout(d time.Duration)`: 予約（現状未使用）

---

## 9. 運用・注意点

- 逆プロキシ: Nginx 等を使う場合は `proxy_buffering off;` または `X-Accel-Buffering: no` を尊重する設定に。
- 断への耐性: 長時間断・高トラフィック時はリプレイ欠損があり得る。重要イベントは別途 REST 参照で補完検討。
- 認可: 現状未実装。導入時は `Authorization: Bearer` などで保護。
- メトリクス: 将来的に接続数、送信/ドロップ件数、リング長、最終 ID 等の公開を検討。

---

## 10. 今後の拡張

- Poller 実装からの実データ送出（`pos`/`events`）。
- SSE のイベント圧縮/間引き（高頻度位置更新での帯域節約）。
- `Last-Event-ID` の堅牢化（永続キューや高速ストレージ採用の検討）。
- 認可・レート制限・監視メトリクスの整備。

