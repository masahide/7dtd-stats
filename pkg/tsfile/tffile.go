package tsfile

import (
	"bufio"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Tags map[string]string

// 正規化された "k=v;k=v;..." 文字列
func (t Tags) Canonical() string {
	if len(t) == 0 {
		return ""
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]byte, 0, 64)
	for i, k := range keys {
		if i > 0 {
			out = append(out, ';')
		}
		out = append(out, k...)
		out = append(out, '=')
		out = append(out, t[k]...)
	}
	return string(out)
}

func (t Tags) Hash() string {
	sum := sha1.Sum([]byte(t.Canonical()))
	// 衝突リスクを抑えつつパス短縮（64bit=16hex）
	return hex.EncodeToString(sum[:8])
}

func (t Tags) Clone() Tags {
	cp := make(Tags, len(t))
	for k, v := range t {
		cp[k] = v
	}
	return cp
}

type Point struct {
	T    time.Time `json:"t"` // UTC
	V    float64   `json:"v"`
	Tags Tags      `json:"tags,omitempty"` // 任意
}

// ---- 単一タグセット用 Writer ----

type writer struct {
	root, series, tagHash string
	tags                  Tags

	loc         *time.Location // ファイル名のタイムゾーン（UTC推奨）
	curHour     time.Time
	f           *os.File
	gz          *gzip.Writer
	bw          *bufio.Writer
	enc         *json.Encoder
	pending     int
	flushEvery  int
	flushTicker *time.Ticker
	flushStop   chan struct{}
	flushWg     sync.WaitGroup
	closeOnce   sync.Once
	mu          sync.Mutex
}

type WriterOpt func(*writer)

func WithLocation(loc *time.Location) WriterOpt { return func(w *writer) { w.loc = loc } }
func WithFlushEvery(n int) WriterOpt            { return func(w *writer) { w.flushEvery = n } }
func WithFlushInterval(d time.Duration) WriterOpt {
	return func(w *writer) {
		if d <= 0 {
			return
		}
		w.flushTicker = time.NewTicker(d)
		w.flushStop = make(chan struct{})
	}
}

func newWriter(root, series string, tags Tags, opts ...WriterOpt) *writer {
	w := &writer{
		root:    root,
		series:  series,
		tags:    tags.Clone(),
		tagHash: tags.Hash(),
		loc:     time.UTC,
	}
	for _, opt := range opts {
		opt(w)
	}
	// ラベルメタを書いておく（同内容なら上書きでOK）
	if err := w.writeLabelsMeta(); err != nil {
		// メタ書き込み失敗は致命でなくても良いのでログ代わりに標準エラーへ
		fmt.Fprintf(os.Stderr, "tsfile: labels meta write error: %v\n", err)
	}
	// 定期フラッシュ
	if w.flushTicker != nil {
		w.flushWg.Add(1)
		go func(ch <-chan time.Time, stop <-chan struct{}) {

			defer w.flushWg.Done()
			for {
				select {
				case <-ch:
					_ = w.flushSync()
				case <-stop:
					return
				}
			}
		}(w.flushTicker.C, w.flushStop)
	}
	return w
}

func (w *writer) pathForHour(h time.Time) (dir, file string) {
	dir = filepath.Join(w.root, w.series, w.tagHash,
		h.Format("2006"), h.Format("01"), h.Format("02"))
	file = filepath.Join(dir, h.Format("15")+".ndjson.gz")
	return
}

func (w *writer) writeLabelsMeta() error {
	dir := filepath.Join(w.root, w.series, w.tagHash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "labels.json")
	tmp := metaPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w.tags); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath)
}

func (w *writer) Append(p Point) error {
	p.T = p.T.UTC()
	// ファイル名タイムゾーンで丸め（例: UTC）
	hour := p.T.In(w.loc).Truncate(time.Hour)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil || hour != w.curHour {
		if err := w.rotate(hour); err != nil {
			return err
		}
	}
	if err := w.enc.Encode(&p); err != nil {
		return err
	}
	w.pending++
	if w.flushEvery > 0 && w.pending >= w.flushEvery {
		if err := w.flushSync(); err != nil {
			return err
		}
		w.pending = 0
	}
	return nil
}

func (w *writer) rotate(hour time.Time) error {
	if err := w.closeCurrent(); err != nil {
		return err
	}
	w.curHour = hour
	dir, file := w.pathForHour(hour)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		f.Close()
		return err
	}
	bw := bufio.NewWriterSize(gz, 1<<20)
	w.f, w.gz, w.bw = f, gz, bw
	w.enc = json.NewEncoder(bw)
	return nil
}

func (w *writer) flushSync() error {
	if w.bw != nil {
		if err := w.bw.Flush(); err != nil {
			return err
		}
	}
	if w.gz != nil {
		if err := w.gz.Flush(); err != nil {
			return err
		}
	}
	if w.f != nil {
		return w.f.Sync()
	}
	return nil
}

func (w *writer) closeCurrent() error {
	if w.enc == nil {
		return nil
	}
	_ = w.flushSync()
	if w.gz != nil {
		_ = w.gz.Close()
	}
	if w.f != nil {
		_ = w.f.Close()
	}
	w.f, w.gz, w.bw, w.enc = nil, nil, nil, nil
	return nil
}

