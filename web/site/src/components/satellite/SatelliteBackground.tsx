"use client";

import { useEffect, useRef } from "react";
import { Box } from "@mui/material";
import * as THREE from "three";
import { LineSegments2 } from "three/examples/jsm/lines/LineSegments2.js";
import { LineSegmentsGeometry } from "three/examples/jsm/lines/LineSegmentsGeometry.js";
import { LineMaterial } from "three/examples/jsm/lines/LineMaterial.js";
import {
  createEarthSurfaceJob,
  type EarthSurfaceJob,
  type EarthSurfaceSet,
} from "./earthTexture";
import {
  createSatelliteState,
  satelliteStateAt,
  type SatelliteState,
} from "./trajectory";
import {
  DEFAULT_SCRIM,
  resolveSettings,
  type ResolvedSettings,
  type SatelliteBackgroundProps,
} from "./types";

// ---------------------------------------------------------------------------
// SatelliteBackground — a fixed, viewport-sized three.js backdrop that renders
// Earth as seen from a satellite flying a configurable orbit. The component is
// fully controlled: every prop is re-resolved on each React render and applied
// to the live scene (no remount on config change). The satellite pose is a
// pure function of the trajectory config and simulated time, and all
// orientation work (orbit plane, attitude, spin, view offset) is done with
// quaternions.
//
// Example:
//   <SatelliteBackground
//     trajectory={{ radius: 2.1, inclinationDeg: 63, periodSeconds: 120 }}
//     orientation={{ mode: "prograde", up: "radial" }}
//     camera={{ offsetDeg: { pitch: 18, yaw: 0, roll: 0 } }}
//     speed={1.5}
//   />
// ---------------------------------------------------------------------------

const AXIS_Y = new THREE.Vector3(0, 1, 0);
const AXIS_Z = new THREE.Vector3(0, 0, 1);

// Framing note: the satellite flies a LOW Earth orbit (~1.2 Earth radii), so
// the planet subtends over 100 degrees of view and can never be a small ball
// in the corner. The composition is the one every real LEO photo uses: the
// camera pitches up from nadir toward the horizon, Earth's curved limb arcs
// across the frame with land and sea below it, and deep space fills the rest.
// A perspective camera only projects a sphere as a circle when the sphere is
// centered on the view axis; the horizon composition never presents a full
// disk, so the globe reads naturally at any viewport aspect — no letterboxing
// or projection tricks. The camera therefore renders full-bleed at the
// container's true aspect ratio.

// Additive fresnel shell slightly larger than Earth: a thin blue glow hugging
// the limb, bright on the sun side and fading to nothing on the night side.
// Space itself stays flat black (see scene.background) — no stars.
const ATMOSPHERE_VERTEX_SHADER = /* glsl */ `
  varying vec3 vWorldNormal;
  varying vec3 vViewDir;
  void main() {
    vec4 worldPos = modelMatrix * vec4(position, 1.0);
    vWorldNormal = normalize(mat3(modelMatrix) * normal);
    vViewDir = normalize(cameraPosition - worldPos.xyz);
    gl_Position = projectionMatrix * viewMatrix * worldPos;
  }
`;

const ATMOSPHERE_FRAGMENT_SHADER = /* glsl */ `
  uniform vec3 uColor;
  uniform vec3 uLightDir;
  uniform float uPower;
  uniform float uStrength;
  uniform float uEdge;
  varying vec3 vWorldNormal;
  varying vec3 vViewDir;
  void main() {
    float ndv = clamp(dot(normalize(vWorldNormal), normalize(vViewDir)), 0.0, 1.0);
    float rim = pow(1.0 - ndv, uPower);
    // Dissolve the glow toward the shell silhouette so the atmosphere's
    // outer outline blurs into space instead of ending on a hard edge.
    float edge = smoothstep(0.0, uEdge, ndv);
    float sun = clamp(dot(normalize(vWorldNormal), normalize(uLightDir)), 0.0, 1.0);
    float glow = rim * edge * uStrength * (0.12 + 0.88 * sun);
    gl_FragColor = vec4(uColor, glow);
  }
`;

