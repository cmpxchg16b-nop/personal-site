import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AudioGraph } from "./audiograph";
import { loadMediaSettings, saveMediaSettings } from "../media/settings";

function mediaTrack(kind: "audio" | "video", deviceId: string = kind) {
  return {
    kind,
    stop: vi.fn(),
    getSettings: () => ({ deviceId }),
    addEventListener: vi.fn(),
  } as unknown as MediaStreamTrack;
}

function mediaStream(track: MediaStreamTrack): MediaStream {
  return {
    getTracks: () => [track],
    getAudioTracks: () => (track.kind === "audio" ? [track] : []),
    getVideoTracks: () => (track.kind === "video" ? [track] : []),
  } as unknown as MediaStream;
}

interface MockNode {
  connect: ReturnType<typeof vi.fn>;
  disconnect: ReturnType<typeof vi.fn>;
}

interface MockGain extends MockNode {
  gain: { value: number };
}

interface MockOscillator extends MockNode {
  frequency: { value: number };
  start: ReturnType<typeof vi.fn>;
  stop: ReturnType<typeof vi.fn>;
  onended: (() => void) | null;
}

const contexts: MockAudioContext[] = [];

class MockAudioContext {
  state: AudioContextState = "running";
  readonly sampleRate = 48_000;
  readonly currentTime = 10;
  readonly destination = { name: "system-output" };
  readonly gains: MockGain[] = [];
  readonly analysers: (AnalyserNode & MockNode)[] = [];
  readonly sources: ({ stream: MediaStream } & MockNode)[] = [];
  readonly destinations: { stream: MediaStream }[] = [];
  readonly oscillators: MockOscillator[] = [];
  readonly addEventListener = vi.fn();
  readonly resume = vi.fn(async () => {
    this.state = "running";
  });
  readonly close = vi.fn(async () => {
    this.state = "closed";
  });

  constructor() {
    contexts.push(this);
  }

  createGain(): GainNode {
    const gain: MockGain = {
      gain: { value: 1 },
      connect: vi.fn(),
      disconnect: vi.fn(),
    };
    this.gains.push(gain);
    return gain as unknown as GainNode;
  }

  createAnalyser(): AnalyserNode {
    const analyser = {
      fftSize: 0,
      smoothingTimeConstant: 0,
      connect: vi.fn(),
      disconnect: vi.fn(),
    };
    this.analysers.push(analyser as unknown as AnalyserNode & MockNode);
    return analyser as unknown as AnalyserNode;
  }

  createMediaStreamSource(stream: MediaStream): MediaStreamAudioSourceNode {
    const source = { stream, connect: vi.fn(), disconnect: vi.fn() };
    this.sources.push(source);
    return source as unknown as MediaStreamAudioSourceNode;
  }

  createMediaStreamDestination(): MediaStreamAudioDestinationNode {
    const destination = { stream: mediaStream(mediaTrack("audio", "wire")) };
    this.destinations.push(destination);
    return destination as unknown as MediaStreamAudioDestinationNode;
  }

