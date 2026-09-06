import * as THREE from "three";
import { LAND_RINGS } from "./landRings";

// ---------------------------------------------------------------------------
// Earth surface, generated as VECTOR geometry instead of pixel maps. The land
// field is REAL geography: Natural Earth 110m coastlines (see landRings.ts)
// sampled onto an equirectangular grid as a signed field — scanline even-odd
// fill for the sign, true coast distance for nodes near sign changes — then
// polygonized with marching squares and tessellated into flat-colored triangle
// polygons (per cell the region is square ∩ half-plane, hence convex, so fan
// triangulation is exact). Continent and island outlines are therefore
// resolution-independent — crisp at any zoom or pixel ratio — with coastline
// strokes delivered as line segments on top. Ice caps are a noise-ragged
// latitude band through the same vector pipeline.
//
// Only the cloud layer remains a raster texture (soft alpha blobs suit it).
//
// createEarthSurfaceJob() streams the work: a coarse preview set is built
// synchronously (a few ms) and delivered immediately, then full-resolution
// bands are processed between frames so the main thread stays responsive.
// ---------------------------------------------------------------------------

export interface EarthSurfaceSet {
  /** Coastline (sea-level isoline) as flat xyz segment endpoints. */
  coastline: Float32Array | null;
  /** Non-indexed triangle positions for the land polygons. */
  land: Float32Array | null;
  /** Non-indexed triangle positions for the ice-cap polygons. */
  ice: Float32Array | null;
  clouds: THREE.CanvasTexture | null;
  dispose: () => void;
}

export type EarthSurfaceSink = (surface: EarthSurfaceSet) => void;

/** Incremental generator for the full-resolution surface set. */
export interface EarthSurfaceJob {
  /** True once the final set has been delivered (or the job was disposed). */
  readonly done: boolean;
  /** Process the next band of work; returns true when the job is done. */
  step(): boolean;
  /**
   * Cancel the job. Sets already delivered through the sink are owned by the
   * receiver — the job itself never disposes them.
   */
  dispose(): void;
}

const PREVIEW_WIDTH = 256;

// Layer stacking radii (Earth = 1): ocean sphere, then polygon layers, then
// coastline strokes, then clouds, then the atmosphere shell (1.03).
const LAND_RADIUS = 1.0006;
const ICE_RADIUS = 1.0012;
const COASTLINE_RADIUS = 1.0018;

const MAX_TRIANGLE_FLOATS = 24_000_000;

// --- Seeded PRNG + 3D value noise ------------------------------------------

/** Hash a lattice corner to a value in [-1, 1). */
function hash3(ix: number, iy: number, iz: number, seed: number): number {
  let h =
    Math.imul(ix, 374761393) ^
    Math.imul(iy, 668265263) ^
    Math.imul(iz, 1440662683) ^
    Math.imul(seed, 1013904223);
  h = Math.imul(h ^ (h >>> 13), 1274126177);
  h ^= h >>> 16;
  return (h >>> 0) / 2147483648 - 1;
}

function smootherstep(t: number): number {
  return t * t * t * (t * (t * 6 - 15) + 10);
}

function valueNoise3(x: number, y: number, z: number, seed: number): number {
  const ix = Math.floor(x);
  const iy = Math.floor(y);
  const iz = Math.floor(z);
  const fx = smootherstep(x - ix);
  const fy = smootherstep(y - iy);
  const fz = smootherstep(z - iz);

  const c000 = hash3(ix, iy, iz, seed);
  const c100 = hash3(ix + 1, iy, iz, seed);
  const c010 = hash3(ix, iy + 1, iz, seed);
  const c110 = hash3(ix + 1, iy + 1, iz, seed);
  const c001 = hash3(ix, iy, iz + 1, seed);
  const c101 = hash3(ix + 1, iy, iz + 1, seed);
  const c011 = hash3(ix, iy + 1, iz + 1, seed);
  const c111 = hash3(ix + 1, iy + 1, iz + 1, seed);

  const x00 = c000 + (c100 - c000) * fx;
  const x10 = c010 + (c110 - c010) * fx;
  const x01 = c001 + (c101 - c001) * fx;
  const x11 = c011 + (c111 - c011) * fx;
  const y0 = x00 + (x10 - x00) * fy;
  const y1 = x01 + (x11 - x01) * fy;
  return y0 + (y1 - y0) * fz;
}