interface Engine {
  renderer: THREE.WebGLRenderer;
  scene: THREE.Scene;
  camera: THREE.PerspectiveCamera;
  earthGroup: THREE.Group;
  earthMesh: THREE.Mesh;
  oceanMaterial: THREE.MeshPhongMaterial;
  landMesh: THREE.Mesh;
  landMaterial: THREE.MeshLambertMaterial;
  iceMesh: THREE.Mesh;
  iceMaterial: THREE.MeshLambertMaterial;
  cloudMesh: THREE.Mesh;
  cloudMaterial: THREE.MeshLambertMaterial;
  coast: LineSegments2 | null;
  coastMaterial: LineMaterial;
  atmosphere: THREE.Mesh;
  atmosphereMaterial: THREE.ShaderMaterial;
  sun: THREE.DirectionalLight;
  ambient: THREE.AmbientLight;

  settings: ResolvedSettings;
  surface: EarthSurfaceSet | null;
  surfaceKey: string;
  surfaceJob: EarthSurfaceJob | null;
  surfaceToken: number;

  // Simulation state.
  simTime: number;
  earthSpin: THREE.Quaternion;
  cloudSpin: THREE.Quaternion;
  spinDelta: THREE.Quaternion;
  attitude: THREE.Quaternion;
  attitudeReady: boolean;
  viewOffset: THREE.Quaternion;
  state: SatelliteState;

  reducedMotion: boolean;
  dirty: boolean;
  disposed: boolean;
}

function surfaceKeyOf(settings: ResolvedSettings): string {
  const e = settings.earth;
  return `${e.textureSeed}|${e.textureWidth}|${e.clouds}`;
}

// Bumped on every job start and on teardown so in-flight deliveries from a
// superseded job can recognize they are stale.
let surfaceTokenSeq = 0;

/** Replace a surface mesh's geometry from flat triangle positions (radial normals). */
function setSurfaceGeometry(
  mesh: THREE.Mesh,
  positions: Float32Array | null,
): void {
  const old = mesh.geometry;
  if (!positions || positions.length < 9) {
    mesh.visible = false;
    return;
  }
  const normals = new Float32Array(positions.length);
  for (let i = 0; i < positions.length; i += 3) {
    const x = positions[i];
    const y = positions[i + 1];
    const z = positions[i + 2];
    const len = Math.hypot(x, y, z) || 1;
    normals[i] = x / len;
    normals[i + 1] = y / len;
    normals[i + 2] = z / len;
  }
  const geometry = new THREE.BufferGeometry();
  geometry.setAttribute("position", new THREE.BufferAttribute(positions, 3));
  geometry.setAttribute("normal", new THREE.BufferAttribute(normals, 3));
  mesh.geometry = geometry;
  mesh.visible = true;
  old.dispose();
}

/** Swap a freshly generated surface set into the scene. Owns disposal of the set it replaces. */
function applySurfaceSet(engine: Engine, surface: EarthSurfaceSet): void {
  engine.surface?.dispose();
  engine.surface = surface;

  if (surface.clouds) {
    engine.cloudMaterial.map = surface.clouds;
    engine.cloudMaterial.needsUpdate = true;
  }
  engine.cloudMesh.visible =
    engine.settings.earth.clouds && surface.clouds !== null;

  // Continents and ice caps are flat-color polygon meshes; coastline strokes
  // are vector lines. All rebuilt per set so layers stay aligned.
  setSurfaceGeometry(engine.landMesh, surface.land);
  setSurfaceGeometry(engine.iceMesh, surface.ice);

  if (engine.coast) {
    engine.earthMesh.remove(engine.coast);
    engine.coast.geometry.dispose();
    engine.coast = null;
  }
  if (surface.coastline && surface.coastline.length >= 6) {
    const geometry = new LineSegmentsGeometry();
    geometry.setPositions(surface.coastline);
    engine.coast = new LineSegments2(geometry, engine.coastMaterial);
    // Segment bounding volumes hug the unit sphere; culling gains nothing.
    engine.coast.frustumCulled = false;
    engine.earthMesh.add(engine.coast);
  }
  engine.dirty = true;
}

