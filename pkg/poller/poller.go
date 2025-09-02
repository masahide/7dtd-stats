package poller

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "strings"
    "sync"
    "time"

    "github.com/masahide/7dtd-stats/pkg/sse"
)

// Player は最小限のプレイヤー情報です。
type Player struct {
    ID   string
    Name string
    X    float64
    Z    float64
}

// Provider はプレイヤー一覧を返すデータソースです。
type Provider interface {
    FetchPlayers(ctx context.Context) ([]Player, error)
}

// JSONProvider は任意の JSON エンドポイントからプレイヤー情報を抽出します。
// 期待構造：
//  - ルートが配列、またはオブジェクト内の players/data/items フィールドが配列
//  - 各要素はオブジェクトで、以下の候補キーから ID, Name, X, Z を抽出
//    ID:   id, player_id, steamid, steamId, entityId
//    Name: name, playerName, nick
//    X:    x, xpos, x_pos
//    Z:    z, zpos, z_pos
type JSONProvider struct {
    URL     string
    Client  *http.Client
    Timeout time.Duration
}

func (p *JSONProvider) FetchPlayers(ctx context.Context) ([]Player, error) {
    if p.URL == "" {
        return nil, errors.New("poller: JSONProvider.URL is empty")
    }
    client := p.Client
    if client == nil {
        client = http.DefaultClient
    }
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
    if err != nil {
        return nil, err
    }
    if p.Timeout > 0 {
        ctx2, cancel := context.WithTimeout(req.Context(), p.Timeout)
        defer cancel()
        req = req.WithContext(ctx2)
    }
    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
        return nil, fmt.Errorf("poller: GET %s: %s: %s", p.URL, resp.Status, string(b))
    }
    dec := json.NewDecoder(resp.Body)
    var root any
    if err := dec.Decode(&root); err != nil {
        return nil, err
    }
    arr, ok := pickArray(root)
    if !ok {
        return nil, errors.New("poller: unsupported JSON shape (array or object with players/data/items[] expected)")
    }
    out := make([]Player, 0, len(arr))
    for _, it := range arr {
        m, ok := it.(map[string]any)
        if !ok {
            continue
        }
        id := pickString(m, "id", "player_id", "steamid", "steamId", "entityId")
        if id == "" {
            continue
        }
        name := pickString(m, "name", "playerName", "nick")
        x, xok := pickFloat(m, "x", "xpos", "x_pos")
        z, zok := pickFloat(m, "z", "zpos", "z_pos")
        if !xok || !zok {
            continue
        }
        out = append(out, Player{ID: id, Name: name, X: x, Z: z})
    }
    return out, nil
}

func pickArray(v any) ([]any, bool) {
    switch t := v.(type) {
    case []any:
        return t, true
    case map[string]any:
        for _, k := range []string{"players", "data", "items", "list"} {
            if a, ok := t[k].([]any); ok {
                return a, true
            }
        }
    }
    return nil, false
}

func pickString(m map[string]any, keys ...string) string {
    for _, k := range keys {
        if v, ok := m[k]; ok {
            switch s := v.(type) {
            case string:
                return s
            }
        }
        for k2, v := range m {
            if strings.EqualFold(k, k2) {
                if s, ok := v.(string); ok { return s }
            }
        }
    }
    return ""
}

func pickFloat(m map[string]any, keys ...string) (float64, bool) {
    for _, k := range keys {
        if v, ok := m[k]; ok {
            switch n := v.(type) {
            case float64:
                return n, true
            case json.Number:
                f, err := n.Float64(); if err == nil { return f, true }
            case int:
                return float64(n), true
            case int64:
                return float64(n), true
            }
        }
        for k2, v := range m {
            if strings.EqualFold(k, k2) {
                switch n := v.(type) {
                case float64: return n, true
                case json.Number: f, err := n.Float64(); if err == nil { return f, true }
                case int:     return float64(n), true
                case int64:   return float64(n), true
                }
            }
        }
    }
    return 0, false
}

// Poller は Provider を一定間隔で呼び出し、差分を SSE へ配信します。
type Poller struct {
    Prov         Provider
    Hub          *sse.Hub
    Interval     time.Duration // 例: 2s
    Jitter       time.Duration // 0で無効（未使用: 予約）
    MovementEPS  float64       // 例: 0.01

    mu   sync.Mutex
    prev map[string]Player
}

// Run はコンテキストがキャンセルされるまでループします。
func (p *Poller) Run(ctx context.Context) error {
    if p.Prov == nil || p.Hub == nil {
        return errors.New("poller: missing Provider or Hub")
    }
    if p.Interval <= 0 { p.Interval = 2 * time.Second }
    if p.MovementEPS <= 0 { p.MovementEPS = 0.001 }
    p.mu.Lock(); if p.prev == nil { p.prev = make(map[string]Player) }; p.mu.Unlock()

    _ = p.tick(ctx)
    t := time.NewTicker(p.Interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-t.C:
            _ = p.tick(ctx)
        }
    }
}

func (p *Poller) tick(ctx context.Context) error {
    players, err := p.Prov.FetchPlayers(ctx)
    if err != nil {
        return err
    }
    now := time.Now().UTC()
    curr := make(map[string]Player, len(players))
    for _, pl := range players { curr[pl.ID] = pl }

    p.mu.Lock()
    prev := p.prev
    p.prev = curr
    p.mu.Unlock()

    for id, pl := range curr {
        if old, ok := prev[id]; ok {
            if moved(old, pl, p.MovementEPS) {
                payload := fmt.Sprintf(`{"pid":%q,"x":%g,"z":%g,"t":%q,"name":%q}`, pl.ID, pl.X, pl.Z, now.Format(time.RFC3339Nano), pl.Name)
                p.Hub.Broadcast("pos", []byte(payload))
            }
        } else {
            payload := fmt.Sprintf(`{"kind":"player_connect","pid":%q,"t":%q,"name":%q}`, pl.ID, now.Format(time.RFC3339Nano), pl.Name)
            p.Hub.Broadcast("events", []byte(payload))
            payload2 := fmt.Sprintf(`{"pid":%q,"x":%g,"z":%g,"t":%q,"name":%q}`, pl.ID, pl.X, pl.Z, now.Format(time.RFC3339Nano), pl.Name)
            p.Hub.Broadcast("pos", []byte(payload2))
        }
    }
    for id, old := range prev {
        if _, ok := curr[id]; !ok {
            payload := fmt.Sprintf(`{"kind":"player_disconnect","pid":%q,"t":%q,"name":%q}`, old.ID, now.Format(time.RFC3339Nano), old.Name)
            p.Hub.Broadcast("events", []byte(payload))
        }
    }
    return nil
}

func moved(a, b Player, eps float64) bool {
    dx := a.X - b.X; if dx < 0 { dx = -dx }
    dz := a.Z - b.Z; if dz < 0 { dz = -dz }
    return dx > eps || dz > eps
}