/** Fractal Brownian motion of the value noise, normalized to roughly [-1, 1]. */
function fbm3(
  x: number,
  y: number,
  z: number,
  octaves: number,
  seed: number,
  lacunarity = 2.1,
  gain = 0.45,
): number {
  let sum = 0;
  let amp = 1;
  let norm = 0;
  let freq = 1;
  for (let o = 0; o < octaves; o++) {
    sum += amp * valueNoise3(x * freq, y * freq, z * freq, seed + o * 101);
    norm += amp;
    amp *= gain;
    freq *= lacunarity;
  }
  return sum / norm;
}

function clamp01(v: number): number {
  return v < 0 ? 0 : v > 1 ? 1 : v;
}

function smoothstep(edge0: number, edge1: number, v: number): number {
  const t = clamp01((v - edge0) / (edge1 - edge0));
  return t * t * (3 - 2 * t);
}

// --- Sphere mapping ----------------------------------------------------------

/** Convert pixel coordinates to the unit-sphere direction they texture (Y-up, row 0 = north pole); returns |latitude| in degrees. */
function pixelDirection(
  x: number,
  y: number,
  w: number,
  h: number,
  out: [number, number, number],
): number {
  const u = (x + 0.5) / w;
  const v = (y + 0.5) / h;
  const lon = u * Math.PI * 2;
  const lat = (0.5 - v) * Math.PI;
  const cosLat = Math.cos(lat);
  out[0] = cosLat * Math.sin(lon);
  out[1] = Math.sin(lat);
  out[2] = cosLat * Math.cos(lon);
  return Math.abs((lat * 180) / Math.PI);
}

/** Map a continuous grid coordinate onto the sphere at `radius`, writing 3 floats at `at`. */
function gridToSphereInto(
  cx: number,
  cy: number,
  w: number,
  h: number,
  radius: number,
  target: Float32Array,
  at: number,
): void {
  const u = (cx + 0.5) / w;
  const v = (cy + 0.5) / h;
  const lon = u * Math.PI * 2;
  const lat = (0.5 - v) * Math.PI;
  const cosLat = Math.cos(lat);
  target[at] = radius * cosLat * Math.sin(lon);
  target[at + 1] = radius * Math.sin(lat);
  target[at + 2] = radius * cosLat * Math.cos(lon);
}

function pushSpherePoint(
  cx: number,
  cy: number,
  w: number,
  h: number,
  radius: number,
  out: number[],
): void {
  const u = (cx + 0.5) / w;
  const v = (cy + 0.5) / h;
  const lon = u * Math.PI * 2;
  const lat = (0.5 - v) * Math.PI;
  const cosLat = Math.cos(lat);
  out.push(
    radius * cosLat * Math.sin(lon),
    radius * Math.sin(lat),
    radius * cosLat * Math.cos(lon),
  );
}

// --- Real coastline data -----------------------------------------------------
//
// The land field samples the Natural Earth coastline rings (LAND_RINGS) onto
// the equirectangular node grid:
// - SIGN: scanline even-odd fill per node row. Hole rings (e.g. the Caspian
//   Sea) fall out of the parity for free, and since the source data is cut at
//   the antimeridian no edge needs longitude wrapping.
// - MAGNITUDE: marching squares interpolates each coastline crossing from the
//   two endpoint VALUES of a grid edge, so nodes touched by a sign change get
//   their true distance to the nearest coastline segment (the interpolation is
//   exact wherever the coast is locally straight, which it is between
//   vertices). All other nodes keep magnitude 1 — only their sign matters.

/** Flattened ring edges as [lon1, lat1, lon2, lat2] quadruples. */
const LAND_EDGES: Float32Array = (() => {
  let count = 0;
  for (const ring of LAND_RINGS) count += ring.length / 2 - 1;
  const edges = new Float32Array(count * 4);
  let e = 0;
  for (const ring of LAND_RINGS) {
    for (let i = 0; i + 3 < ring.length; i += 2) {
      edges[e++] = ring[i];
      edges[e++] = ring[i + 1];
      edges[e++] = ring[i + 2];
      edges[e++] = ring[i + 3];
    }
  }
  return edges;
})();