/**
 * Kick off procedural surface generation: a coarse preview lands in the scene
 * immediately, then the full-resolution set streams in band-by-band between
 * frames (see earthTexture.ts) so the main thread stays responsive.
 */
function startSurfaceJob(engine: Engine): void {
  engine.surfaceJob?.dispose();
  const token = ++surfaceTokenSeq;
  engine.surfaceToken = token;

  const settings = engine.settings;
  const anisotropy = Math.min(
    4,
    engine.renderer.capabilities.getMaxAnisotropy(),
  );
  const job = createEarthSurfaceJob(
    settings.earth.textureSeed,
    settings.earth.textureWidth,
    settings.earth.clouds,
    anisotropy,
    (surface) => {
      // The job was superseded (regeneration) or the component unmounted
      // between scheduling and delivery.
      if (engine.disposed || engine.surfaceToken !== token) {
        surface.dispose();
        return;
      }
      applySurfaceSet(engine, surface);
    },
  );
  engine.surfaceJob = job;
  if (job.done) {
    // Small grids are generated synchronously and already delivered.
    engine.surfaceJob = null;
    return;
  }

  const scheduleStep = () => {
    window.setTimeout(() => {
      if (engine.disposed || engine.surfaceToken !== token) return;
      if (engine.surfaceJob !== job) return;
      if (job.step()) {
        engine.surfaceJob = null;
      } else {
        scheduleStep();
      }
    }, 0);
  };
  scheduleStep();
}

/** Push resolved React props into the live scene. Cheap enough to run every render. */
function applySettings(engine: Engine, settings: ResolvedSettings): void {
  engine.settings = settings;
  engine.dirty = true;

  // Camera: FOV, local view offset (quaternion in the satellite frame) and a
  // far plane that tracks the orbit size.
  const camera = engine.camera;
  const { fovDeg, offsetDeg } = settings.camera;
  let projectionChanged = false;
  if (camera.fov !== fovDeg) {
    camera.fov = fovDeg;
    projectionChanged = true;
  }
  const far =
    settings.trajectory.radius * (1 + settings.trajectory.eccentricity) * 3 +
    10;
  if (Math.abs(camera.far - far) > 1e-3) {
    camera.far = far;
    projectionChanged = true;
  }
  if (projectionChanged) camera.updateProjectionMatrix();
  engine.viewOffset.setFromEuler(
    new THREE.Euler(
      THREE.MathUtils.degToRad(offsetDeg.pitch),
      THREE.MathUtils.degToRad(offsetDeg.yaw),
      THREE.MathUtils.degToRad(offsetDeg.roll),
      "XYZ",
    ),
  );

  // Deep space: a flat dark backdrop color.
  (engine.scene.background as THREE.Color).set(settings.backgroundColor);

  // Sun + ambient.
  const sunDir = new THREE.Vector3(...settings.lighting.sunDirection);
  if (sunDir.lengthSq() < 1e-9) sunDir.set(1, 0, 0);
  sunDir.normalize();
  engine.sun.position.copy(sunDir).multiplyScalar(10);
  engine.sun.color.set(settings.lighting.sunColor);
  engine.sun.intensity = settings.lighting.sunIntensity;
  engine.ambient.color.set(settings.lighting.ambientColor);
  engine.ambient.intensity = settings.lighting.ambientIntensity;

  // Atmosphere shell tracks the sun direction and the config flag.
  const uLightDir = engine.atmosphereMaterial.uniforms.uLightDir
    .value as THREE.Vector3;
  uLightDir.copy(sunDir);
  engine.atmosphere.visible = settings.earth.atmosphere;

  // Axial tilt as a quaternion about world Z, wrapping the spinning meshes.
  engine.earthGroup.quaternion.setFromAxisAngle(
    AXIS_Z,
    THREE.MathUtils.degToRad(settings.earth.axialTiltDeg),
  );
  engine.cloudMesh.visible =
    settings.earth.clouds && engine.surface?.clouds != null;

  const pixelRatio = Math.min(
    window.devicePixelRatio || 1,
    settings.maxPixelRatio,
  );
  if (engine.renderer.getPixelRatio() !== pixelRatio) {
    engine.renderer.setPixelRatio(pixelRatio);
  }

  if (surfaceKeyOf(settings) !== engine.surfaceKey) {
    engine.surfaceKey = surfaceKeyOf(settings);
    startSurfaceJob(engine);
  }
}

