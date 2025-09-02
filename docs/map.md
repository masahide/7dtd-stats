https://7dtd.illy.bz/browser/binary-improvements/webserver_legacy/js/map.js の実装を参考に
svelte をつかって実装可能でしょうか？

各マップのタイル画像は以下のパスのようにゲームプロセスから取得可能です。

```bash
curl 'http://xxx.xxx.xxx.xxx:8080/map/0/0/0.png?t=1756728782772'
```

結論：**Svelte（SvelteKit でも OK）＋ Leaflet で、あの `map.js` の挙動は再現できます。**
タイルは `http://<host>:8080/map/{z}/{x}/{y}.png?t=<timestamp>` をそのまま使えますし、Leaflet 側で 7DTD 特有の座標系（X=lat, Z=lng）と **TMS 風の Y 反転**を合わせれば動きます（僕の意見です）。根拠はオリジナルの `map.js`/`leaflet.layer.sdtdtiles.js` の設定で、**tileSize=128 / maxNativeZoom=4 / Y 反転**が肝です。([7dtd.illy.bz][1]) ([7dtd.illy.bz][2])

---

# そのまま動かせる最小 Svelte（SvelteKit）実装例

> 依存: `leaflet`（CSS も必須）
> 注意: ゲームプロセスに **CORS** が無い場合は、Go 側で `/map/{z}/{x}/{y}.png` の**プロキシ**を用意すると安心（僕の意見）。

`src/lib/Map7dtd.svelte`

```svelte
<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import L from "leaflet";
  import "leaflet/dist/leaflet.css";

  export let mapBaseUrl = "http://xxx.xxx.xxx.xxx:8080/map"; // ゲームプロセス
  export let maxzoom = 4;     // 7DTD既定
  export let tilesize = 128;  // 7DTD既定

  let mapEl: HTMLDivElement;
  let map: L.Map;

  // --- 7DTD 専用投影 & CRS（オリジナルと同じ式） ---
  const SDTD_Projection = {
    project(latlng: L.LatLng) {
      return new L.Point(
        (latlng.lat) / Math.pow(2, maxzoom),
        (latlng.lng) / Math.pow(2, maxzoom)
      );
    },
    unproject(point: L.Point) {
      return new L.LatLng(
        point.x * Math.pow(2, maxzoom),
        point.y * Math.pow(2, maxzoom)
      );
    }
  } as L.Projection;

  const SDTD_CRS: L.CRS = {
    ...(L.CRS.Simple as any),
    projection: SDTD_Projection,
    transformation: new L.Transformation(1, 0, -1, 0), // Y軸反転
    scale(zoom: number) { return Math.pow(2, zoom); }
  } as L.CRS;

  // --- 7DTD タイルレイヤ（Y反転 + キャッシュバスター） ---
  function createSdtdTileLayer(isMini = false) {
    const initTime = Date.now();
    const layer = L.tileLayer(`${mapBaseUrl}/{z}/{x}/{y}.png?t={time}`, {
      maxZoom: isMini ? maxzoom : maxzoom + 1,
      minZoom: isMini ? -1 : Math.max(0, maxzoom - 5),
      maxNativeZoom: maxzoom,
      minNativeZoom: 0,
      tileSize: tilesize,
      time: initTime
    } as any);

    // オリジナル同様に Y を TMS 方向へ反転（tms:true ではなく明示反転）
    const orig = (layer as any).getTileUrl.bind(layer);
    (layer as any).getTileUrl = (coords: any) => {
      coords.y = (-coords.y) - 1;
      return orig(coords);
    };
    return layer;
  }

  onMount(() => {
    map = L.map(mapEl, {
      crs: SDTD_CRS,
      zoomControl: true,
      zoomSnap: 1,
      zoomDelta: 1,
      wheelPxPerZoomLevel: 60
    }).setView([0, 0], Math.max(0, maxzoom - 5)); // 初期中心

    const tile = createSdtdTileLayer(false).addTo(map);
    // もしミニマップ等が必要なら createSdtdTileLayer(true) を使ってください

    // 例: 手動リロード（タイルキャッシュ破棄）
    const reloadTiles = () => {
      (tile as any).options.time = Date.now();
      tile.redraw();
    };

    // とりあえず R キーでタイルを再読込
    const onKey = (e: KeyboardEvent) => { if (e.key.toLowerCase() === "r") reloadTiles(); };
    window.addEventListener("keydown", onKey);

    onDestroy(() => {
      window.removeEventListener("keydown", onKey);
      map.remove();
    });
  });
</script>

<style>
  :global(.leaflet-container) { background: #000; }
  .map { width: 100%; height: 100%; position: absolute; inset: 0; }
</style>

<div bind:this={mapEl} class="map" />
```

