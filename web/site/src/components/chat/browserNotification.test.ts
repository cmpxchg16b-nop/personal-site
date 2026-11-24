import { afterEach, describe, expect, it, vi } from "vitest";
import { showBrowserNotification } from "./browserNotification";

class MockNotification {
  static permission: NotificationPermission = "default";
  static requestPermission: () => Promise<NotificationPermission> = vi.fn();
  static instances: MockNotification[] = [];

  readonly close = vi.fn();
  onclick: (() => void) | null = null;

  constructor(
    readonly title: string,
    readonly options: NotificationOptions,
  ) {
    MockNotification.instances.push(this);
  }
}

const focus = vi.fn();

function stubNotification(permission: NotificationPermission) {
  MockNotification.permission = permission;
  MockNotification.instances = [];
  MockNotification.requestPermission = vi.fn();
  focus.mockReset();
  vi.stubGlobal("window", { Notification: MockNotification, focus });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("showBrowserNotification", () => {
  it("shows a granted notification and focuses the window on click", () => {
    stubNotification("granted");

    const cleanup = showBrowserNotification("Alice", {
      body: "Incoming call",
      tag: "call-1",
    });

    expect(MockNotification.instances).toHaveLength(1);
    const notification = MockNotification.instances[0];
    expect(notification.title).toBe("Alice");
    expect(notification.options).toEqual({
      body: "Incoming call",
      tag: "call-1",
    });

    notification.onclick?.();
    expect(focus).toHaveBeenCalledOnce();
    expect(notification.close).toHaveBeenCalledOnce();
    cleanup();
    expect(notification.close).toHaveBeenCalledTimes(2);
  });

  it("shows a default-permission notification only after permission is granted", async () => {
    stubNotification("default");
    let grant: (permission: NotificationPermission) => void = () => {};
    MockNotification.requestPermission = vi.fn(
      () =>
        new Promise<NotificationPermission>((resolve) => {
          grant = resolve;
        }),
    );

    const cleanup = showBrowserNotification("Alice", {});
    expect(MockNotification.instances).toHaveLength(0);
    expect(MockNotification.requestPermission).toHaveBeenCalledOnce();

    MockNotification.permission = "granted";
    grant("granted");
    await Promise.resolve();
    expect(MockNotification.instances).toHaveLength(1);
    cleanup();
  });

  it("does not show a notification when a permission request is denied", async () => {
    stubNotification("default");
    MockNotification.requestPermission = vi.fn(
      async (): Promise<NotificationPermission> => "denied",
    );

    const cleanup = showBrowserNotification("Alice", {});
    await Promise.resolve();

    expect(MockNotification.requestPermission).toHaveBeenCalledOnce();
    expect(MockNotification.instances).toHaveLength(0);
    cleanup();
  });

  it("does not request permission again when it was already denied", () => {
    stubNotification("denied");

    const cleanup = showBrowserNotification("Alice", {});

    expect(MockNotification.requestPermission).not.toHaveBeenCalled();
    expect(MockNotification.instances).toHaveLength(0);
    cleanup();
  });

  it("suppresses a notification whose call ended before permission resolved", async () => {
    stubNotification("default");
    let grant: (permission: NotificationPermission) => void = () => {};
    MockNotification.requestPermission = vi.fn(
      () =>
        new Promise<NotificationPermission>((resolve) => {
          grant = resolve;
        }),
    );

    const cleanup = showBrowserNotification("Alice", {});
    cleanup();
    MockNotification.permission = "granted";
    grant("granted");
    await Promise.resolve();

    expect(MockNotification.instances).toHaveLength(0);
  });

  it("keeps the in-page UI usable when the API is unavailable", () => {
    vi.stubGlobal("window", { focus });

    expect(() => {
      const cleanup = showBrowserNotification("Alice", {});
      cleanup();
    }).not.toThrow();
  });

  it("contains constructor and permission-request failures", async () => {
    stubNotification("default");
    MockNotification.requestPermission = vi.fn(async () => {
      throw new Error("restricted");
    });

    const cleanup = showBrowserNotification("Alice", {});
    await Promise.resolve();
    await Promise.resolve();
    expect(MockNotification.instances).toHaveLength(0);
    cleanup();

    stubNotification("granted");
    vi.stubGlobal("window", {
      Notification: class {
        static permission = "granted";
        constructor() {
          throw new Error("unsupported");
        }
      },
      focus,
    });
    expect(() => showBrowserNotification("Alice", {})).not.toThrow();
  });
});
