package storage

import (
	"sync"
	"testing"
	"time"

	"github.com/masahide/7dtd-stats/pkg/tsfile"
)

// --------------- helpers ---------------

func newStoreForTest(t *testing.T) (*TSStore, string) {
	t.Helper()
	root := t.TempDir()

	// テストは読み取り前に Close するため、FlushInterval は不要
	s := NewTSStore(
		root,
		tsfile.WithLocation(time.UTC),
		tsfile.WithFlushEvery(1),    // 念のため 1件ごと Flush（必須ではない）
		tsfile.WithFlushInterval(0), // 定期フラッシュ無効
	)
	// ここでは Cleanup で Close するが、各テスト中で明示 Close 済みでも多重 Close 可
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func withinDur(a, b float64, eps float64) bool {
	if a > b {
		return a-b <= eps
	}
	return b-a <= eps
}

// ScanRange ユーティリティ
func collect(t *testing.T, root, series string, from, to time.Time, pred func(p tsfile.Point) bool) ([]tsfile.Point, error) {
	t.Helper()
	var points []tsfile.Point
	err := tsfile.ScanRange(root, series, from, to, func(p tsfile.Point) bool {
		if pred == nil || pred(p) {
			points = append(points, p)
		}
		return true
	})
	return points, err
}

// --------------- tests ---------------

func TestEnsureRouterIdempotentAndConcurrent(t *testing.T) {
	s, _ := newStoreForTest(t)

	const series = "players.x"
	const goroutines = 16

	var wg sync.WaitGroup
	wg.Add(goroutines)

	ptrs := make([]*tsfile.Router, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			r, err := s.EnsureRouter(series)
			ptrs[i] = r
			errs[i] = err
		}()
	}
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("EnsureRouter err at %d: %v", i, errs[i])
		}
	}

	head := ptrs[0]
	for i := 1; i < goroutines; i++ {
		if ptrs[i] != head {
			t.Fatalf("EnsureRouter returned different instances: %p vs %p", head, ptrs[i])
		}
	}
}

func TestAppendVecAndScan(t *testing.T) {
	s, root := newStoreForTest(t)

	now := time.Now().UTC()
	pid := "P:test:1"
	world := "Navezgane"
	src := "test"
	name := "Alice"
	x := 123.45
	z := -67.89

	if err := s.AppendVec("players", now, map[string]float64{"x": x, "z": z},
		map[string]string{"player_id": pid, "world": world, "src": src, "name": name}); err != nil {
		t.Fatalf("AppendVec error: %v", err)
	}

	// 重要：読み取り前に Close（gzip フッターを書き出す）
	if err := s.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	// players.x
	px, err := collect(t, root, "players.x", from, to, func(p tsfile.Point) bool {
		return p.Tags["player_id"] == pid && p.Tags["world"] == world && p.Tags["src"] == src
	})
	if err != nil {
		t.Fatalf("ScanRange players.x: %v", err)
	}
	if len(px) == 0 {
		t.Fatalf("players.x: no points found")
	}
	foundX := false
	for _, p := range px {
		if p.T.Equal(now) && withinDur(p.V, x, 1e-9) {
			foundX = true
			break
		}
	}
	if !foundX {
		t.Fatalf("players.x: expected value %v at %v not found; got %+v", x, now, px)
	}

	// players.z
	pz, err := collect(t, root, "players.z", from, to, func(p tsfile.Point) bool {
		return p.Tags["player_id"] == pid && p.Tags["world"] == world && p.Tags["src"] == src
	})
	if err != nil {
		t.Fatalf("ScanRange players.z: %v", err)
	}
	if len(pz) == 0 {
		t.Fatalf("players.z: no points found")
	}
	foundZ := false
	for _, p := range pz {
		if p.T.Equal(now) && withinDur(p.V, z, 1e-9) {
			foundZ = true
			break
		}
	}
	if !foundZ {
		t.Fatalf("players.z: expected value %v at %v not found; got %+v", z, now, pz)
	}
}

