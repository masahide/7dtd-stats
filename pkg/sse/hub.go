package sse

import (
	"bufio"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event は1件のSSEイベントです。
// Data はUTF-8テキスト（通常はJSON文字列）を想定します。
type Event struct {
	ID   int64  // 連番ID（文字列化して id: に出力）
	Name string // event: 名（空文字可）
	Data []byte // data: 本文（改行含む可）
}

// オプション
type options struct {
	replaySize   int
	pingInterval time.Duration
	clientBuf    int
	writeTimeout time.Duration
}

// Option は Hub のオプション設定です。
type Option func(*options)

// WithReplay はリプレイ保持件数を設定します（0 で無効）。
func WithReplay(n int) Option {
	return func(o *options) {
		if n < 0 {
			n = 0
		}
		o.replaySize = n
	}
}

// WithPingInterval は :ping コメント送信間隔を設定します。
func WithPingInterval(d time.Duration) Option { return func(o *options) { o.pingInterval = d } }

// WithClientBuffer は各クライアントの送信バッファサイズを設定します。
func WithClientBuffer(n int) Option {
	return func(o *options) {
		if n < 1 {
			n = 1
		}
		o.clientBuf = n
	}
}

// WithWriteTimeout は各書き込みのタイムアウトを設定します（0 で無効）。
func WithWriteTimeout(d time.Duration) Option { return func(o *options) { o.writeTimeout = d } }

// Hub はSSEの接続・ブロードキャスト・リプレイを管理します。
type Hub struct {
	// 設定
	opt options

	// 連番ID
	nextID int64

	// リプレイ用リングバッファ
	mu     sync.RWMutex
	ring   []Event // len <= opt.replaySize
	start  int     // リングの先頭インデックス
	length int     // 現在の件数

	// 接続管理
	register   chan *client
	unregister chan *client
	broadcast  chan Event

	// ライフサイクル
	done chan struct{}
}

// client は1つの接続を表します。
type client struct {
	w       http.ResponseWriter
	flusher http.Flusher
	r       *http.Request
	ch      chan Event
	filter  func(Event) bool
}

// NewHub を生成します。
func NewHub(opts ...Option) *Hub {
	o := options{
		replaySize:   256,
		pingInterval: 15 * time.Second,
		clientBuf:    32,
		writeTimeout: 0,
	}
	for _, f := range opts {
		f(&o)
	}
	h := &Hub{
		opt:        o,
		register:   make(chan *client),
		unregister: make(chan *client),
		broadcast:  make(chan Event, 128),
		done:       make(chan struct{}),
	}
	if o.replaySize > 0 {
		h.ring = make([]Event, o.replaySize)
	}
	return h
}

// Run は Hub のメインループを開始します。別ゴルーチンで実行してください。
func (h *Hub) Run() {
	// 接続集合（Runスレッド専有）
	conns := make(map[*client]struct{})
	for {
		select {
		case <-h.done:
			// 全切断
			for c := range conns {
				close(c.ch)
			}
			return
		case c := <-h.register:
			conns[c] = struct{}{}
		case c := <-h.unregister:
			if _, ok := conns[c]; ok {
				delete(conns, c)
				close(c.ch)
			}
		case ev := <-h.broadcast:
			// リングに記録
			h.pushReplay(ev)
			// 各クライアントに送信（バッファフルなら落とす）
			for c := range conns {
				if c.filter != nil && !c.filter(ev) {
					continue
				}
				select {
				case c.ch <- ev:
				default:
					// バッファ溢れはドロップ（混雑耐性）
				}
			}
		}
	}
}

// Close は全接続を閉じ、Run ループを停止します。
func (h *Hub) Close() { close(h.done) }

// Broadcast はイベントを全クライアントに送信します。ID は内部で付与されます。
func (h *Hub) Broadcast(name string, data []byte) Event {
	id := atomic.AddInt64(&h.nextID, 1)
	ev := Event{ID: id, Name: name, Data: append([]byte(nil), data...)}
	select {
	case h.broadcast <- ev:
	default:
		// 混雑時にブロッキングしない（通常は十分なバッファを推奨）
		h.broadcast <- ev
	}
	return ev
}

// ServeHTTP は /sse/live ハンドラ実装です。
// クエリ: topics=pos,events （省略時は制限なし）
// ヘッダ or クエリ: Last-Event-ID / last_event_id（数値）
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// SSE ヘッダ
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// フィルタ（topics）
	var filter func(Event) bool
	topics := parseCSV(r.URL.Query().Get("topics"))
	if len(topics) > 0 {
		allowed := make(map[string]struct{}, len(topics))
		for _, t := range topics {
			if t == "" {
				continue
			}
			allowed[t] = struct{}{}
		}
		filter = func(ev Event) bool {
			if ev.Name == "" {
				return true
			}
			_, ok := allowed[ev.Name]
			return ok
		}
	}

	c := &client{
		w:       w,
		flusher: flusher,
		r:       r,
		ch:      make(chan Event, h.opt.clientBuf),
		filter:  filter,
	}

	// 接続登録
	select {
	case <-h.done:
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	case h.register <- c:
	}

	// リプレイ送信
	if lastID, ok := readLastEventID(r); ok {
		replay := h.collectSince(lastID)
		for _, ev := range replay {
			if !writeEvent(w, flusher, h.opt.writeTimeout, ev) {
				h.unregister <- c
				return
			}
		}
	}

	// 初期フラッシュ（ヘッダ送信）
	flusher.Flush()

	// ピングタイマ
	var ping *time.Ticker
	if h.opt.pingInterval > 0 {
		ping = time.NewTicker(h.opt.pingInterval)
		defer ping.Stop()
	}

	// クライアントループ
	for {
		select {
		case <-r.Context().Done():
			h.unregister <- c
			return
		case <-h.done:
			h.unregister <- c
			return
		case ev, ok := <-c.ch:
			if !ok {
				return
			}
			if !writeEvent(w, flusher, h.opt.writeTimeout, ev) {
				h.unregister <- c
				return
			}
		case <-ping.C:
			if !writePing(w, flusher, h.opt.writeTimeout) {
				h.unregister <- c
				return
			}
		}
	}
}

