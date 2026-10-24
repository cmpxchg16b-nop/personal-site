"use client";

export interface IceRestartControllerOptions {
  disconnectGraceMs?: number;
  retryBaseMs?: number;
  retryMaxMs?: number;
  random?: () => number;
  /** Starts a local ICE restart, or asks the polite peer to start one. */
  restart?: () => void;
  onRestartError?: (err: unknown) => void;
}

const DEFAULT_DISCONNECT_GRACE_MS = 3_000;
const DEFAULT_RETRY_BASE_MS = 2_000;
const DEFAULT_RETRY_MAX_MS = 30_000;

/**
 * Restarts ICE after an established connection stops working. A disconnected
 * state gets a grace period; failed restarts immediately. The retry timer is a
 * watchdog, so checking does not cancel recovery — only connected/completed,
 * closed, or close() does.
 */
export class IceRestartController {
  private readonly disconnectGraceMs: number;
  private readonly retryBaseMs: number;
  private readonly retryMaxMs: number;
  private readonly random: () => number;
  private readonly requestRestart: () => void;
  private readonly onRestartError: (err: unknown) => void;

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
  }

  handleState(state: RTCIceConnectionState): void {
    if (this.closed) return;

    switch (state) {
      case "connected":
      case "completed":
        this.usable = true;
        this.recovering = false;
        this.attempts = 0;
        this.stopTimer();
        return;
      case "disconnected":
        if (this.usable && !this.recovering) {
          this.recovering = true;
          this.schedule(this.disconnectGraceMs);
        }
        return;
      case "failed":
        if (!this.usable || (this.recovering && this.attempts > 0)) return;
        this.recovering = true;
        this.stopTimer();
        this.schedule(0);
        return;
      case "closed":
        this.close();
        return;
      default:
        // new/checking are not outcomes: a restart watchdog must keep running.
        return;
    }
  }

  close(): void {
    this.closed = true;
    this.recovering = false;
    this.stopTimer();
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
  }

  private restart(): void {
    if (this.closed || !this.recovering) return;
    if (
      this.pc.iceConnectionState === "connected" ||
      this.pc.iceConnectionState === "completed"
    ) {
      this.handleState(this.pc.iceConnectionState);
      return;
    }
    if (
      this.pc.iceConnectionState === "closed" ||
      this.pc.signalingState === "closed"
    ) {
      this.close();
      return;
    }

    this.attempts += 1;
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
}