// Segment index for distance queries: edge indices bucketed over a 2°×2°
// lon/lat grid.
const BUCKET_DEG = 2;
const LON_BUCKETS = 360 / BUCKET_DEG;
const LAT_BUCKETS = 180 / BUCKET_DEG;
const LAND_BUCKETS: Map<number, number[]> = (() => {
  const buckets = new Map<number, number[]>();
  for (let e = 0; e < LAND_EDGES.length / 4; e++) {
    const lon1 = LAND_EDGES[e * 4];
    const lat1 = LAND_EDGES[e * 4 + 1];
    const lon2 = LAND_EDGES[e * 4 + 2];
    const lat2 = LAND_EDGES[e * 4 + 3];
    const by0 = Math.max(
      0,
      Math.floor((Math.min(lat1, lat2) + 90) / BUCKET_DEG),
    );
    const by1 = Math.min(
      LAT_BUCKETS - 1,
      Math.floor((Math.max(lat1, lat2) + 90) / BUCKET_DEG),
    );
    const bx0 = Math.max(
      0,
      Math.floor((Math.min(lon1, lon2) + 180) / BUCKET_DEG),
    );
    const bx1 = Math.min(
      LON_BUCKETS - 1,
      Math.floor((Math.max(lon1, lon2) + 180) / BUCKET_DEG),
    );
    for (let by = by0; by <= by1; by++) {
      for (let bx = bx0; bx <= bx1; bx++) {
        const key = by * LON_BUCKETS + bx;
        const list = buckets.get(key);
        if (list) list.push(e);
        else buckets.set(key, [e]);
      }
    }
  }
  return buckets;
})();

/**
 * Distance from (lon, lat) to the nearest coastline segment, in a locally flat
 * metric (longitude scaled by cos(latitude)), in degrees. Only called for
 * coast-adjacent nodes, where the nearest segment is always within a couple of
 * grid cells; the bucket window expands to stay correct on tiny grids.
 */
function coastDistance(lon: number, lat: number): number {
  const cosLat = Math.cos((lat * Math.PI) / 180);
  const bx = Math.floor((lon + 180) / BUCKET_DEG);
  const by = Math.floor((lat + 90) / BUCKET_DEG);
  for (const reach of [3, 9, 27]) {
    let best = Infinity;
    for (let dy = -reach; dy <= reach; dy++) {
      const y = by + dy;
      if (y < 0 || y >= LAT_BUCKETS) continue;
      for (let dx = -reach; dx <= reach; dx++) {
        let x = (bx + dx) % LON_BUCKETS;
        if (x < 0) x += LON_BUCKETS;
        const list = LAND_BUCKETS.get(y * LON_BUCKETS + x);
        if (!list) continue;
        for (const e of list) {
          let ax = LAND_EDGES[e * 4] - lon;
          ax -= Math.round(ax / 360) * 360;
          ax *= cosLat;
          const ay = LAND_EDGES[e * 4 + 1] - lat;
          let bx2 = LAND_EDGES[e * 4 + 2] - lon;
          bx2 -= Math.round(bx2 / 360) * 360;
          bx2 *= cosLat;
          const by2 = LAND_EDGES[e * 4 + 3] - lat;
          const sx = bx2 - ax;
          const sy = by2 - ay;
          const len2 = sx * sx + sy * sy;
          let t = len2 === 0 ? 0 : -(ax * sx + ay * sy) / len2;
          t = t < 0 ? 0 : t > 1 ? 1 : t;
          const ex = ax + t * sx;
          const ey = ay + t * sy;
          const d = Math.sqrt(ex * ex + ey * ey);
          if (d < best) best = d;
        }
      }
    }
    if (best < Infinity) return best;
  }
  return 108; // no coast within 54 buckets — deep ocean node on a tiny grid
}

// Scratch crossing list for the scanline (single-threaded generator).
const rowCrossings: number[] = [];