// 内部: リングに push（排他）
func (h *Hub) pushReplay(ev Event) {
	if cap(h.ring) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.length < cap(h.ring) {
		h.ring[h.length] = ev
		h.length++
		return
	}
	// 古い先頭を上書き
	h.ring[h.start] = ev
	h.start = (h.start + 1) % cap(h.ring)
}

// 内部: lastID より新しいイベントを取得（排他）
func (h *Hub) collectSince(lastID int64) []Event {
	if h.length == 0 || cap(h.ring) == 0 {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := h.length
	res := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		idx := (h.start + i) % cap(h.ring)
		ev := h.ring[idx]
		if ev.ID > lastID {
			res = append(res, ev)
		}
	}
	return res
}

// ユーティリティ
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readLastEventID(r *http.Request) (int64, bool) {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return id, true
		}
	}
	if v := r.URL.Query().Get("last_event_id"); v != "" {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return id, true
		}
	}
	return 0, false
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, timeout time.Duration, ev Event) bool {
	bw := bufio.NewWriter(w)
	if ev.Name != "" {
		if _, err := bw.WriteString("event: "); err != nil {
			return false
		}
		if _, err := bw.WriteString(ev.Name); err != nil {
			return false
		}
		if _, err := bw.WriteString("\n"); err != nil {
			return false
		}
	}
	if ev.ID > 0 {
		if _, err := bw.WriteString("id: "); err != nil {
			return false
		}
		if _, err := bw.WriteString(strconv.FormatInt(ev.ID, 10)); err != nil {
			return false
		}
		if _, err := bw.WriteString("\n"); err != nil {
			return false
		}
	}
	// data:（複数行対応）
	if len(ev.Data) > 0 {
		for _, line := range strings.Split(string(ev.Data), "\n") {
			if _, err := bw.WriteString("data: "); err != nil {
				return false
			}
			if _, err := bw.WriteString(line); err != nil {
				return false
			}
			if _, err := bw.WriteString("\n"); err != nil {
				return false
			}
		}
	} else {
		if _, err := bw.WriteString("data:\n"); err != nil {
			return false
		}
	}
	if _, err := bw.WriteString("\n"); err != nil {
		return false
	}
	if err := bw.Flush(); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func writePing(w http.ResponseWriter, flusher http.Flusher, timeout time.Duration) bool {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(":ping\n\n"); err != nil {
		return false
	}
	if err := bw.Flush(); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// DebugString は現在のリングの内容を文字列化（テスト/デバッグ用）
func (h *Hub) DebugString() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var b strings.Builder
	b.WriteString("ring[")
	for i := 0; i < h.length; i++ {
		idx := (h.start + i) % cap(h.ring)
		ev := h.ring[idx]
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d:%s", ev.ID, ev.Name)
	}
	b.WriteString("]")
	return b.String()
}
