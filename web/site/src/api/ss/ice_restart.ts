"use client";

export type IceRestartDiagnostic = (
  event: string,
  details?: Record<string, unknown>,
) => void;

export interface IceRestartControllerOptions {
  disconnectGraceMs?: number;
  retryBaseMs?: number;
  retryMaxMs?: number;
  random?: () => number;
  /** Starts a local ICE restart, or asks the polite peer to start one. */
  restart?: () => void;
  onRestartError?: (err: unknown) => void;
  /** Read-only visibility into watchdog decisions; does not alter recovery. */
  onEvent?: IceRestartDiagnostic;
}

const DEFAULT_DISCONNECT_GRACE_MS = 3_000;
const DEFAULT_RETRY_BASE_MS = 2_000;
const DEFAULT_RETRY_MAX_MS = 30_000;

/**
 * Restarts ICE after an established connection stops working. A disconnected
 * state gets a grace period; failed restarts immediately. The retry timer is a
 * watchdog, so checking does not cancel recovery. Success requires both a
 * connected/completed ICE state and an aggregate connected state: a selected
 * ICE pair alone can leave DTLS or another transport unhealthy. Closed or
 * close() stops recovery.
 */
export class IceRestartController {
  private readonly disconnectGraceMs: number;
  private readonly retryBaseMs: number;
  private readonly retryMaxMs: number;
  private readonly random: () => number;
  private readonly requestRestart: () => void;
  private readonly onRestartError: (err: unknown) => void;
  private readonly onEvent?: IceRestartDiagnostic;

  private timer?: ReturnType<typeof setTimeout>;
  private usable = false;
  private recovering = false;
  private attempts = 0;
  private closed = false;

  constructor(
    private readonly pc: RTCPeerConnection,
    options: IceRestartControllerOptions = {},
  ) {
    this.disconnectGraceMs =
      options.disconnectGraceMs ?? DEFAULT_DISCONNECT_GRACE_MS;
    this.retryBaseMs = options.retryBaseMs ?? DEFAULT_RETRY_BASE_MS;
    this.retryMaxMs = options.retryMaxMs ?? DEFAULT_RETRY_MAX_MS;
    this.random = options.random ?? Math.random;
    this.requestRestart = options.restart ?? (() => this.pc.restartIce());
    this.onRestartError =
      options.onRestartError ??
      ((err) => console.error("ICE restart failed", err));
    this.onEvent = options.onEvent;
  }

  handleState(state: RTCIceConnectionState): void {
    if (this.closed) return;

    if (this.isHealthy(state, this.pc.connectionState)) {
      this.markHealthy();
      return;
    }
    switch (state) {
      case "connected":
      case "completed":
        // ICE can find a working candidate pair before DTLS (or another
        // transport) is healthy. Let the aggregate state decide whether the
        // recovery watchdog may stop.
        this.handleConnectionState(this.pc.connectionState);
        return;
      case "disconnected":
        this.handleDisconnected();
        return;
      case "failed":
        this.handleFailed();
        return;
      case "closed":
        this.close();
        return;
      default:
        // new/checking are not outcomes: a restart watchdog must keep running.
        return;
    }
  }

  /** Reconciles recovery with the aggregate ICE + DTLS connection state. */
  handleConnectionState(state: RTCPeerConnectionState): void {
    if (this.closed) return;
    if (this.isHealthy(this.pc.iceConnectionState, state)) {
      this.markHealthy();
      return;
    }
    switch (state) {
      case "disconnected":
        this.handleDisconnected();
        return;
      case "failed":
        this.handleFailed();
        return;
      case "closed":
        this.close();
        return;
      default:
        // new/connecting are not outcomes; an active watchdog keeps running.
        return;
    }
  }

  private isHealthy(
    ice: RTCIceConnectionState,
    connection: RTCPeerConnectionState,
  ): boolean {
    return (
      (ice === "connected" || ice === "completed") &&
      connection === "connected"
    );
  }

  private markHealthy(): void {
    const wasRecovering =
      this.recovering || this.attempts > 0 || this.timer !== undefined;
    const previousAttempts = this.attempts;
    this.usable = true;
    this.recovering = false;
    this.attempts = 0;
    this.stopTimer();
    if (wasRecovering) this.emit("recovered", { previousAttempts });
  }

  private handleDisconnected(): void {
    if (this.usable && !this.recovering) {
      this.recovering = true;
      this.schedule(this.disconnectGraceMs);
    }
  }

  private handleFailed(): void {
    if (!this.usable || (this.recovering && this.attempts > 0)) return;
    this.recovering = true;
    this.stopTimer();
    this.schedule(0);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.recovering = false;
    this.stopTimer();
    this.emit("closed");
  }

  private stopTimer(): void {
    if (this.timer !== undefined) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
  }

  private schedule(delayMs: number): void {
    if (this.closed || !this.recovering || this.timer !== undefined) return;
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.restart();
    }, delayMs);
    this.emit("watchdog:scheduled", { delayMs });
  }

  private restart(): void {
    if (this.closed || !this.recovering) return;
    if (
      this.isHealthy(this.pc.iceConnectionState, this.pc.connectionState)
    ) {
      this.markHealthy();
      return;
    }
    if (
      this.pc.iceConnectionState === "closed" ||
      this.pc.connectionState === "closed" ||
      this.pc.signalingState === "closed"
    ) {
      this.close();
      return;
    }

    this.attempts += 1;
    this.emit("restart:attempt");
    try {
      this.requestRestart();
    } catch (err) {
      this.onRestartError(err);
    }
    this.schedule(this.nextRetryDelay());
  }

  private nextRetryDelay(): number {
    const exponential = this.retryBaseMs * 2 ** Math.min(this.attempts - 1, 20);
    const capped = Math.min(this.retryMaxMs, exponential);
    return capped * (0.8 + this.random() * 0.4);
  }

  private emit(event: string, details: Record<string, unknown> = {}): void {
    this.onEvent?.(event, {
      iceConnectionState: this.pc.iceConnectionState,
      connectionState: this.pc.connectionState,
      signalingState: this.pc.signalingState,
      usable: this.usable,
      recovering: this.recovering,
      attempts: this.attempts,
      timerPending: this.timer !== undefined,
      ...details,
    });
  }
}
