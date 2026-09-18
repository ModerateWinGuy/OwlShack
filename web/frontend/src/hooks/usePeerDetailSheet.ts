import { useMemo, useState } from "react";
import type { PeerLike } from "@/components/PeerDetailSheet";

// Spread `sheetProps` onto <PeerDetailSheet> and add `companions` at the call site.
export function usePeerDetailSheet<T extends PeerLike>(peers: T[]) {
  const [selectedKey, setSelectedKey] = useState<string | null>(null);

  const selected = useMemo(
    () => peers.find((p) => p.pubkey === selectedKey) ?? null,
    [peers, selectedKey],
  );

  return {
    selectPeer: setSelectedKey,
    sheetProps: {
      peer: selected,
      peers,
      open: !!selectedKey,
      onOpenChange: (open: boolean) => {
        if (!open) setSelectedKey(null);
      },
    },
  };
}