/** Fill the whole land field from the coastline data (see the section comment). */
function rasterizeLandField(p: SurfacePlanes): void {
  const { width: w, height: h, land, landInside } = p;

  // Pass 1 — sign per node: even-odd fill of the edge crossings along the
  // row's latitude line.
  for (let y = 0; y < h; y++) {
    const lat = (0.5 - (y + 0.5) / h) * 180;
    rowCrossings.length = 0;
    for (let e = 0; e < LAND_EDGES.length; e += 4) {
      const lat1 = LAND_EDGES[e + 1];
      const lat2 = LAND_EDGES[e + 3];
      if (lat1 > lat !== lat2 > lat) {
        rowCrossings.push(
          LAND_EDGES[e] +
            ((LAND_EDGES[e + 2] - LAND_EDGES[e]) * (lat - lat1)) /
              (lat2 - lat1),
        );
      }
    }
    rowCrossings.sort((a, b) => a - b);
    let c = 0;
    const n = rowCrossings.length;
    const rowBase = y * w;
    for (let x = 0; x < w; x++) {
      const lon = ((x + 0.5) / w) * 360 - 180;
      while (c < n && rowCrossings[c] <= lon) c++;
      const isLand = (c & 1) === 1;
      landInside[rowBase + x] = isLand ? 1 : 0;
      land[rowBase + x] = isLand ? 1 : -1;
    }
  }

  // Pass 2 — magnitude for coast-adjacent nodes (a node whose 4-neighborhood
  // — longitude wraps — contains a sign change).
  for (let y = 0; y < h; y++) {
    const lat = (0.5 - (y + 0.5) / h) * 180;
    const rowBase = y * w;
    const upBase = Math.max(0, y - 1) * w;
    const downBase = Math.min(h - 1, y + 1) * w;
    for (let x = 0; x < w; x++) {
      const s = landInside[rowBase + x];
      if (
        landInside[rowBase + ((x + w - 1) % w)] === s &&
        landInside[rowBase + ((x + 1) % w)] === s &&
        landInside[upBase + x] === s &&
        landInside[downBase + x] === s
      ) {
        continue;
      }
      const lon = ((x + 0.5) / w) * 360 - 180;
      const d = Math.max(coastDistance(lon, lat), 1e-3);
      land[rowBase + x] = s === 1 ? d : -d;
    }
  }
}

// --- Scalar fields -----------------------------------------------------------

/** Evaluate the ice-cap field at one grid node (zero-isoline = ice boundary). */
function iceField(
  dx: number,
  dy: number,
  dz: number,
  absLatDeg: number,
  seed: number,
): number {
  // Polar caps beyond ~75°, ragged by a little low-frequency noise.
  // Positive INSIDE the cap.
  const wobble = fbm3(
    dx * 2.3 + 5.1,
    dy * 2.3 - 9.7,
    dz * 2.3 + 2.9,
    2,
    seed + 913,
  );
  return absLatDeg - (75 + 3.5 * wobble);
}

// --- Marching squares: polygons + coastline ----------------------------------

// Cyclic polygon vertex lists per marching-squares case, as vertex ids:
// 0=TL 1=TR 2=BR 3=BL corners, 4=T 5=R 6=B 7=L edge crossings.
// Per cell the region is square ∩ half-plane(s), i.e. convex, so a triangle
// fan from vertex 0 tessellates it exactly.
const CASE_POLYS: (number[] | null)[] = [
  null, // 0
  [4, 0, 7], // 1
  [5, 1, 4], // 2
  [7, 0, 1, 5], // 3
  [6, 2, 5], // 4
  null, // 5  (saddle)
  [4, 1, 2, 6], // 6
  [7, 0, 1, 2, 6], // 7
  [7, 3, 6], // 8
  [6, 3, 0, 4], // 9
  null, // 10 (saddle)
  [6, 3, 0, 1, 5], // 11
  [5, 2, 3, 7], // 12
  [4, 0, 3, 2, 5], // 13
  [7, 3, 2, 1, 4], // 14
  null, // 15
];
const SADDLE_5 = {
  high: [[4, 5, 2, 6, 7, 0]],
  low: [
    [4, 0, 7],
    [5, 2, 6],
  ],
};
const SADDLE_10 = {
  high: [[4, 1, 5, 6, 3, 7]],
  low: [
    [4, 1, 5],
    [6, 3, 7],
  ],
};

// Coastline segment endpoint pairs (crossing ids) per case.
const CASE_SEGMENTS: (number[] | null)[] = [
  null,
  [7, 4],
  [4, 5],
  [7, 5],
  [5, 6],
  null,
  [4, 6],
  [7, 6],
  [7, 6],
  [4, 6],
  null,
  [5, 6],
  [7, 5],
  [4, 5],
  [7, 4],
  null,
];

