import * as THREE from "three";
import type { AttitudeUp, OrientationConfig, TrajectoryConfig } from "./types";

// ---------------------------------------------------------------------------
// Orbit + attitude math for the satellite background. Everything is expressed
// with quaternions: the orbit plane is a quaternion composition (RAAN ⊗
// inclination), positions/velocities are plane-frame vectors rotated by that
// quaternion, and attitudes are built as quaternions from an explicit basis.
//
// The frame-loop entry point is satelliteStateAt(); all scratch objects are
// module-level so a frame never allocates.
// ---------------------------------------------------------------------------

const AXIS_X = new THREE.Vector3(1, 0, 0);
const AXIS_Y = new THREE.Vector3(0, 1, 0);
const WORLD_NORTH = new THREE.Vector3(0, 1, 0);
const ORIGIN = new THREE.Vector3(0, 0, 0);

// Scratch (single-threaded render loop only).
const scratchPlaneQ = new THREE.Quaternion();
const scratchRaanQ = new THREE.Quaternion();
const scratchInclQ = new THREE.Quaternion();
const scratchBasis = new THREE.Matrix4();
const scratchZAxis = new THREE.Vector3();
const scratchXAxis = new THREE.Vector3();
const scratchYAxis = new THREE.Vector3();
const scratchUp = new THREE.Vector3();
const scratchTarget = new THREE.Vector3();

/** Full pose of the satellite at a point in simulated time. */
export interface SatelliteState {
  /** World-space position, in Earth radii. */
  position: THREE.Vector3;
  /** World-space velocity, in Earth radii per simulated second. */
  velocity: THREE.Vector3;
  /** Unit normal of the orbit plane (right-handed about the direction of travel for direction = +1). */
  orbitNormal: THREE.Vector3;
  /** Target attitude quaternion implied by the orientation config. */
  attitude: THREE.Quaternion;
}

export function createSatelliteState(): SatelliteState {
  return {
    position: new THREE.Vector3(),
    velocity: new THREE.Vector3(),
    orbitNormal: new THREE.Vector3(0, 1, 0),
    attitude: new THREE.Quaternion(),
  };
}

/**
 * Orientation of the orbit plane as a single quaternion: tilt the reference
 * XZ plane about X by the inclination, then rotate about world Y (Earth's
 * spin axis) by the RAAN.
 *
 *   Q_plane = q_Y(raan) ⊗ q_X(inclination)
 */
export function orbitPlaneQuaternion(
  trajectory: TrajectoryConfig,
  out = new THREE.Quaternion(),
): THREE.Quaternion {
  scratchRaanQ.setFromAxisAngle(AXIS_Y, THREE.MathUtils.degToRad(trajectory.raanDeg));
  scratchInclQ.setFromAxisAngle(AXIS_X, THREE.MathUtils.degToRad(trajectory.inclinationDeg));
  return out.multiplyQuaternions(scratchRaanQ, scratchInclQ);
}

/** Newton-Raphson solution of Kepler's equation M = E - e·sin(E). */
export function solveKeplerE(meanAnomaly: number, e: number): number {
  const twoPi = Math.PI * 2;
  let m = meanAnomaly % twoPi;
  if (m < 0) m += twoPi;
  // Starting guess: E = M is fine for low eccentricity; E = pi converges
  // better for high eccentricity.
  let E = e < 0.8 ? m : Math.PI;
  for (let i = 0; i < 12; i++) {
    const dE = (E - e * Math.sin(E) - m) / (1 - e * Math.cos(E));
    E -= dE;
    if (Math.abs(dE) < 1e-10) break;
  }
  return E;
}

/**
 * Satellite pose at `timeSeconds` of simulated time: a pure function of the
 * trajectory config, so the React component stays fully controlled — changing
 * any trajectory prop re-parameterizes the path without rebuilding the scene.
 */
