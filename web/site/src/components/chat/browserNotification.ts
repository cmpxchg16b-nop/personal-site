// showBrowserNotification adds native browser chrome without making it a
// prerequisite for the corresponding in-page UI. The returned cleanup closes
// a visible notification and suppresses one whose permission is still pending.
export function showBrowserNotification(
  title: string,
  options: NotificationOptions,
): () => void {
  if (typeof window === "undefined") return () => {};
  const NotificationAPI = window.Notification;
  if (typeof NotificationAPI !== "function") return () => {};

  let disposed = false;
  let notification: Notification | null = null;

  const show = () => {
    if (disposed || NotificationAPI.permission !== "granted") return;
    try {
      const current = new NotificationAPI(title, options);
      notification = current;
      current.onclick = () => {
        window.focus();
        current.close();
      };
    } catch {
      // The in-page notification remains available if native construction is
      // blocked by the browser or platform.
    }
  };

  if (NotificationAPI.permission === "granted") {
    show();
  } else if (NotificationAPI.permission === "default") {
    try {
      void NotificationAPI.requestPermission().then(
        (permission) => {
          if (permission === "granted") show();
        },
        () => {},
      );
    } catch {
      // Permission requests can also fail synchronously in restricted contexts.
    }
  }

  return () => {
    disposed = true;
    notification?.close();
    notification = null;
  };
}