/** One animation frame: advance the sim, pose the camera, render. */
function frame(engine: Engine, timer: THREE.Timer): void {
  if (engine.disposed) return;
  const s = engine.settings;
  const frozen = s.paused || (s.pauseOnReducedMotion && engine.reducedMotion);

  // Always consume the timer delta (clamped so a backgrounded tab doesn't
  // teleport the satellite on return).
  timer.update();
  const dt = Math.min(timer.getDelta(), 0.1);

  // While frozen, only re-render if props changed since the last frame.
  if (frozen && !engine.dirty) return;

  const sdt = frozen ? 0 : dt * s.speed;
  engine.simTime += sdt;

  // Earth + cloud self-rotation, accumulated as quaternions about the local
  // (tilted) spin axis.
  if (s.earth.rotationPeriodSeconds > 0 && sdt !== 0) {
    const omega = (Math.PI * 2) / s.earth.rotationPeriodSeconds;
    engine.spinDelta.setFromAxisAngle(AXIS_Y, omega * sdt);
    engine.earthSpin.multiply(engine.spinDelta);
    engine.earthMesh.quaternion.copy(engine.earthSpin);
    engine.spinDelta.setFromAxisAngle(
      AXIS_Y,
      omega * sdt * s.earth.cloudSpeedFactor,
    );
    engine.cloudSpin.multiply(engine.spinDelta);
    engine.cloudMesh.quaternion.copy(engine.cloudSpin);
  }

  // Satellite pose: pure function of trajectory + simulated time.
  const state = satelliteStateAt(
    s.trajectory,
    s.orientation,
    engine.simTime,
    engine.state,
  );

  // Attitude: snap on first frame or while frozen (settings edits), otherwise
  // damped slerp toward the target (frame-rate independent).
  if (!engine.attitudeReady || frozen) {
    engine.attitude.copy(state.attitude);
    engine.attitudeReady = true;
  } else {
    const k = 1 - Math.exp(-s.attitudeResponsiveness * dt);
    engine.attitude.slerp(state.attitude, k);
  }

  engine.camera.position.copy(state.position);
  engine.camera.quaternion.copy(engine.attitude).multiply(engine.viewOffset);

  engine.renderer.render(engine.scene, engine.camera);
  engine.dirty = false;
}