func (w *writer) Close() error {
	var cerr error
	w.closeOnce.Do(func() {
		// 停止: フラッシュ goroutine（順不同でもOKだが明示で止める）
		if w.flushTicker != nil {
			w.flushTicker.Stop()
			w.flushTicker = nil
		}
		if w.flushStop != nil {
			close(w.flushStop) // 1回だけ
			w.flushStop = nil
		}
		w.flushWg.Wait() // goroutine 終了待ち

		w.mu.Lock()
		defer w.mu.Unlock()
		cerr = w.closeCurrent()
	})
	return cerr
}

// ---- タグ付きマルチライター（推奨 API） ----

type Router struct {
	root, series string
	loc          *time.Location
	opts         []WriterOpt

	mu      sync.Mutex
	writers map[string]*writer // key = tagHash
}

func NewRouter(root, series string, opts ...WriterOpt) *Router {
	r := &Router{
		root:    root,
		series:  series,
		loc:     time.UTC,
		opts:    append([]WriterOpt{WithLocation(time.UTC)}, opts...),
		writers: make(map[string]*writer),
	}
	return r
}

func (r *Router) Append(p Point) error {
	if p.Tags == nil {
		p.Tags = Tags{}
	}
	key := p.Tags.Hash()

	r.mu.Lock()
	w, ok := r.writers[key]
	if !ok {
		w = newWriter(r.root, r.series, p.Tags, r.opts...)
		r.writers[key] = w
	}
	r.mu.Unlock()

	return w.Append(p)
}

// すべての内部 writer を Flush+Sync
func (r *Router) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.writers {
		if err := w.flushSync(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for _, w := range r.writers {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---- 範囲スキャン（必要なときに） ----

// ScanRange は series 配下の全タグセットを舐めて [from,to] をストリーム処理。
// fn が false を返すと早期終了。
func ScanRange(root, series string, from, to time.Time, fn func(Point) bool) error {
	if to.Before(from) {
		return errors.New("invalid range")
	}
	from = from.UTC()
	to = to.UTC()

	seriesDir := filepath.Join(root, series)
	entries, err := os.ReadDir(seriesDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// e.Name() は tagHash ディレクトリ
		if err := scanTagDir(filepath.Join(seriesDir, e.Name()), from, to, fn); err != nil {
			if errors.Is(err, errEarlyStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

func scanTagDir(tagDir string, from, to time.Time, fn func(Point) bool) error {
	// YYYY/MM/DD/HH.ndjson.gz を辿る
	for h := from.Truncate(time.Hour); !h.After(to); h = h.Add(time.Hour) {
		path := filepath.Join(tagDir,
			h.Format("2006"), h.Format("01"), h.Format("02"), h.Format("15")+".ndjson.gz")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := scanFile(path, from, to, fn); err != nil {
			if errors.Is(err, errEarlyStop) {
				return errEarlyStop
			}
			return err
		}
	}
	return nil
}

func scanFile(path string, from, to time.Time, fn func(Point) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	dec := json.NewDecoder(gz)
	for {
		var p Point
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if p.T.Before(from) || p.T.After(to) {
			continue
		}
		if !fn(p) {
			return errEarlyStop
		}
	}
}

var errEarlyStop = errors.New("tsfile: early stop")

// ---- 保管期間ユーティリティ ----

// DeleteBeforeDay は、指定 loc の日境界で boundaryDay の「その日より前」の日ディレクトリ
// (YYYY/MM/DD) を series 配下の全 tagHash について再帰削除する。
// 例: boundaryDay=JSTで 2025-08-26 の場合、2025/08/25 以前のディレクトリを削除。
func DeleteBeforeDay(root, series string, boundaryDay time.Time, loc *time.Location) error {
	if loc == nil {
		loc = time.UTC
	}
	by, bm, bd := boundaryDay.In(loc).Date()
	cutYMD := by*10000 + int(bm)*100 + bd

	seriesDir := filepath.Join(root, series)
	tagDirs, err := os.ReadDir(seriesDir)
	if err != nil {
		return err
	}
	for _, td := range tagDirs {
		if !td.IsDir() {
			continue
		}
		tagDir := filepath.Join(seriesDir, td.Name())
		// 年ディレクトリ
		years, err := os.ReadDir(tagDir)
		if err != nil {
			return err
		}
		for _, yentry := range years {
			if !yentry.IsDir() {
				continue
			}
			y, err := strconv.Atoi(yentry.Name())
			if err != nil || y <= 0 {
				continue
			}
			ydir := filepath.Join(tagDir, yentry.Name())
			months, err := os.ReadDir(ydir)
			if err != nil {
				return err
			}
			for _, mentry := range months {
				if !mentry.IsDir() {
					continue
				}
				m, err := strconv.Atoi(mentry.Name())
				if err != nil || m < 1 || m > 12 {
					continue
				}
				mdir := filepath.Join(ydir, mentry.Name())
				days, err := os.ReadDir(mdir)
				if err != nil {
					return err
				}
				for _, dentry := range days {
					if !dentry.IsDir() {
						continue
					}
					d, err := strconv.Atoi(dentry.Name())
					if err != nil || d < 1 || d > 31 {
						continue
					}
					ymd := y*10000 + m*100 + d
					if ymd < cutYMD {
						// 対象日ディレクトリを削除
						if err := os.RemoveAll(filepath.Join(mdir, dentry.Name())); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}
