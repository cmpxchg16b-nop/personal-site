#!/usr/bin/env node
// Generates web/site/src/components/satellite/landRings.ts from Natural Earth
// 110m land polygons (public domain — https://www.naturalearthdata.com).
//
// Pipeline: fetch GeoJSON (cached in scripts/.cache) -> flatten every polygon
// ring (exteriors AND holes, e.g. the Caspian Sea — the consumer fills with the
// even-odd rule, so holes work automatically) -> Douglas-Peucker simplify ->
// drop sliver rings -> quantize to 0.01 deg -> emit a TS module.
//
// Rings are cut at the antimeridian in the source data, and this pipeline never
// joins across it, so the emitted polylines are safe to rasterize in
// equirectangular lon/lat space.
//
// Usage: node scripts/build-land-rings.mjs

import { mkdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const CACHE_DIR = join(ROOT, "scripts", ".cache");
const CACHE_FILE = join(CACHE_DIR, "ne_110m_land.geojson");
const OUT_FILE = join(ROOT, "web/site/src/components/satellite/landRings.ts");
const SOURCE_URL =
  "https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_110m_land.geojson";

// Simplification tolerance in degrees (~6 km). The vector globe rasterizes at
// 0.35 deg/grid cell at full resolution, so this is far below visible error.
const TOLERANCE_DEG = 0.055;
// Rings smaller than this (square degrees, cos-lat weighted) are dropped.
const MIN_AREA_DEG2 = 0.015;

async function loadGeoJSON() {
  if (existsSync(CACHE_FILE)) {
    return JSON.parse(readFileSync(CACHE_FILE, "utf8"));
  }
  console.log(`fetching ${SOURCE_URL}`);
  const res = await fetch(SOURCE_URL);
  if (!res.ok) throw new Error(`fetch failed: ${res.status} ${res.statusText}`);
  const text = await res.text();
  mkdirSync(CACHE_DIR, { recursive: true });
  writeFileSync(CACHE_FILE, text);
  return JSON.parse(text);
}

/** Perpendicular distance of p to segment a-b, in a locally flat metric. */
function pointSegmentDistance(p, a, b, cosLat) {
  const ax = a[0] * cosLat;
  const ay = a[1];
  const bx = b[0] * cosLat;
  const by = b[1];
  const px = p[0] * cosLat;
  const py = p[1];
  const dx = bx - ax;
  const dy = by - ay;
  const len2 = dx * dx + dy * dy;
  let t = len2 === 0 ? 0 : ((px - ax) * dx + (py - ay) * dy) / len2;
  t = Math.max(0, Math.min(1, t));
  const ex = ax + t * dx - px;
  const ey = ay + t * dy - py;
  return Math.hypot(ex, ey);
}

/** Iterative Douglas-Peucker on an open polyline; returns kept point flags. */
function douglasPeucker(points, tolerance) {
  const n = points.length;
  const keep = new Uint8Array(n);
  keep[0] = keep[n - 1] = 1;
  const stack = [[0, n - 1]];
  while (stack.length > 0) {
    const [i0, i1] = stack.pop();
    if (i1 <= i0 + 1) continue;
    const a = points[i0];
    const b = points[i1];
    const cosLat = Math.cos((((a[1] + b[1]) / 2) * Math.PI) / 180);
    let maxDist = -1;
    let maxIdx = -1;
    for (let i = i0 + 1; i < i1; i++) {
      const d = pointSegmentDistance(points[i], a, b, cosLat);
      if (d > maxDist) {
        maxDist = d;
        maxIdx = i;
      }
    }
    if (maxDist > tolerance) {
      keep[maxIdx] = 1;
      stack.push([i0, maxIdx], [maxIdx, i1]);
    }
  }
  return keep;
}

/** Simplify a closed ring. The first/last point coincide; DP pins that seam. */
function simplifyRing(ring, tolerance) {
  if (ring.length < 2) return ring;
  const keep = douglasPeucker(ring, tolerance);
  const out = ring.filter((_, i) => keep[i]);
  // Keep the ring explicitly closed.
  const first = out[0];
  const last = out[out.length - 1];
  if (first[0] !== last[0] || first[1] !== last[1]) out.push(first);
  return out;
}

/** cos-lat-weighted shoelace area in square degrees. */
function ringArea(ring) {
  let sum = 0;
  let latSum = 0;
  for (let i = 0; i < ring.length - 1; i++) {
    const [x1, y1] = ring[i];
    const [x2, y2] = ring[i + 1];
    sum += x1 * y2 - x2 * y1;
    latSum += y1;
  }
  const cosLat = Math.cos(((latSum / (ring.length - 1)) * Math.PI) / 180);
  return (Math.abs(sum) / 2) * Math.max(0.05, cosLat);
}

function quantize(v) {
  return Math.round(v * 100) / 100;
}

const geojson = await loadGeoJSON();
const rings = [];
let inputPoints = 0;
for (const feature of geojson.features) {
  const { type, coordinates } = feature.geometry;
  const polygons = type === "Polygon" ? [coordinates] : coordinates; // MultiPolygon
  for (const polygon of polygons) {
    for (const ring of polygon) {
      inputPoints += ring.length;
      const simplified = simplifyRing(ring, TOLERANCE_DEG);
      if (simplified.length < 4) continue;
      if (ringArea(simplified) < MIN_AREA_DEG2) continue;
      rings.push(simplified);
    }
  }
}

const outputPoints = rings.reduce((n, r) => n + r.length, 0);
console.log(
  `rings: ${rings.length}, points: ${inputPoints} -> ${outputPoints} ` +
    `(${Math.round((outputPoints / inputPoints) * 100)}% kept)`,
);

const lines = [];
lines.push(
  "// ---------------------------------------------------------------------------",
);
lines.push("// GENERATED FILE — do not edit by hand. Rebuild with:");
lines.push("//   node scripts/build-land-rings.mjs");
lines.push("//");
lines.push(
  "// Real Earth coastlines from Natural Earth 110m land (public domain:",
);
lines.push(
  "// https://www.naturalearthdata.com). Each entry is one closed polygon ring as",
);
lines.push(
  "// flat [lon, lat, lon, lat, ...] degrees, simplified to ~0.05 deg and",
);
lines.push(
  "// quantized to 0.01 deg. Exterior rings and hole rings (e.g. the Caspian Sea)",
);
lines.push(
  "// are mixed; the consumer fills with the even-odd rule. No ring crosses the",
);
lines.push("// antimeridian (source data is cut at +/-180 deg).");
lines.push(
  "// ---------------------------------------------------------------------------",
);
lines.push("");
lines.push("export const LAND_RINGS: readonly number[][] = [");
for (const ring of rings) {
  const flat = [];
  for (const [lon, lat] of ring) {
    flat.push(quantize(lon), quantize(lat));
  }
  lines.push(`  [${flat.join(",")}],`);
}
lines.push("];");
lines.push("");

writeFileSync(OUT_FILE, lines.join("\n"));
const kb = Math.round(lines.join("\n").length / 1024);
console.log(`wrote ${OUT_FILE} (${kb} KB)`);