export default function SatelliteBackground(props: SatelliteBackgroundProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const engineRef = useRef<Engine | null>(null);
  const settingsRef = useRef<ResolvedSettings | null>(null);
  if (settingsRef.current === null) {
    settingsRef.current = resolveSettings(props);
  }
  const scrim = props.scrim ?? DEFAULT_SCRIM;

  // Scene construction (mount only). Declared before the settings sync effect
  // below so the engine exists by the time props are first applied.
  useEffect(() => {
    const container = containerRef.current;
    const initialSettings = settingsRef.current;
    if (!container || !initialSettings) return;

    const width = Math.max(1, container.clientWidth);
    const height = Math.max(1, container.clientHeight);

    const renderer = new THREE.WebGLRenderer({
      antialias: true,
      powerPreference: "high-performance",
    });
    renderer.setPixelRatio(
      Math.min(window.devicePixelRatio || 1, initialSettings.maxPixelRatio),
    );
    // The canvas fills the container; the drawing buffer matches it exactly
    // (re-sized by the ResizeObserver below), so pixels are never rescaled.
    renderer.domElement.style.width = "100%";
    renderer.domElement.style.height = "100%";
    renderer.setSize(width, height, false);
    container.appendChild(renderer.domElement);

    const scene = new THREE.Scene();
    scene.background = new THREE.Color(initialSettings.backgroundColor);

    const camera = new THREE.PerspectiveCamera(
      initialSettings.camera.fovDeg,
      width / height,
      0.01,
      20,
    );

    // Earth: tilt group (quaternion) wrapping the spinning surface + clouds.
    const earthGroup = new THREE.Group();
    scene.add(earthGroup);

    const earthGeometry = new THREE.SphereGeometry(1, 96, 64);
    const oceanMaterial = new THREE.MeshPhongMaterial({
      // GitHub-dark vector palette: blues and darker blues only. The ocean is
      // a pure deep-navy color; continents are polygon meshes layered slightly
      // above (see applySurfaceSet).
      color: 0x0d1b33,
      specular: new THREE.Color(0x2c4a70),
      shininess: 28,
    });
    const earthMesh = new THREE.Mesh(earthGeometry, oceanMaterial);
    earthGroup.add(earthMesh);

    // Flat-color polygon layers: land and ice caps (geometry arrives from the
    // surface job; hidden until then).
    const landMaterial = new THREE.MeshLambertMaterial({ color: 0x2c5d9b });
    const iceMaterial = new THREE.MeshLambertMaterial({ color: 0xa8cdf0 });
    const landMesh = new THREE.Mesh(new THREE.BufferGeometry(), landMaterial);
    const iceMesh = new THREE.Mesh(new THREE.BufferGeometry(), iceMaterial);
    landMesh.visible = false;
    iceMesh.visible = false;
    // Children of the ocean sphere so they inherit its spin quaternion.
    earthMesh.add(landMesh, iceMesh);

    const cloudGeometry = new THREE.SphereGeometry(1.008, 64, 48);
    const cloudMaterial = new THREE.MeshLambertMaterial({
      // Pale blue wisps, kept faint so they never blanket the globe.
      color: 0xb8cfe2,
      transparent: true,
      opacity: 0.8,
      depthWrite: false,
    });
    const cloudMesh = new THREE.Mesh(cloudGeometry, cloudMaterial);
    cloudMesh.visible = false;
    earthGroup.add(cloudMesh);

    // Screen-space-wide vector strokes for the coastlines (fat lines, since
    // raw WebGL line primitives are stuck at 1px).
    const coastMaterial = new LineMaterial({
      // GitHub link blue strokes.
      color: 0x79c0ff,
      linewidth: 1.25,
      transparent: true,
      opacity: 0.9,
      depthWrite: false,
    });
    coastMaterial.resolution.set(
      width * renderer.getPixelRatio(),
      height * renderer.getPixelRatio(),
    );

    const atmosphereGeometry = new THREE.SphereGeometry(1.03, 64, 48);
    const atmosphereMaterial = new THREE.ShaderMaterial({
      uniforms: {
        uColor: { value: new THREE.Color("#4a90ff") },
        uLightDir: { value: new THREE.Vector3(1, 0, 0) },
        uPower: { value: 3.0 },
        uStrength: { value: 1.0 },
        uEdge: { value: 0.3 },
      },
      vertexShader: ATMOSPHERE_VERTEX_SHADER,
      fragmentShader: ATMOSPHERE_FRAGMENT_SHADER,
      transparent: true,
      blending: THREE.AdditiveBlending,
      depthWrite: false,
      side: THREE.FrontSide,
    });
    const atmosphere = new THREE.Mesh(atmosphereGeometry, atmosphereMaterial);
    scene.add(atmosphere);

    const sun = new THREE.DirectionalLight(0xffffff, 1.35);
    scene.add(sun);
    const ambient = new THREE.AmbientLight(0x30405f, 1.1);
    scene.add(ambient);

    const engine: Engine = {
      renderer,
      scene,
      camera,
      earthGroup,
      earthMesh,
      oceanMaterial,
      landMesh,
      landMaterial,
      iceMesh,
      iceMaterial,
      cloudMesh,
      cloudMaterial,
      coast: null,
      coastMaterial,
      atmosphere,
      atmosphereMaterial,
      sun,
      ambient,
      settings: initialSettings,
      surface: null,
      surfaceKey: "",
      surfaceJob: null,
      surfaceToken: 0,
      simTime: 0,
      earthSpin: new THREE.Quaternion(),
      cloudSpin: new THREE.Quaternion(),
      spinDelta: new THREE.Quaternion(),
      attitude: new THREE.Quaternion(),
      attitudeReady: false,
      viewOffset: new THREE.Quaternion(),
      state: createSatelliteState(),
      reducedMotion: false,
      dirty: true,
      disposed: false,
    };
    engineRef.current = engine;
    applySettings(engine, initialSettings);

    // Accessibility: honor the OS reduced-motion preference by freezing the
    // simulation (the static view still renders).
    const motionQuery = window.matchMedia("(prefers-reduced-motion: reduce)");
    engine.reducedMotion = motionQuery.matches;
    const onMotionChange = (event: MediaQueryListEvent) => {
      engine.reducedMotion = event.matches;
      engine.dirty = true;
    };
    motionQuery.addEventListener("change", onMotionChange);

    const resizeObserver = new ResizeObserver(() => {
      const cw = Math.max(1, container.clientWidth);
      const ch = Math.max(1, container.clientHeight);
      renderer.setSize(cw, ch, false);
      // The projection tracks the true container aspect; the horizon
      // composition stays natural at any window shape (see the framing note).
      camera.aspect = cw / ch;
      camera.updateProjectionMatrix();
      const pr = renderer.getPixelRatio();
      coastMaterial.resolution.set(cw * pr, ch * pr);
      engine.dirty = true;
    });
    resizeObserver.observe(container);

    const timer = new THREE.Timer();
    // Page Visibility handling inside the timer avoids huge deltas after the
    // tab was hidden.
    timer.connect(document);
    renderer.setAnimationLoop(() => frame(engine, timer));

    return () => {
      engine.disposed = true;
      // Invalidate in-flight surface deliveries, then stop the job.
      engine.surfaceToken++;
      engine.surfaceJob?.dispose();
      engine.surfaceJob = null;
      renderer.setAnimationLoop(null);
      timer.disconnect();
      resizeObserver.disconnect();
      motionQuery.removeEventListener("change", onMotionChange);
      engine.surface?.dispose();
      if (engine.coast) engine.coast.geometry.dispose();
      coastMaterial.dispose();
      earthGeometry.dispose();
      oceanMaterial.dispose();
      landMesh.geometry.dispose();
      landMaterial.dispose();
      iceMesh.geometry.dispose();
      iceMaterial.dispose();
      cloudGeometry.dispose();
      cloudMaterial.dispose();
      atmosphereGeometry.dispose();
      atmosphereMaterial.dispose();
      renderer.dispose();
      if (renderer.domElement.parentElement === container) {
        container.removeChild(renderer.domElement);
      }
      engineRef.current = null;
    };
  }, []);

  // Controlled props: re-resolve and push into the live scene on every render.
  useEffect(() => {
    const settings = resolveSettings(props);
    settingsRef.current = settings;
    const engine = engineRef.current;
    if (engine) applySettings(engine, settings);
  });

  return (
    <Box
      ref={containerRef}
      aria-hidden
      className={props.className}
      sx={{
        // Fixed to the viewport: exact viewport dimensions, unaffected by
        // scrolling. zIndex -1 keeps the whole page stack above it, and
        // pointer-events none keeps it from swallowing clicks.
        position: "fixed",
        top: 0,
        left: 0,
        width: "100vw",
        // dvh where supported so mobile URL-bar collapse doesn't leave a gap;
        // vh as the fallback.
        height: ["100vh", "100dvh"],
        zIndex: -1,
        overflow: "hidden",
        pointerEvents: "none",
        bgcolor: "#000",
      }}
      style={props.style}
    >
      {scrim > 0 ? (
        <Box
          sx={{
            position: "absolute",
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            // Positioned above the statically-placed canvas so foreground copy
            // stays readable without hiding the scene.
            zIndex: 1,
            bgcolor: `rgba(0, 0, 0, ${scrim})`,
          }}
        />
      ) : null}
    </Box>
  );
}