func TestAppendEventAndScan(t *testing.T) {
	s, root := newStoreForTest(t)

	now := time.Now().UTC()
	pid := "P:test:2"
	world := "RWG"
	src := "test"
	kind := "player_connect"

	if err := s.AppendEvent(now, kind, map[string]string{
		"player_id": pid, "world": world, "src": src,
	}); err != nil {
		t.Fatalf("AppendEvent error: %v", err)
	}

	// 読み取り前に Close（gzip 完了）
	if err := s.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	pts, err := collect(t, root, "events.count", from, to, func(p tsfile.Point) bool {
		return p.Tags["kind"] == kind && p.Tags["player_id"] == pid && p.Tags["world"] == world
	})
	if err != nil {
		t.Fatalf("ScanRange events.count: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("events.count: no matching points")
	}
	seen := false
	for _, p := range pts {
		if p.T.Equal(now) && withinDur(p.V, 1.0, 1e-12) {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("events.count: expected (V=1, T=%v) not found; got %+v", now, pts)
	}
}

func TestFlushCloseAndPostCloseBehavior(t *testing.T) {
	s, _ := newStoreForTest(t)

	// Write one point
	now := time.Now().UTC()
	if err := s.Append("players.x", tsfile.Point{
		T: now, V: 10,
		Tags: map[string]string{"player_id": "P:test:3"},
	}); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// FlushAll は任意だが、Close で確実に完結
	if err := s.FlushAll(); err != nil {
		t.Fatalf("FlushAll error: %v", err)
	}

	// Close は冪等
	if err := s.Close(); err != nil {
		t.Fatalf("Close(1) error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close(2) error: %v", err)
	}

	// Close 後の EnsureRouter はエラー
	if _, err := s.EnsureRouter("players.x"); err == nil {
		t.Fatalf("EnsureRouter should fail after Close, but got nil error")
	}
}

func TestRetentionRemovesOldData(t *testing.T) {
	s, root := newStoreForTest(t)

	jst, _ := time.LoadLocation("Asia/Tokyo")

	// 2日前と現在に点を投入
	oldT := time.Now().In(jst).Add(-48 * time.Hour).UTC()
	now := time.Now().UTC()
	tags := map[string]string{"player_id": "P:test:4"}

	if err := s.Append("players.x", tsfile.Point{T: oldT, V: 1, Tags: tags}); err != nil {
		t.Fatalf("Append old error: %v", err)
	}
	if err := s.Append("players.x", tsfile.Point{T: now, V: 2, Tags: tags}); err != nil {
		t.Fatalf("Append now error: %v", err)
	}

	// 読み取りとリテンションを行う前に Close（gzip 完結）
	if err := s.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	// Sanity: 両方見えること
	{
		pts, err := collect(t, root, "players.x", oldT.Add(-time.Minute), now.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["player_id"] == "P:test:4"
		})
		if err != nil {
			t.Fatalf("pre-retention ScanRange: %v", err)
		}
		if len(pts) < 2 {
			t.Fatalf("pre-retention: expected >=2 points, got %d", len(pts))
		}
	}

	// Retention: JST 境界で days=0 → 今日より前（=前日以前）を削除
	// TSStore.Retention は Close 済みでも動作する（ファイル削除のみ）
	if err := s.Retention(0, jst, "players.x"); err != nil {
		t.Fatalf("Retention error: %v", err)
	}

	// 古い点は消え、現在の点は残る
	{
		ptsOld, err := collect(t, root, "players.x", oldT.Add(-30*time.Minute), oldT.Add(30*time.Minute), func(p tsfile.Point) bool {
			return p.Tags["player_id"] == "P:test:4" && p.T.Equal(oldT)
		})
		if err != nil {
			t.Fatalf("post-retention ScanRange (old): %v", err)
		}
		if len(ptsOld) != 0 {
			t.Fatalf("post-retention: old point should be deleted, got %d", len(ptsOld))
		}

		ptsNow, err := collect(t, root, "players.x", now.Add(-time.Minute), now.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["player_id"] == "P:test:4" && p.T.Equal(now)
		})
		if err != nil {
			t.Fatalf("post-retention ScanRange (now): %v", err)
		}
		if len(ptsNow) == 0 {
			t.Fatalf("post-retention: recent point missing")
		}
	}
}

