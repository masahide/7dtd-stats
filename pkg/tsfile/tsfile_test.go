package tsfile

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

func readAllNDJSONGz(t *testing.T, path string) []Point {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	var out []Point
	dec := json.NewDecoder(bufio.NewReader(gz))
	for {
		var p Point
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestTagsCanonicalAndHashDeterministic(t *testing.T) {
	a := Tags{"b": "2", "a": "1"}
	b := Tags{"a": "1", "b": "2"}
	if a.Canonical() != "a=1;b=2" {
		t.Fatalf("canonical unexpected: %q", a.Canonical())
	}
	if a.Hash() != b.Hash() {
		t.Fatalf("hash differs for logically same tags: %s vs %s", a.Hash(), b.Hash())
	}
	c := a.Clone()
	c["a"] = "9"
	if a["a"] == c["a"] {
		t.Fatalf("Clone should be deep copy")
	}
	if a.Hash() == c.Hash() {
		t.Fatalf("hash should change when value changes")
	}
}

func TestRouterAppendCreatesFilesAndLabels(t *testing.T) {
	dir := t.TempDir()
	series := "metrics"
	tags := Tags{"host": "game01", "region": "tokyo"}
	tagHash := tags.Hash()

	r := NewRouter(dir, series,
		WithLocation(time.UTC),
		WithFlushEvery(1), // 1件ごとにFlushして確実に出力
	)
	defer r.Close()

	base := time.Date(2025, 8, 26, 12, 34, 0, 0, time.UTC)
	points := []Point{
		{T: base.Add(1 * time.Second), V: 1.0, Tags: tags},
		{T: base.Add(2 * time.Second), V: 2.0, Tags: tags},
	}
	for _, p := range points {
		if err := r.Append(p); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	_ = r.Close()

	// 12時のファイルを確認
	hourDir := filepath.Join(dir, series, tagHash, "2025", "08", "26")
	fpath := filepath.Join(hourDir, "12.ndjson.gz")
	if _, err := os.Stat(fpath); err != nil {
		t.Fatalf("expected file not found: %s (%v)", fpath, err)
	}
	got := readAllNDJSONGz(t, fpath)
	if len(got) != 2 {
		t.Fatalf("want 2 points, got %d", len(got))
	}

	// labels.json 確認
	lblPath := filepath.Join(dir, series, tagHash, "labels.json")
	bs, err := os.ReadFile(lblPath)
	if err != nil {
		t.Fatalf("read labels.json: %v", err)
	}
	var gotLabels map[string]string
	if err := json.Unmarshal(bs, &gotLabels); err != nil {
		t.Fatalf("labels.json parse: %v", err)
	}
	if gotLabels["host"] != "game01" || gotLabels["region"] != "tokyo" {
		t.Fatalf("labels mismatch: %+v", gotLabels)
	}
}

func TestRotationAcrossHour(t *testing.T) {
	dir := t.TempDir()
	series := "metrics"
	tags := Tags{"app": "srv", "env": "prod"}
	tagHash := tags.Hash()

	r := NewRouter(dir, series, WithLocation(time.UTC))
	defer r.Close()

	base := time.Date(2025, 8, 26, 12, 59, 50, 0, time.UTC)
	// 12:59:50〜12:59:59（10件）と13:00:00〜13:00:09（10件）
	for i := 0; i < 20; i++ {
		p := Point{T: base.Add(time.Duration(i) * time.Second), V: float64(i), Tags: tags}
		if err := r.Append(p); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	_ = r.Close()

	path12 := filepath.Join(dir, series, tagHash, "2025", "08", "26", "12.ndjson.gz")
	path13 := filepath.Join(dir, series, tagHash, "2025", "08", "26", "13.ndjson.gz")

	if _, err := os.Stat(path12); err != nil {
		t.Fatalf("missing %s: %v", path12, err)
	}
	if _, err := os.Stat(path13); err != nil {
		t.Fatalf("missing %s: %v", path13, err)
	}
	pts12 := readAllNDJSONGz(t, path12)
	pts13 := readAllNDJSONGz(t, path13)
	if len(pts12) == 0 || len(pts13) == 0 {
		t.Fatalf("rotation split should produce data in both files, got %d and %d", len(pts12), len(pts13))
	}
	// 12時ファイルの最後 ≤ 12:59:59, 13時ファイルの最初 ≥ 13:00:00 をざっくり確認
	last12 := pts12[len(pts12)-1].T
	first13 := pts13[0].T
	if !(last12.Hour() == 12 && first13.Hour() == 13) {
		t.Fatalf("unexpected hour boundaries: last12=%s first13=%s", last12, first13)
	}
}

func TestScanRangeReturnsExpectedPoints(t *testing.T) {
	dir := t.TempDir()
	series := "metrics"

	r := NewRouter(dir, series,
		WithLocation(time.UTC),
		WithFlushEvery(1),
	)
	defer r.Close()

	base := time.Date(2025, 8, 26, 10, 0, 0, 0, time.UTC)
	tokyo := Tags{"region": "tokyo", "host": "game01"}
	osaka := Tags{"region": "osaka", "host": "game02"}

	// 4点ずつ投入
	for i := 0; i < 4; i++ {
		_ = r.Append(Point{T: base.Add(time.Minute * time.Duration(i)), V: float64(i), Tags: tokyo})
		_ = r.Append(Point{T: base.Add(time.Minute * time.Duration(i)), V: float64(i + 100), Tags: osaka})
	}
	_ = r.Close()

	from := base.Add(-30 * time.Second)
	to := base.Add(3 * time.Minute).Add(30 * time.Second)

	var got []Point
	err := ScanRange(dir, series, from, to, func(p Point) bool {
		if p.Tags["region"] == "tokyo" {
			got = append(got, p)
		}
		return true
	})
	if err != nil {
		t.Fatalf("ScanRange: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 tokyo points, got %d", len(got))
	}
	// 時刻でソートして妥当性確認
	sort.Slice(got, func(i, j int) bool { return got[i].T.Before(got[j].T) })
	for i, p := range got {
		want := base.Add(time.Minute * time.Duration(i))
		if !p.T.Equal(want) {
			t.Fatalf("point %d time mismatch: got %s want %s", i, p.T, want)
		}
		if p.Tags["host"] != "game01" {
			t.Fatalf("unexpected host: %s", p.Tags["host"])
		}
	}
}

func TestConcurrentAppendIsSafe(t *testing.T) {
	dir := t.TempDir()
	series := "metrics"

	r := NewRouter(dir, series,
		WithLocation(time.UTC),
		WithFlushEvery(50),
	)
	defer r.Close()

	var wg sync.WaitGroup
	workers := 8
	perWorker := 100
	base := time.Date(2025, 8, 26, 11, 0, 0, 0, time.UTC)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tag := Tags{"region": "tokyo", "host": "game%02d"}
			tag["host"] = sprintf("game%02d", w)
			for i := 0; i < perWorker; i++ {
				_ = r.Append(Point{
					T:    base.Add(time.Second * time.Duration(w*perWorker+i)),
					V:    float64(w*perWorker + i),
					Tags: tag,
				})
			}
		}(w)
	}
	wg.Wait()
	_ = r.Close()

	// 全範囲をスキャンして件数を確認
	from := base
	to := base.Add(time.Second * time.Duration(workers*perWorker+1))
	count := 0
	err := ScanRange(dir, series, from, to, func(p Point) bool {
		count++
		return true
	})
	if err != nil {
		t.Fatalf("ScanRange: %v", err)
	}
	want := workers * perWorker
	if count != want {
		t.Fatalf("want %d points, got %d", want, count)
	}
	if runtime.GOOS == "windows" {
		// Windows のファイル共有モード絡みで false positive を避けるため、念のため Flush 済み
	}
}

// small printf helper to avoid importing fmt everywhere
func sprintf(format string, a ...any) string {
	return (func() string {
		return fmtSprintf(format, a...)
	})()
}

// keep fmt in a tiny separate scope to not clutter imports in each test
func fmtSprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}

// --- ここから追加テスト ---

func TestRouterFlushWritesBufferedData(t *testing.T) {
	dir := t.TempDir()
	series := "metrics"
	tags := Tags{"host": "game01", "region": "tokyo"}
	tagHash := tags.Hash()

	// FlushIntervalは使わず、FlushEveryもしない＝バッファに溜める
	r := NewRouter(dir, series,
		WithLocation(time.UTC),
	)
	defer r.Close()

	base := time.Date(2025, 8, 26, 12, 0, 0, 0, time.UTC)
	// 1件だけ書いて、Flush前のファイルサイズが0であることを（できれば）確認
	if err := r.Append(Point{T: base.Add(10 * time.Second), V: 42, Tags: tags}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// ファイルパス
	hourDir := filepath.Join(dir, series, tagHash, "2025", "08", "26")
	fpath := filepath.Join(hourDir, "12.ndjson.gz")

	// ファイルは作られているはず（rotateでOpenFileするため）
	if _, err := os.Stat(fpath); err != nil {
		t.Fatalf("expected file to exist before flush: %v", err)
	}

	// Flush前はbufio/gzipが吐き出していないため、通常はサイズ0のはず
	if fi, err := os.Stat(fpath); err == nil {
		if fi.Size() != 0 {
			t.Fatalf("expected size=0 before Flush, got %d", fi.Size())
		}
	}

	// 明示Flushで実体が出る
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Flush後はサイズが増えているはず
	after, err := os.Stat(fpath)
	if err != nil {
		t.Fatalf("stat after flush: %v", err)
	}
	if after.Size() <= 0 {
		t.Fatalf("expected file to have non-zero size after Flush, got %d", after.Size())
	}

	// 最後はCloseしてgzipフッタを書き、デコード可能になることを確認
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := readAllNDJSONGz(t, fpath)
	if len(got) != 1 || got[0].V != 42 {
		t.Fatalf("decoded points mismatch: %+v", got)
	}
}

func TestWithFlushIntervalAutoFlush(t *testing.T) {
	dir := t.TempDir()
	series := "metrics"
	tags := Tags{"app": "srv", "env": "prod"}
	tagHash := tags.Hash()
	// 50msごとの自動Flushを有効化
	r := NewRouter(dir, series,
		WithLocation(time.UTC),
		WithFlushInterval(50*time.Millisecond),
	)
	defer r.Close()

	base := time.Date(2025, 8, 26, 13, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := r.Append(Point{
			T:    base.Add(time.Duration(i) * time.Second),
			V:    float64(i),
			Tags: tags,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	hourDir := filepath.Join(dir, series, tagHash, "2025", "08", "26")
	fpath := filepath.Join(hourDir, "13.ndjson.gz")

	// 自動Flushにより、Closeする前でも一定時間内にサイズが >0 になることを待つ
	deadline := time.Now().Add(1 * time.Second)
	for {
		fi, err := os.Stat(fpath)
		if err == nil && fi.Size() > 0 {
			break // OK: 自動フラッシュで吐かれた
		}
		if time.Now().After(deadline) {
			t.Fatalf("file size did not become >0 within 1s (err=%v size=%v)", err, sizeOrZero(fi))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 最後にCloseして全データがデコードできることを確認
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := readAllNDJSONGz(t, fpath)
	if len(got) != 5 {
		t.Fatalf("want 5 points, got %d", len(got))
	}
	// 軽い妥当性
	for i := range got {
		if got[i].V != float64(i) {
			t.Fatalf("unexpected V at %d: %v", i, got[i].V)
		}
	}
}

func sizeOrZero(fi os.FileInfo) int64 {
	if fi == nil {
		return 0
	}
	return fi.Size()
}
