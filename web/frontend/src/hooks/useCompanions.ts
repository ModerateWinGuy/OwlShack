import { useEffect, useMemo, useState } from "react";

import { companionIdFromRef, findByRef } from "@/lib/companionRef";
import { useCompanionsChanged } from "@/lib/companionsChanged";

// A companion reference as exposed by GET /api/companions.
export interface CompanionRef {
  id: number;
  name: string;
  pubkey?: string;
}

// Module-level stale-while-revalidate cache: many pages fetch /api/companions independently.
let cache: CompanionRef[] | null = null;
let inflight: Promise<CompanionRef[]> | null = null;
const subscribers = new Set<(c: CompanionRef[]) => void>();

function fetchCompanions(): Promise<CompanionRef[]> {
  if (inflight) return inflight;
  inflight = fetch("/api/companions")
    .then((r) => (r.ok ? r.json() : []))
    .then((cs: CompanionRef[]) => {
      cache = cs || [];
      subscribers.forEach((fn) => fn(cache!));
      return cache;
    })
    .catch(() => cache ?? [])
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

export function useCompanions(): CompanionRef[] {
  const [companions, setCompanions] = useState<CompanionRef[]>(cache ?? []);

  useEffect(() => {
    let active = true;
    const update = (c: CompanionRef[]) => {
      if (active) setCompanions(c);
    };
    subscribers.add(update);
    // Revalidate on every mount, so a mutation elsewhere self-heals.
    fetchCompanions().then(update);
    return () => {
      subscribers.delete(update);
      active = false;
    };
  }, []);

  // A rename does not remount these pages, so mount-time revalidation alone leaves a stale name.
  useCompanionsChanged(refreshCompanions);

  return companions;
}

function refreshCompanions(): void {
  inflight = null;
  fetchCompanions();
}

// useCompanionRef reads the :ref route segment. Use `ref` for links and API paths, where it must
// survive a rename, and `name` wherever a person reads it or it is compared against a sender - the
// two stop being interchangeable the moment a companion is renamed.
//
// `name` is empty until the companion list resolves, and stays the segment itself for a plain-name
// link made before refs existed.
export function useCompanionRef(ref: string | undefined): {
  ref: string;
  id: number | null;
  name: string;
} {
  const companions = useCompanions();
  return useMemo(() => {
    const seg = ref ?? "";
    const c = findByRef(seg, companions);
    if (c) return { ref: seg, id: c.id, name: c.name };
    // Unresolved: a plain-name link is its own name, an id ref has none until the list arrives.
    const id = companionIdFromRef(seg);
    return { ref: seg, id, name: id == null ? seg : "" };
  }, [ref, companions]);
}