interface CellCase {
  idx: number;
  /** Crossing x/y for T,R,B,L. */
  px: [number, number, number, number];
  py: [number, number, number, number];
}

const scratchCell: CellCase = {
  idx: 0,
  px: [0, 0, 0, 0],
  py: [0, 0, 0, 0],
};

function cellCase(
  field: Float32Array,
  w: number,
  x0: number,
  y0: number,
  x1: number,
  y1: number,
  stride: number,
): CellCase | null {
  const i00 = y0 * w + (x0 % w);
  const i10 = y0 * w + (x1 % w);
  const i11 = y1 * w + (x1 % w);
  const i01 = y1 * w + (x0 % w);
  const v0 = field[i00];
  const v1 = field[i10];
  const v2 = field[i11];
  const v3 = field[i01];
  const idx =
    (v0 > 0 ? 1 : 0) | (v1 > 0 ? 2 : 0) | (v2 > 0 ? 4 : 0) | (v3 > 0 ? 8 : 0);
  if (idx === 0) return null;
  if (idx === 15) {
    // Fully inside the region: the whole cell is polygon, no edge crossings.
    scratchCell.idx = 15;
    return scratchCell;
  }

  const c = scratchCell;
  c.idx = idx;
  c.px[0] = x0 + clamp01(-v0 / (v1 - v0)) * stride;
  c.py[0] = y0;
  c.px[1] = x1;
  c.py[1] = y0 + clamp01(-v1 / (v2 - v1)) * stride;
  c.px[2] = x0 + clamp01(-v3 / (v2 - v3)) * stride;
  c.py[2] = y1;
  c.px[3] = x0;
  c.py[3] = y0 + clamp01(-v0 / (v3 - v0)) * stride;
  return c;
}

/** Vertex id -> grid coordinate within a cell case. */
function vertexCoord(
  id: number,
  c: CellCase,
  x0: number,
  y0: number,
  x1: number,
  y1: number,
  out: [number, number],
): void {
  switch (id) {
    case 0:
      out[0] = x0;
      out[1] = y0;
      break;
    case 1:
      out[0] = x1;
      out[1] = y0;
      break;
    case 2:
      out[0] = x1;
      out[1] = y1;
      break;
    case 3:
      out[0] = x0;
      out[1] = y1;
      break;
    default:
      out[0] = c.px[id - 4];
      out[1] = c.py[id - 4];
      break;
  }
}

// Scratch for polygon -> sphere mapping (max polygon = 6 vertices).
const polyTmp = new Float32Array(18);
const coordTmp: [number, number] = [0, 0];

/** Fan-triangulate one convex polygon of a cell case into `out` (9 floats/tri). */
function pushPoly(
  poly: number[],
  c: CellCase,
  x0: number,
  y0: number,
  x1: number,
  y1: number,
  w: number,
  h: number,
  radius: number,
  out: number[],
): void {
  const n = poly.length;
  for (let i = 0; i < n; i++) {
    vertexCoord(poly[i], c, x0, y0, x1, y1, coordTmp);
    gridToSphereInto(coordTmp[0], coordTmp[1], w, h, radius, polyTmp, i * 3);
  }
  for (let k = 1; k + 1 < n; k++) {
    const a = 0;
    const b = k;
    const d = k + 1;
    const ax = polyTmp[a * 3];
    const ay = polyTmp[a * 3 + 1];
    const az = polyTmp[a * 3 + 2];
    let bx = polyTmp[b * 3];
    let by = polyTmp[b * 3 + 1];
    let bz = polyTmp[b * 3 + 2];
    let cx = polyTmp[d * 3];
    let cy = polyTmp[d * 3 + 1];
    let cz = polyTmp[d * 3 + 2];
    // Orient the triangle outward (away from the sphere center).
    const e1x = bx - ax;
    const e1y = by - ay;
    const e1z = bz - az;
    const e2x = cx - ax;
    const e2y = cy - ay;
    const e2z = cz - az;
    const nx = e1y * e2z - e1z * e2y;
    const ny = e1z * e2x - e1x * e2z;
    const nz = e1x * e2y - e1y * e2x;
    if (nx * (ax + bx + cx) + ny * (ay + by + cy) + nz * (az + bz + cz) < 0) {
      cx = bx;
      cy = by;
      cz = bz;
      bx = polyTmp[d * 3];
      by = polyTmp[d * 3 + 1];
      bz = polyTmp[d * 3 + 2];
    }
    out.push(ax, ay, az, bx, by, bz, cx, cy, cz);
  }
}