export function satelliteStateAt(
  trajectory: TrajectoryConfig,
  orientation: OrientationConfig,
  timeSeconds: number,
  out: SatelliteState = createSatelliteState(),
): SatelliteState {
  const a = trajectory.radius;
  const e = trajectory.eccentricity;
  // Mean motion (signed by direction of travel).
  const n = (Math.PI * 2 * trajectory.direction) / trajectory.periodSeconds;
  const M = THREE.MathUtils.degToRad(trajectory.phaseDeg) + n * timeSeconds;
  const E = solveKeplerE(M, e);
  const cosE = Math.cos(E);
  const sinE = Math.sin(E);
  const b = a * Math.sqrt(1 - e * e);
  const eDot = n / (1 - e * cosE);

  // In-plane state with Earth at the focus and periapsis on +X. The sign of
  // the Z components makes travel right-handed about the plane's +Y normal
  // (+X -> -Z), matching orbitPlaneQuaternion's convention.
  const px = a * (cosE - e);
  const pz = -b * sinE;
  const vx = -a * sinE * eDot;
  const vz = -b * cosE * eDot;

  const planeQ = orbitPlaneQuaternion(trajectory, scratchPlaneQ);
  out.position.set(px, 0, pz).applyQuaternion(planeQ);
  out.velocity.set(vx, 0, vz).applyQuaternion(planeQ);
  out.orbitNormal.set(0, 1, 0).applyQuaternion(planeQ);

  attitudeQuaternion(out.position, out.velocity, out.orbitNormal, orientation, out.attitude);
  return out;
}

/**
 * Target attitude quaternion for an orientation mode:
 * - nadir: -Z points at Earth's center,
 * - prograde: -Z points along the velocity vector,
 * - fixed: the explicit inertial quaternion from the config.
 */
export function attitudeQuaternion(
  position: THREE.Vector3,
  velocity: THREE.Vector3,
  orbitNormal: THREE.Vector3,
  orientation: OrientationConfig,
  out = new THREE.Quaternion(),
): THREE.Quaternion {
  switch (orientation.mode) {
    case "fixed":
      return out.set(...orientation.quaternion).normalize();
    case "prograde": {
      upVector(orientation.up ?? "radial", position, velocity, orbitNormal, scratchUp);
      scratchTarget.copy(position).add(velocity);
      return lookQuaternion(position, scratchTarget, scratchUp, out);
    }
    case "nadir":
    default: {
      upVector(orientation.up ?? "prograde", position, velocity, orbitNormal, scratchUp);
      return lookQuaternion(position, ORIGIN, scratchUp, out);
    }
  }
}

function upVector(
  kind: AttitudeUp,
  position: THREE.Vector3,
  velocity: THREE.Vector3,
  orbitNormal: THREE.Vector3,
  out: THREE.Vector3,
): THREE.Vector3 {
  switch (kind) {
    case "orbit-normal":
      return out.copy(orbitNormal);
    case "world-north":
      return out.copy(WORLD_NORTH);
    case "radial":
      return out.copy(position);
    case "prograde":
    default:
      return out.copy(velocity);
  }
}

/**
 * Camera-convention attitude quaternion: local -Z points from `eye` toward
 * `target` while local +Y leans toward `up`. Built from an explicit
 * orthonormal basis converted through a rotation matrix.
 */
export function lookQuaternion(
  eye: THREE.Vector3,
  target: THREE.Vector3,
  up: THREE.Vector3,
  out = new THREE.Quaternion(),
): THREE.Quaternion {
  scratchZAxis.subVectors(eye, target);
  if (scratchZAxis.lengthSq() < 1e-12) scratchZAxis.set(0, 0, 1);
  scratchZAxis.normalize();

  scratchXAxis.crossVectors(up, scratchZAxis);
  if (scratchXAxis.lengthSq() < 1e-12) {
    // Degenerate when `up` is parallel to the view direction (e.g. nadir with
    // up = world-north while passing over a pole): fall back to any vector
    // that is guaranteed non-parallel.
    const fallback = Math.abs(scratchZAxis.y) < 0.9 ? WORLD_NORTH : AXIS_X;
    scratchXAxis.crossVectors(fallback, scratchZAxis);
  }
  scratchXAxis.normalize();
  scratchYAxis.crossVectors(scratchZAxis, scratchXAxis);

  scratchBasis.makeBasis(scratchXAxis, scratchYAxis, scratchZAxis);
  return out.setFromRotationMatrix(scratchBasis);
}
