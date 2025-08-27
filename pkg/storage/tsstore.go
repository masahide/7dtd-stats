// storage/tsstore.go
package storage

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/masahide/7dtd-stats/pkg/tsfile"
)

type RouterFactory func(series string) []tsfile.WriterOpt

type TSStore struct {
	root     string
	factory  RouterFactory // シリーズごとに WriterOpt を与えたい場合に使う
	routers  sync.Map      // map[string]*tsfile.Router  (シリーズ名 → Router)
	closeMux sync.Mutex
	closed   bool
}

// NewTSStore: 既定の WriterOpt を使う簡易コンストラクタ
func NewTSStore(root string, defaultOpts ...tsfile.WriterOpt) *TSStore {
	return &TSStore{
		root: root,
		factory: func(_ string) []tsfile.WriterOpt {
			return defaultOpts
		},
	}
}

// NewTSStoreWithFactory: シリーズごとに個別のオプションを付与したい場合
func NewTSStoreWithFactory(root string, f RouterFactory) *TSStore {
	return &TSStore{root: root, factory: f}
}

// EnsureRouter: シリーズ名に対応する Router を遅延生成（スレッド安全）
func (s *TSStore) EnsureRouter(series string) (*tsfile.Router, error) {
	if s.isClosed() {
		return nil, errors.New("TSStore closed")
	}
	if v, ok := s.routers.Load(series); ok {
		return v.(*tsfile.Router), nil
	}
	// create new
	r := tsfile.NewRouter(s.root, series, s.factory(series)...)
	actual, loaded := s.routers.LoadOrStore(series, r)
	if loaded {
		// すでに他ゴルーチンが作っていたら今作った方を閉じる
		_ = r.Close()
	}
	return actual.(*tsfile.Router), nil
}

// Append: 汎用の 1点書き込み
func (s *TSStore) Append(series string, p tsfile.Point) error {
	r, err := s.EnsureRouter(series)
	if err != nil {
		return err
	}
	return r.Append(p)
}

// AppendVec: ベクトル値（例: players の X/Z/Y）を任意軸だけ書く
// 例: AppendVec("players", t, map[string]float64{"x":X, "z":Z}, tags)
func (s *TSStore) AppendVec(base string, t time.Time, axes map[string]float64, tags map[string]string) error {
	for axis, v := range axes {
		if err := s.Append(base+"."+axis, tsfile.Point{T: t, V: v, Tags: tags}); err != nil {
			return err
		}
	}
	return nil
}

// AppendEvent: カウント系イベント（connect/death など）
func (s *TSStore) AppendEvent(t time.Time, kind string, tags map[string]string) error {
	if tags == nil {
		tags = map[string]string{}
	}
	tags["kind"] = kind
	return s.Append("events.count", tsfile.Point{T: t, V: 1, Tags: tags})
}

func (s *TSStore) FlushAll() error {
	var err error
	s.routers.Range(func(_, v any) bool {
		if e := v.(*tsfile.Router).Flush(); e != nil && err == nil {
			err = e
		}
		return true
	})
	return err
}

func (s *TSStore) Close() error {
	s.closeMux.Lock()
	defer s.closeMux.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	s.routers.Range(func(_, v any) bool {
		if e := v.(*tsfile.Router).Close(); e != nil && err == nil {
			err = e
		}
		return true
	})
	return err
}

func (s *TSStore) isClosed() bool {
	s.closeMux.Lock()
	defer s.closeMux.Unlock()
	return s.closed
}

// Retention: 引数 series が空なら root 直下の全シリーズを自動列挙
func (s *TSStore) Retention(days int, loc *time.Location, series ...string) error {
	if loc == nil {
		loc = time.UTC
	}
	boundary := time.Now().In(loc).AddDate(0, 0, -days)

	list := series
	if len(list) == 0 {
		ents, _ := os.ReadDir(s.root)
		for _, e := range ents {
			if e.IsDir() {
				list = append(list, e.Name())
			}
		}
	}

	for _, sv := range list {
		if err := tsfile.DeleteBeforeDay(s.root, sv, boundary, loc); err != nil {
			return err
		}
	}
	return nil
}