function pushPolygons(
  field: Float32Array,
  c: CellCase,
  x0: number,
  y0: number,
  x1: number,
  y1: number,
  w: number,
  h: number,
  radius: number,
  out: number[],
): void {
  if (c.idx === 15) {
    // Solid cell: emit the full quad as two triangles.
    pushPoly([0, 1, 2, 3], c, x0, y0, x1, y1, w, h, radius, out);
    return;
  }
  let polys: number[][];
  if (c.idx === 5) {
    polys =
      fieldAtCenter(field, w, x0, y0, x1, y1) > 0
        ? SADDLE_5.high
        : SADDLE_5.low;
  } else if (c.idx === 10) {
    polys =
      fieldAtCenter(field, w, x0, y0, x1, y1) > 0
        ? SADDLE_10.high
        : SADDLE_10.low;
  } else {
    const poly = CASE_POLYS[c.idx];
    if (!poly) return;
    polys = [poly];
  }
  for (const poly of polys)
    pushPoly(poly, c, x0, y0, x1, y1, w, h, radius, out);
}

function fieldAtCenter(
  field: Float32Array,
  w: number,
  x0: number,
  y0: number,
  x1: number,
  y1: number,
): number {
  return (
    field[y0 * w + (x0 % w)] +
    field[y0 * w + (x1 % w)] +
    field[y1 * w + (x1 % w)] +
    field[y1 * w + (x0 % w)]
  );
}

// --- Surface planes (incremental) --------------------------------------------

interface SurfacePlanes {
  width: number;
  height: number;
  land: Float32Array;
  ice: Float32Array;
  /** Inside/outside bitmask backing the land field (1 = land). */
  landInside: Uint8Array;
  /** Set once the real-coastline land field has been rasterized. */
  landReady: boolean;

  coastline: number[];
  landTris: number[];
  iceTris: number[];

  cloudCanvas: HTMLCanvasElement | null;
  cloudCtx: CanvasRenderingContext2D | null;
  cloudData: ImageData | null;

  result: EarthSurfaceSet | null;
}

function makeCanvas(width: number, height: number): HTMLCanvasElement {
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  return canvas;
}

function makeContext(canvas: HTMLCanvasElement): CanvasRenderingContext2D {
  const ctx = canvas.getContext("2d");
  if (!ctx)
    throw new Error("satellite background: 2D canvas context unavailable");
  return ctx;
}

function makeTexture(
  canvas: HTMLCanvasElement,
  maxAnisotropy: number,
): THREE.CanvasTexture {
  const texture = new THREE.CanvasTexture(canvas);
  texture.wrapS = THREE.RepeatWrapping;
  texture.wrapT = THREE.ClampToEdgeWrapping;
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.anisotropy = maxAnisotropy;
  texture.needsUpdate = true;
  return texture;
}

function createPlanes(
  w: number,
  h: number,
  withClouds: boolean,
): SurfacePlanes {
  const cloudCanvas = withClouds ? makeCanvas(w, h) : null;
  const cloudCtx = cloudCanvas ? makeContext(cloudCanvas) : null;
  return {
    width: w,
    height: h,
    land: new Float32Array(w * h),
    ice: new Float32Array(w * h),
    landInside: new Uint8Array(w * h),
    landReady: false,
    coastline: [],
    landTris: [],
    iceTris: [],
    cloudCanvas,
    cloudCtx,
    cloudData: cloudCtx ? cloudCtx.createImageData(w, h) : null,
    result: null,
  };
}

const nodeDir: [number, number, number] = [0, 0, 0];

/** Coastline segment endpoint pairs (crossing ids) for a marching-squares case. */
function coastSegments(idx: number, centerHigh: boolean): number[][] | null {
  if (idx === 5) {
    return centerHigh
      ? [
          [4, 5],
          [6, 7],
        ]
      : [
          [7, 4],
          [5, 6],
        ];
  }
  if (idx === 10) {
    return centerHigh
      ? [
          [7, 4],
          [5, 6],
        ]
      : [
          [4, 5],
          [7, 6],
        ];
  }
  const pair = CASE_SEGMENTS[idx];
  return pair ? [pair] : null;
}

