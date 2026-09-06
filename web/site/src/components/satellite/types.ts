import type { CSSProperties } from "react";

// ---------------------------------------------------------------------------
// World conventions (three.js, Y-up):
// - Earth is a unit sphere centered at the origin (1 world unit = 1 Earth
//   radius).
// - Earth's spin axis is world +Y before the configurable axial tilt is
//   applied.
// - The reference orbit lies in the XZ plane with its normal along +Y; the
//   RAAN and inclination rotate/tilt that plane, composed as a single
//   orbit-plane quaternion (see trajectory.ts).
// ---------------------------------------------------------------------------

/**
 * Keplerian elements describing the satellite's path around Earth. The
 * satellite position at any simulation time is a pure function of these
 * parameters plus the elapsed time.
 */
export interface TrajectoryConfig {
  /**
   * Semi-major axis in Earth radii. Clamped to >= 1.06 so the camera stays
   * outside the atmosphere shell (r = 1.03). Low Earth orbit territory is
   * ~1.05-1.3 (a few hundred to ~2000 km altitude); the default flies there.
   */
  radius: number;
  /** Orbit eccentricity, clamped to [0, 0.9). 0 renders a circular orbit. */
  eccentricity: number;
  /** Tilt of the orbit plane off the equator, degrees. */
  inclinationDeg: number;
  /** Rotation of the orbit plane about Earth's spin axis, degrees. */
  raanDeg: number;
  /** Simulated seconds per full revolution. */
  periodSeconds: number;
  /** Mean anomaly at t = 0, degrees. */
  phaseDeg: number;
  /** +1 travels right-handed about the orbit normal, -1 reverses. */
  direction: 1 | -1;
}

/**
 * Reference "up" used to resolve the satellite's attitude about its line of
 * sight:
 * - "prograde": along the velocity vector (ground tracks flow "down" the frame)
 * - "orbit-normal": along the orbit plane normal
 * - "world-north": along Earth's spin axis
 * - "radial": away from Earth's center
 */
export type AttitudeUp = "prograde" | "orbit-normal" | "world-north" | "radial";

/**
 * How the satellite — and therefore the camera — is oriented each frame.
 * The resulting attitude is a quaternion; transitions between configs are
 * damped with a spherical lerp (see attitudeResponsiveness).
 * - "nadir": look straight down at Earth's center (classic overflight view).
 * - "prograde": look along the velocity vector toward the horizon.
 * - "fixed": inertial hold of an explicit quaternion [x, y, z, w].
 */
export type OrientationConfig =
  | { mode: "nadir"; up?: AttitudeUp }
  | { mode: "prograde"; up?: AttitudeUp }
  | { mode: "fixed"; quaternion: [number, number, number, number] };

export interface CameraConfig {
  /** Vertical field of view, degrees. */
  fovDeg: number;
  /**
   * Deviation of the camera's line of sight from the satellite attitude, in
   * the satellite's local frame, degrees: pitch + looks up away from the
   * attitude's line of sight, yaw + looks left, roll + rolls
   * counter-clockwise. Composed as a quaternion and multiplied onto the
   * satellite attitude. The camera's POSITION always stays on the trajectory;
   * only its orientation is adjusted here.
   *
   * With the default nadir attitude the attitude's line of sight is Earth's
   * center; from low orbit that fills the whole frame with ground. Pitching
   * up raises the line of sight toward the horizon so Earth's limb arcs
   * across the frame — land and sea below, deep space above. The default
   * pitch places the horizon just below frame center.
   */
  offsetDeg: { pitch: number; yaw: number; roll: number };
}

export interface EarthConfig {
  /** Tilt of Earth's spin axis away from world +Y, degrees (applied about world Z as a quaternion). */
  axialTiltDeg: number;
  /** Simulated seconds per full self-rotation. 0 disables self-spin, leaving all apparent motion to the satellite's flight. */
  rotationPeriodSeconds: number;
  /** Render a translucent cloud layer just above the surface. */
  clouds: boolean;
  /** Cloud-layer spin as a multiple of the surface spin rate (1 = locked to the surface). */
  cloudSpeedFactor: number;
  /** Render the thin additive fresnel atmosphere glow at the limb. */
  atmosphere: boolean;
  /**
   * Seed for the procedural parts of the surface (cloud shapes, ice-cap
   * raggedness). Continents are NOT procedural: they come from real Natural
   * Earth coastline data (see earthTexture.ts) and never change.
   */
  textureSeed: number;
  /**
   * Longitude grid width for the vector surface tessellation and the cloud
   * texture (height = width / 2). Higher = more detailed coastlines but more
   * polygons; widths above 512 stream in incrementally between frames with an
   * immediate coarse preview, so large values don't block first paint.
   */
  textureWidth: number;
}

export interface LightingConfig {
  /** Direction the sun shines FROM (does not need to be normalized). */
  sunDirection: [number, number, number];
  sunColor: string;
  sunIntensity: number;
  ambientColor: string;
  ambientIntensity: number;
}

