import { useEffect, useRef } from "react";

// Config has no WS topic, so a rename is invisible to anything that already fetched the roster:
// the sidebar kept showing the old name until a full page reload. Every companion mutation calls
// notifyCompanionsChanged, and the views that cache a roster re-fetch.

const listeners = new Set<() => void>();

export function notifyCompanionsChanged(): void {
  for (const fn of [...listeners]) fn();
}

export function useCompanionsChanged(onChange: () => void): void {
  const latest = useRef(onChange);
  latest.current = onChange;
  useEffect(() => {
    const fn = () => latest.current();
    listeners.add(fn);
    return () => {
      listeners.delete(fn);
    };
  }, []);
}