/** Push the boundary stroke segments of a cell case onto the stroke list. */
function pushCoastSegments(
  field: Float32Array,
  c: CellCase,
  x0: number,
  y0: number,
  x1: number,
  y1: number,
  w: number,
  h: number,
  out: number[],
): void {
  if (c.idx === 15) return;
  const segs = coastSegments(
    c.idx,
    fieldAtCenter(field, w, x0, y0, x1, y1) > 0,
  );
  if (!segs) return;
  for (const pair of segs) {
    pushSpherePoint(
      c.px[pair[0] - 4],
      c.py[pair[0] - 4],
      w,
      h,
      COASTLINE_RADIUS,
      out,
    );
    pushSpherePoint(
      c.px[pair[1] - 4],
      c.py[pair[1] - 4],
      w,
      h,
      COASTLINE_RADIUS,
      out,
    );
  }
}

/** Sample field nodes for rows [y0, y1] and polygonize the cell rows between. */
function fillSurfaceBand(
  p: SurfacePlanes,
  seed: number,
  y0: number,
  y1: number,
): void {
  if (!p.landReady) {
    // The land field covers the whole grid in one pass (a few ms — far cheaper
    // than the noise field it replaces) and must be complete before any cell
    // of any band is polygonized.
    rasterizeLandField(p);
    p.landReady = true;
  }

  const { width: w, height: h } = p;
  const nodeLast = Math.min(h - 1, y1);
  for (let y = y0; y <= nodeLast; y++) {
    for (let x = 0; x < w; x++) {
      const absLatDeg = pixelDirection(x, y, w, h, nodeDir);
      p.ice[y * w + x] = iceField(
        nodeDir[0],
        nodeDir[1],
        nodeDir[2],
        absLatDeg,
        seed,
      );
    }
  }

  const cellLast = Math.min(h - 2, y1 - 1);
  for (let y = y0; y <= cellLast; y++) {
    for (let x = 0; x < w; x++) {
      const x1 = x + 1;
      const y1c = y + 1;

      const landCase = cellCase(p.land, w, x, y, x1, y1c, 1);
      if (landCase) {
        // Coastline strokes (vector outline of the land/sea boundary).
        pushCoastSegments(p.land, landCase, x, y, x1, y1c, w, h, p.coastline);
        if (p.landTris.length < MAX_TRIANGLE_FLOATS) {
          pushPolygons(
            p.land,
            landCase,
            x,
            y,
            x1,
            y1c,
            w,
            h,
            LAND_RADIUS,
            p.landTris,
          );
        }
      }

      const iceCase = cellCase(p.ice, w, x, y, x1, y1c, 1);
      if (iceCase) {
        // Ice caps get the same vector outline treatment as coastlines.
        pushCoastSegments(p.ice, iceCase, x, y, x1, y1c, w, h, p.coastline);
        if (p.iceTris.length < MAX_TRIANGLE_FLOATS) {
          pushPolygons(
            p.ice,
            iceCase,
            x,
            y,
            x1,
            y1c,
            w,
            h,
            ICE_RADIUS,
            p.iceTris,
          );
        }
      }
    }
  }
}

/** Fill cloud rows [y0, y1). */
function fillCloudBand(
  p: SurfacePlanes,
  seed: number,
  y0: number,
  y1: number,
): void {
  if (!p.cloudCtx || !p.cloudData) return;
  const data = p.cloudData.data;
  const { width: w } = p;
  for (let y = y0; y < y1; y++) {
    for (let x = 0; x < w; x++) {
      const absLatDeg = pixelDirection(x, y, w, p.height, nodeDir);
      const [dx, dy, dz] = nodeDir;
      const coverage = fbm3(
        dx * 2.6 + 3.1,
        dy * 2.6 - 7.2,
        dz * 2.6 + 1.9,
        4,
        seed + 555,
      );
      const detail =
        0.25 *
        fbm3(dx * 7.5 - 13.3, dy * 7.5 + 2.7, dz * 7.5 + 9.4, 2, seed + 777);
      // Latitude structure: ITCZ at the equator, storm belts near +/-52 deg,
      // dry subsidence near +/-25 deg.
      const band =
        0.2 * Math.exp(-((absLatDeg / 9) ** 2)) +
        0.16 * Math.exp(-(((absLatDeg - 52) / 13) ** 2)) -
        0.14 * Math.exp(-(((absLatDeg - 25) / 11) ** 2));
      const alpha = smoothstep(0.3, 0.8, coverage + detail + band);
      const i4 = (y * w + x) * 4;
      data[i4] = 255;
      data[i4 + 1] = 255;
      data[i4 + 2] = 255;
      data[i4 + 3] = Math.round(alpha * 170);
    }
  }
  p.cloudCtx.putImageData(p.cloudData, 0, 0, 0, y0, w, y1 - y0);
}

