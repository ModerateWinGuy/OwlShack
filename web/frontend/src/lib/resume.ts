import { useEffect, useRef } from "react";

// After its first fetch a view is only ever updated by the socket, so whatever happened while the
// tab slept is simply absent — which looks exactly like a quiet mesh. Waking the tab and re-opening
// a dropped socket both raise this, and the live views re-fetch on it.

const listeners = new Set<() => void>();

export function notifyResume(): void {
  for (const fn of [...listeners]) fn();
}

let wasHidden = document.visibilityState === "hidden";
document.addEventListener("visibilitychange", () => {
  const hidden = document.visibilityState === "hidden";
  // A socket that survived the sleep still loses anything the hub dropped from a full send buffer.
  if (wasHidden && !hidden) notifyResume();
  wasHidden = hidden;
});
window.addEventListener("online", notifyResume);

export function useResume(onResume: () => void): void {
  const latest = useRef(onResume);
  latest.current = onResume;
  useEffect(() => {
    const fn = () => latest.current();
    listeners.add(fn);
    return () => {
      listeners.delete(fn);
    };
  }, []);
}