/**
 * All scene colors in one place, so the dark and bright schemes can diverge
 * structurally — not just in hue. The bright scheme drops layers that only
 * read on a dark backdrop: deep space goes transparent (the page's own
 * background shows through), continents become stroked outlines over a light
 * grey fill, and the white-on-white layers (ice fills, clouds) drop out.
 */
export interface ScenePalette {
  /** Deep-space backdrop color; null = transparent canvas. */
  background: string | null;
  /** Ocean sphere color (sun-lit, so it shades toward the night side). */
  ocean: string;
  /** Ocean specular-highlight tint. */
  oceanSpecular: string;
  /** Land polygon fill color; null = stroked outlines only, no fill. */
  land: string | null;
  /** Ice-cap polygon fill color; null = stroked outlines only, no fill. */
  ice: string | null;
  /** Coastline and ice-boundary stroke color. */
  coastline: string;
  /** Cloud-wisp color; null hides the cloud layer. */
  clouds: string | null;
  /** Atmosphere limb-glow color; null hides the glow. */
  atmosphere: string | null;
  /** "r, g, b" triplet for the readability scrim laid over the scene. */
  scrimColor: string;
  /** Scheme-default lighting; an explicit `lighting` prop overrides it. */
  lighting: LightingConfig;
}

export interface SatelliteBackgroundProps {
  /** Orbit shape and timing (partial; defaults are filled in). */
  trajectory?: Partial<TrajectoryConfig>;
  /** Satellite attitude mode; fully controlled — switching modes slerps smoothly. */
  orientation?: OrientationConfig;
  /** Field of view and local view offset (partial). */
  camera?: Partial<CameraConfig>;
  /** Earth spin, clouds, atmosphere, texture generation (partial). */
  earth?: Partial<EarthConfig>;
  /** Sun and ambient light (partial). */
  lighting?: Partial<LightingConfig>;
  /** Simulation time multiplier. */
  speed?: number;
  /** Freeze the simulation; the current frame still renders when props change. */
  paused?: boolean;
  /** Freeze when the OS "prefers-reduced-motion" setting is active. Default true. */
  pauseOnReducedMotion?: boolean;
  /** Exponential slerp rate (per simulated second) damping attitude changes. Higher = snappier. */
  attitudeResponsiveness?: number;
  /**
   * Pin the scene palette to a scheme instead of following the site's MUI
   * color scheme (useful outside the themed app). Default: follow the site.
   */
  colorScheme?: "dark" | "light";
  /** Override individual palette entries over the active scheme's palette. */
  palette?: Partial<ScenePalette>;
  /** Opacity of a scrim laid over the scene so foreground text stays readable (its color follows the palette). 0 disables. */
  scrim?: number;
  /** Cap for renderer.setPixelRatio (default 2). */
  maxPixelRatio?: number;
  className?: string;
  style?: CSSProperties;
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

export const DEFAULT_TRAJECTORY: TrajectoryConfig = {
  // Low Earth orbit, ~1.22 Earth radii (~1400 km): close enough that the
  // curved limb and the ground rushing by read as a genuine overflight, high
  // enough that a good share of the frame stays deep space. ISS-like
  // inclination. The period is deliberately faster than a real LEO period
  // (~110 min) so the motion reads within a few seconds without feeling
  // rushed; the sim makes no attempt at wall-clock accuracy.
  radius: 1.22,
  eccentricity: 0,
  inclinationDeg: 51.6,
  raanDeg: 0,
  periodSeconds: 900,
  phaseDeg: 0,
  direction: 1,
};

export const DEFAULT_ORIENTATION: OrientationConfig = {
  mode: "nadir",
  up: "prograde",
};

export const DEFAULT_CAMERA: CameraConfig = {
  // Moderate 50 deg vertical FOV: wide enough for the horizon arc to sweep
  // the frame, narrow enough that perspective stays honest at the edges.
  fovDeg: 50,
  // Pitch up from nadir toward the forward horizon, yawed a touch left so the
  // planet's presence anchors the bottom-right of the frame instead of sitting
  // dead center. From the default orbit (r = 1.22) Earth's angular radius is
  // ~55 deg; pitch 58 keeps the limb just below frame center while yaw 18
  // shifts it right — land and sea fill the lower-right, deep space the rest.
  offsetDeg: { pitch: 58, yaw: 30, roll: -20 },
};

export const DEFAULT_EARTH: EarthConfig = {
  axialTiltDeg: 23.4,
  // Self-spin stays well slower than the orbital rate (720 s) so the ground
  // always flows along the flight direction; the apparent spin of Earth reads
  // as the satellite's overflight.
  rotationPeriodSeconds: 2400,
  clouds: true,
  cloudSpeedFactor: 1.12,
  atmosphere: true,
  textureSeed: 20260906,
  // Grid density for the continent polygons; 1024 keeps the triangle count
  // moderate while the real coastlines stay crisp (they are geometry, not
  // texels).
  textureWidth: 1024,
};

export const DEFAULT_LIGHTING: LightingConfig = {
  // Chosen so the default orbit's opening stretch flies over the day side:
  // the sun sits roughly above the phase-0 ground track. The fixed sun still
  // gives a realistic night pass half an orbit later.
  sunDirection: [5, 2, -1],
  sunColor: "#eef2ff",
  sunIntensity: 2.0,
  ambientColor: "#2b3a55",
  ambientIntensity: 1.6,
};

export const DEFAULT_SCRIM = 0.22;

/** GitHub-dark blues: the original look. */
export const DARK_PALETTE: ScenePalette = {
  background: "#010409",
  ocean: "#0d1b33",
  oceanSpecular: "#2c4a70",
  land: "#2c5d9b",
  ice: "#a8cdf0",
  coastline: "#79c0ff",
  clouds: "#b8cfe2",
  atmosphere: "#4a90ff",
  scrimColor: "0, 0, 0",
  lighting: DEFAULT_LIGHTING,
};

/**
 * GitHub-light: deep space is transparent (the page's white background shows
 * through), the ocean is a white sun-shaded sphere, and continents are
 * stroked slate outlines over a light grey fill. Ice caps render as
 * outline-only and clouds are hidden — white-on-white layers would only dirty
 * the globe. The atmosphere becomes a soft blue halo that reads against the
 * page background.
 */
export const BRIGHT_PALETTE: ScenePalette = {
  background: null,
  // Pure white ocean under strong, neutral light: the day side saturates to
  // white (the bright-scheme globe should read WHITE, not grey), the night
  // side stays a light neutral grey for volume. All tints are hue-free — any
  // blue in the lighting or specular reads as a blue haze on white.
  ocean: "#ffffff",
  oceanSpecular: "#d9dee6",
  land: "#edf0f4",
  ice: null,
  // GitHub light-theme muted foreground: dark enough to carry the outlines.
  coastline: "#57606a",
  clouds: null,
  // The additive limb glow only reads on a dark backdrop; on white it hazes
  // the whole surface blue, so the bright scheme drops it entirely.
  atmosphere: null,
  scrimColor: "255, 255, 255",
  lighting: {
    ...DEFAULT_LIGHTING,
    sunColor: "#ffffff",
    sunIntensity: 1.85,
    ambientColor: "#efefef",
    ambientIntensity: 1.15,
  },
};

/** Everything resolveSettings fills in — what the render loop consumes. */
export interface ResolvedSettings {
  trajectory: TrajectoryConfig;
  orientation: OrientationConfig;
  camera: CameraConfig;
  earth: EarthConfig;
  lighting: LightingConfig;
  speed: number;
  paused: boolean;
  pauseOnReducedMotion: boolean;
  attitudeResponsiveness: number;
  palette: ScenePalette;
  scrim: number;
  maxPixelRatio: number;
}

/** Merge partial props over the defaults, clamping values that would break the sim. */
export function resolveSettings(
  p: SatelliteBackgroundProps,
  mode: "dark" | "light",
): ResolvedSettings {
  const basePalette = mode === "light" ? BRIGHT_PALETTE : DARK_PALETTE;
  const palette: ScenePalette = {
    ...basePalette,
    ...p.palette,
    lighting: { ...basePalette.lighting, ...p.palette?.lighting },
  };
  const trajectory = { ...DEFAULT_TRAJECTORY, ...p.trajectory };
  return {
    trajectory: {
      ...trajectory,
      // Keep the camera outside the atmosphere shell (r = 1.03) and the
      // Kepler solver well-conditioned.
      radius: Math.max(1.06, trajectory.radius),
      eccentricity: Math.min(0.9, Math.max(0, trajectory.eccentricity)),
      periodSeconds: Math.max(1e-3, trajectory.periodSeconds),
    },
    orientation: p.orientation ?? DEFAULT_ORIENTATION,
    camera: {
      ...DEFAULT_CAMERA,
      ...p.camera,
      offsetDeg: { ...DEFAULT_CAMERA.offsetDeg, ...p.camera?.offsetDeg },
    },
    earth: { ...DEFAULT_EARTH, ...p.earth },
    // An explicit lighting prop wins over the palette's scheme default.
    lighting: { ...palette.lighting, ...p.lighting },
    palette,
    speed: p.speed ?? 1,
    paused: p.paused ?? false,
    pauseOnReducedMotion: p.pauseOnReducedMotion ?? true,
    attitudeResponsiveness: Math.max(0, p.attitudeResponsiveness ?? 5),
    scrim: Math.min(1, Math.max(0, p.scrim ?? DEFAULT_SCRIM)),
    maxPixelRatio: Math.max(0.5, p.maxPixelRatio ?? 2),
  };
}