function assembleSurface(
  p: SurfacePlanes,
  maxAnisotropy: number,
): EarthSurfaceSet {
  const clouds = p.cloudCanvas
    ? makeTexture(p.cloudCanvas, maxAnisotropy)
    : null;
  const set: EarthSurfaceSet = {
    coastline: p.coastline.length >= 6 ? new Float32Array(p.coastline) : null,
    land: p.landTris.length >= 9 ? new Float32Array(p.landTris) : null,
    ice: p.iceTris.length >= 9 ? new Float32Array(p.iceTris) : null,
    clouds,
    dispose() {
      clouds?.dispose();
    },
  };
  return set;
}

// --- Public API ----------------------------------------------------------------

/**
 * Generate a complete surface set in one synchronous pass. Blocks the main
 * thread proportionally to width^2 — used for the small coarse preview; for
 * full-resolution sets prefer createEarthSurfaceJob().
 */
export function generateEarthSurface(
  seed: number,
  width: number,
  withClouds: boolean,
  maxAnisotropy = 4,
): EarthSurfaceSet {
  const w = Math.max(64, Math.round(width));
  const h = Math.max(32, Math.round(w / 2));
  const planes = createPlanes(w, h, withClouds);
  fillSurfaceBand(planes, seed, 0, h - 1);
  fillCloudBand(planes, seed, 0, h);
  return assembleSurface(planes, maxAnisotropy);
}

/**
 * Create an incremental surface job:
 * 1. a coarse preview set is generated synchronously and delivered through
 *    `onSurface` immediately (safe to call during scene setup),
 * 2. each step() then processes a small band of the full-resolution fields,
 * 3. the final step delivers the full-resolution set through `onSurface`.
 *
 * Every delivered EarthSurfaceSet is owned by the receiver: dispose superseded
 * previews when a new set arrives, and the latest set on teardown. The job
 * only cancels pending work; it never disposes delivered sets.
 */
export function createEarthSurfaceJob(
  seed: number,
  width: number,
  withClouds: boolean,
  maxAnisotropy: number,
  onSurface: EarthSurfaceSink,
): EarthSurfaceJob {
  const w = Math.max(64, Math.round(width));
  const h = Math.max(32, Math.round(w / 2));

  // Small targets are cheap enough to generate in one synchronous pass.
  if (w <= 512) {
    onSurface(generateEarthSurface(seed, w, withClouds, maxAnisotropy));
    return {
      done: true,
      step: () => true,
      dispose: () => {},
    };
  }

  onSurface(
    generateEarthSurface(
      seed,
      Math.min(PREVIEW_WIDTH, w),
      withClouds,
      maxAnisotropy,
    ),
  );

  const planes = createPlanes(w, h, withClouds);
  // ~32k nodes of noise per chunk keeps a step around one to two frames.
  const rowsPerChunk = Math.max(4, Math.round(32768 / w));
  let surfaceRow = 0;
  let cloudRow = 0;
  let done = false;
  let disposed = false;

  return {
    get done() {
      return done;
    },
    step() {
      if (disposed || done) return true;
      if (surfaceRow < h - 1) {
        const y1 = Math.min(h - 1, surfaceRow + rowsPerChunk);
        fillSurfaceBand(planes, seed, surfaceRow, y1);
        surfaceRow = y1;
        return false;
      }
      if (planes.cloudData && cloudRow < h) {
        const y1 = Math.min(h, cloudRow + rowsPerChunk);
        fillCloudBand(planes, seed, cloudRow, y1);
        cloudRow = y1;
        return false;
      }
      done = true;
      onSurface(assembleSurface(planes, maxAnisotropy));
      return true;
    },
    dispose() {
      disposed = true;
    },
  };
}