SvelteKit のページから使うなら：

`src/routes/+page.svelte`

```svelte
<script>
  import Map7dtd from "$lib/Map7dtd.svelte";
</script>

<Map7dtd mapBaseUrl="http://xxx.xxx.xxx.xxx:8080/map" />
```

---

## 実装のポイント（オリジナルとの対応）

- **タイルの設定**
  `tileSize=128`, `maxNativeZoom=4` を採用。ミニマップ用は `maxZoom=maxzoom`、通常は `maxZoom=maxzoom+1` とするのが原実装の流儀です。([7dtd.illy.bz][2])
- **Y 反転（TMS）**
  元コードは `getTileUrl` をオーバーライドして **`coords.y = (-y) - 1`** の反転を行っています。Svelte 版でも同じ方式にしました（`tms:true` でもいける場合がありますが、オリジナル準拠が安全だと私は思います）。([7dtd.illy.bz][2])
- **座標系（X/Z を lat/lng に割り当て）**
  `SDTD_Projection` + `transformation(1,0,-1,0)` を使うことで、**ゲーム X を Leaflet の lat、ゲーム Z を lng**として扱う設計です。`L.marker([x,z])` で置けるのはこのため。([7dtd.illy.bz][1])
- **キャッシュバスター**
  URL クエリ `t={time}` を付けてタイルの再読込を制御。オリジナルも同様。([7dtd.illy.bz][2])

---

## よくあるハマりどころ

- **CORS/同一生成元ポリシー**：ゲームプロセスを直叩きするなら CORS を許可するか、**Go のタイルプロキシ**（`/map`）を挟むのが堅い（僕の意見）。

---

## タイルプロキシ（同じパスで転送）

このリポジトリには、`/map/...` を同じパスのまま上流（ゲームサーバー）へ転送する軽量プロキシを同梱しています。

- 実行ファイル: `cmd/server`
- ライブラリ: `pkg/mapproxy`

起動例:

```bash
go build -o server ./cmd/server
./server -listen :8081 -upstream http://xxx.xxx.xxx.xxx:8080
# 以降、http://localhost:8081/map/0/0/0.png?t=... にアクセスすると
# http://xxx.xxx.xxx.xxx:8080/map/0/0/0.png?t=... に転送されます
```

Svelte/Leaflet 側では `mapBaseUrl` を `http://localhost:8081/map` に向ければ、同一オリジンで画像が取得できます。
- **座標の並び**：Leaflet は `[lat,lng]` なので **`[x,z]`** の順を間違えない。
- **ズームの上限**：7DTD 側の `maxzoom=4` を越えても画像は粗くなるだけなので、`maxNativeZoom=4` を守る。

---

## さらに（SSE でプレイヤー軌跡を描きたい場合）

SSE で `{type:"pos", pid:"...", x:123.4, z:-56.7, t:"..."}` を流すだけで、クライアントは `L.polyline([...])` に追記すれば OK です。
（必要なら、この Svelte に SSE 受信＆ポリライン描画の雛形も足します！）

---

### 参考にした実装

- `map.js` の **投影/CRS・zoom 挙動** とマーカー座標の与え方。([7dtd.illy.bz][1])
- `leaflet.layer.sdtdtiles.js` の **タイル URL・Y 反転・tileSize/maxNativeZoom**。([7dtd.illy.bz][2])

---

ざっとここまで。僕の意見ですが、**まずはこの最小構成で「タイルが正しく見える」状態にしてから**、SSE やレイヤ制御、ミニマップ、距離計測などを足していくのが一番はやいです。

[1]: https://7dtd.illy.bz/browser/binary-improvements/webserver_legacy/js/map.js "map.js in binary-improvements/webserver_legacy/js          – 7 Days to Die - Linux Server Management"
[2]: https://7dtd.illy.bz/browser/binary-improvements/webserver_legacy/js/leaflet.layer.sdtdtiles.js "leaflet.layer.sdtdtiles.js in binary-improvements/webserver_legacy/js          – 7 Days to Die - Linux Server Management"