func TestRetentionEnumeratesSeries(t *testing.T) {
	s, root := newStoreForTest(t)

	jst, _ := time.LoadLocation("Asia/Tokyo")

	// 2日前の古いデータを2つのシリーズに投入（enumeration 対象にする）
	oldT := time.Now().In(jst).Add(-48 * time.Hour).UTC()

	if err := s.Append("players.x", tsfile.Point{
		T: oldT, V: 100,
		Tags: map[string]string{"player_id": "P:A", "world": "W1"},
	}); err != nil {
		t.Fatalf("Append players.x(old): %v", err)
	}
	if err := s.Append("events.count", tsfile.Point{
		T: oldT, V: 1,
		Tags: map[string]string{"kind": "player_connect", "player_id": "P:B", "world": "W1"},
	}); err != nil {
		t.Fatalf("Append events.count(old): %v", err)
	}

	// 念のため片方には現在のデータも入れておく（残ることを確認）
	now := time.Now().UTC()
	if err := s.Append("players.x", tsfile.Point{
		T: now, V: 200,
		Tags: map[string]string{"player_id": "P:A", "world": "W1"},
	}); err != nil {
		t.Fatalf("Append players.x(now): %v", err)
	}

	// 読み取り・リテンション前に Close して gzip を完結
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 事前確認: 両シリーズに古いデータが見えること
	{
		pts, err := collect(t, root, "players.x", oldT.Add(-time.Minute), oldT.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["player_id"] == "P:A"
		})
		if err != nil {
			t.Fatalf("pre players.x ScanRange: %v", err)
		}
		if len(pts) == 0 {
			t.Fatalf("pre: players.x old not found")
		}

		pts2, err := collect(t, root, "events.count", oldT.Add(-time.Minute), oldT.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["kind"] == "player_connect" && p.Tags["player_id"] == "P:B"
		})
		if err != nil {
			t.Fatalf("pre events.count ScanRange: %v", err)
		}
		if len(pts2) == 0 {
			t.Fatalf("pre: events.count old not found")
		}
	}

	// ★ series 指定なしで Retention 実行 → os.ReadDir(s.root) による自動列挙分岐を通る
	if err := s.Retention(0, jst /* no series args */); err != nil {
		t.Fatalf("Retention (enumerate) error: %v", err)
	}

	// 古いデータは両シリーズで削除されている
	{
		pts, err := collect(t, root, "players.x", oldT.Add(-time.Minute), oldT.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["player_id"] == "P:A"
		})
		if err != nil {
			t.Fatalf("post players.x ScanRange(old): %v", err)
		}
		if len(pts) != 0 {
			t.Fatalf("post: players.x old should be deleted, got %d", len(pts))
		}

		pts2, err := collect(t, root, "events.count", oldT.Add(-time.Minute), oldT.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["kind"] == "player_connect" && p.Tags["player_id"] == "P:B"
		})
		if err != nil {
			t.Fatalf("post events.count ScanRange(old): %v", err)
		}
		if len(pts2) != 0 {
			t.Fatalf("post: events.count old should be deleted, got %d", len(pts2))
		}
	}

	// 直近データ（players.x の now）は残っている
	{
		ptsNow, err := collect(t, root, "players.x", now.Add(-time.Minute), now.Add(time.Minute), func(p tsfile.Point) bool {
			return p.Tags["player_id"] == "P:A" && p.T.Equal(now)
		})
		if err != nil {
			t.Fatalf("post players.x ScanRange(now): %v", err)
		}
		if len(ptsNow) == 0 {
			t.Fatalf("post: players.x recent point missing")
		}
	}
}