  createOscillator(): OscillatorNode {
    const oscillator: MockOscillator = {
      frequency: { value: 0 },
      connect: vi.fn(),
      disconnect: vi.fn(),
      start: vi.fn(),
      stop: vi.fn(),
      onended: null,
    };
    this.oscillators.push(oscillator);
    return oscillator as unknown as OscillatorNode;
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const storage = new Map<string, string>();
const getUserMedia = vi.fn();

beforeEach(() => {
  contexts.length = 0;
  storage.clear();
  getUserMedia.mockReset();
  vi.stubGlobal("AudioContext", MockAudioContext);
  vi.stubGlobal("window", {
    localStorage: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => storage.set(key, value),
    },
  });
  vi.stubGlobal("navigator", { mediaDevices: { getUserMedia } });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AudioGraph media settings", () => {
  it("loads selected mic and volumes, applies gains and persists both controls", async () => {
    saveMediaSettings({
      microphoneId: "mic-a",
      microphoneVolume: 0.3,
      speakerVolume: 0.4,
    });
    const rawTrack = mediaTrack("audio", "mic-a");
    getUserMedia.mockResolvedValue(mediaStream(rawTrack));
    const graph = new AudioGraph();
    const listener = vi.fn();
    const unsubscribe = graph.subscribeVolumes(listener);

    graph.resume();
    await graph.acquireLocalInput();
    const context = contexts[0];
    expect(getUserMedia).toHaveBeenCalledWith({
      audio: {
        deviceId: { exact: "mic-a" },
        echoCancellation: false,
        noiseSuppression: false,
        autoGainControl: false,
      },
    });
    expect(context.gains[0]?.gain.value).toBe(0.4);
    expect(context.gains[1]?.gain.value).toBe(0.3);

    graph.setLocalVolume(0.6);
    graph.setRemoteVolume(0.2);
    expect(context.gains[0]?.gain.value).toBe(0.2);
    expect(context.gains[1]?.gain.value).toBe(0.6);
    expect(listener).toHaveBeenCalledTimes(2);
    expect(loadMediaSettings()).toMatchObject({
      microphoneId: "mic-a",
      microphoneVolume: 0.6,
      speakerVolume: 0.2,
    });
    unsubscribe();
    graph.release();
  });

  it("switches the raw microphone without replacing the processed wire track", async () => {
    const oldTrack = mediaTrack("audio", "default");
    const newTrack = mediaTrack("audio", "mic-b");
    getUserMedia
      .mockResolvedValueOnce(mediaStream(oldTrack))
      .mockResolvedValueOnce(mediaStream(newTrack));
    const graph = new AudioGraph();
    const input = await graph.acquireLocalInput();
    const context = contexts[0];
    const analyser = graph.localAnalyser;

    await graph.setMicrophoneDevice("mic-b");
    expect(context.destinations[0]?.stream.getAudioTracks()[0]).toBe(
      input.track,
    );
    expect(graph.localAnalyser).toBe(analyser);
    expect(context.sources[1]?.stream.getAudioTracks()[0]).toBe(newTrack);
    expect(oldTrack.stop).toHaveBeenCalledOnce();
    expect(newTrack.stop).not.toHaveBeenCalled();
    expect(getUserMedia).toHaveBeenLastCalledWith({
      audio: expect.objectContaining({ deviceId: { exact: "mic-b" } }),
    });
    graph.releaseLocalInput();
    expect(newTrack.stop).toHaveBeenCalledOnce();
    graph.release();
  });

  it("retries a pending initial capture when the selection changes", async () => {
    const pending = deferred<MediaStream>();
    const obsoleteTrack = mediaTrack("audio", "default");
    const selectedTrack = mediaTrack("audio", "mic-b");
    getUserMedia
      .mockReturnValueOnce(pending.promise)
      .mockResolvedValueOnce(mediaStream(selectedTrack));
    const graph = new AudioGraph();
    const acquisition = graph.acquireLocalInput();
    await graph.setMicrophoneDevice("mic-b");
    pending.resolve(mediaStream(obsoleteTrack));

    await acquisition;
    expect(obsoleteTrack.stop).toHaveBeenCalledOnce();
    expect(contexts[0].sources[0]?.stream.getAudioTracks()[0]).toBe(
      selectedTrack,
    );
    graph.release();
  });

  it("routes the speaker test through the persisted application gain and post-gain tap", () => {
    const graph = new AudioGraph();
    graph.resume();
    const context = contexts[0];
    graph.setRemoteVolume(0.3);
    graph.startSpeakerTest();

    const remoteGain = context.gains[0];
    const outputTap = context.analysers[0];
    const envelope = context.gains[1];
    expect(remoteGain?.gain.value).toBe(0.3);
    expect(remoteGain?.connect).toHaveBeenCalledWith(outputTap);
    expect(outputTap?.connect).toHaveBeenCalledWith(context.destination);
    expect(envelope?.connect).toHaveBeenCalledWith(remoteGain);
    expect(context.oscillators[0]?.frequency.value).toBe(660);
    expect(context.oscillators[0]?.stop).not.toHaveBeenCalled();
    graph.stopSpeakerTest();
    expect(context.oscillators[0]?.stop).toHaveBeenCalledOnce();
    graph.setRemoteVolume(0);
    expect(remoteGain?.gain.value).toBe(0);
    graph.release();
  });
});
