import { useMemo } from "react";
import { cn } from "@/lib/utils";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  hexToHopHashes,
  buildPeerCandidatesByHash,
  resolveHopPeer,
  type PathPeer,
  type ResolvedHop as Hop,
} from "@/lib/linkPath";

export type { PathPeer };

function useHops(
  path: string | undefined,
  hashSize: number | undefined,
  peers: PathPeer[] | null,
): Hop[] {
  const size = Math.max(1, hashSize ?? 1);
  return useMemo(() => {
    if (!path) return [];
    const byHash = buildPeerCandidatesByHash(peers ?? [], size);
    return hexToHopHashes(path, size).map((h) => resolveHopPeer(h, byHash.get(h)));
  }, [path, size, peers]);
}

// An ambiguous hop opens on click rather than hover: a title attribute never appears on a touch
// screen, so on a phone the candidates would have been unreachable.
function HopName({ hop }: { hop: Hop }) {
  if (!hop.peer) {
    return (
      <span className="font-mono text-muted-foreground/60">
        {hop.hash.toUpperCase()}
      </span>
    );
  }
  if (hop.alternatives.length === 0) {
    return <span>{hop.peer.name}</span>;
  }

  const candidates = [hop.peer, ...hop.alternatives];
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={`Hop ${hop.hash.toUpperCase()} matches ${candidates.length} peers`}
          className="underline decoration-dotted decoration-muted-foreground/50 underline-offset-2 hover:decoration-foreground focus-visible:outline focus-visible:outline-1 focus-visible:outline-primary text-left"
        >
          {hop.peer.name}
          <span className="text-muted-foreground/60"> +{hop.alternatives.length}</span>
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-72 p-0">
        <div className="border-b border-border px-3 py-2">
          <span className="label-overline">Hop {hop.hash.toUpperCase()}</span>
          <p className="mt-1 text-xs text-muted-foreground">
            {candidates.length} peers share this hash. The packet carries only the
            hash, so which one forwarded it is not recorded.
          </p>
        </div>
        <ul className="max-h-56 overflow-y-auto py-1">
          {candidates.map((c, i) => (
            <li
              key={c.pubkey}
              className="px-3 py-1.5 text-xs flex items-baseline gap-2"
            >
              <span className="flex-1 break-words">{c.name}</span>
              {i === 0 && (
                <span className="shrink-0 font-mono text-[10px] uppercase tracking-[0.08em] text-primary">
                  shown
                </span>
              )}
              <span className="shrink-0 font-mono text-[10px] text-muted-foreground/50">
                {c.type === "REPEATER" ? "RPT" : (c.type ?? "").slice(0, 3)}
              </span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}

// HopPath renders a packet's recorded path: the raw hop hashes it actually carries, and underneath,
// the peers those hashes most likely name. The hashes are the fact and stay visible; the names are
// inference, and an ambiguous one is underlined with the count of other peers it could equally be.
export function HopPath({
  path,
  hashSize,
  direction,
  route,
  peers,
  compact,
  className,
}: {
  path?: string;
  hashSize?: number;
  direction?: string;
  route?: string;
  peers: PathPeer[] | null;
  compact?: boolean;
  className?: string;
}) {
  const hops = useHops(path, hashSize, peers);

  if (hops.length === 0) {
    return (
      <span className={cn("text-muted-foreground/60", className)}>
        No hops recorded
      </span>
    );
  }

  const chain = hops.map((h, i) => (
    <span key={`${h.hash}-${i}`}>
      {i > 0 && <span className="text-muted-foreground/40"> → </span>}
      <HopName hop={h} />
    </span>
  ));
  const us = <span className="text-muted-foreground/60">you</span>;

  // Where we sit on the chain, and whether we sit on it at all. A flood ACCUMULATES a hash at each
  // relay, so a received one lists where the packet has been and it reached us from the last of
  // them: "chain → you". Our own send leads: "you → chain". A direct route CONSUMES its hashes
  // (Mesh.cpp:334-342), so a received one carries only the road ahead — a leg running from the
  // relay we overheard to its next hop, with us on neither end. That gets no "you" at all: naming
  // us at the head would claim we are forwarding it on, which is as wrong as the tail claiming the
  // next hop delivered it to us.
  const forward = direction === "tx" || route?.includes("DIRECT");
  const lead = direction === "tx" ? us : null;
  const tail = forward ? null : us;
  const ahead = route?.includes("DIRECT") && direction !== "tx";

  const body = (
    <>
      {lead && (
        <>
          {lead}
          <span className="text-muted-foreground/40"> → </span>
        </>
      )}
      {chain}
      {tail && (
        <>
          <span className="text-muted-foreground/40"> → </span>
          {tail}
        </>
      )}
      {ahead && (
        <span className="text-muted-foreground/50"> (still to go)</span>
      )}
    </>
  );

  if (compact) {
    return <span className={cn("text-[11px]", className)}>{body}</span>;
  }

  return (
    <div className={cn("space-y-1.5", className)}>
      <div className="flex flex-wrap gap-1">
        {hops.map((h, i) => (
          <span
            key={`${h.hash}-${i}`}
            className="border border-border px-1.5 py-0.5 font-mono text-[10px] uppercase tabular-nums text-muted-foreground"
          >
            {h.hash}
          </span>
        ))}
      </div>
      <div className="text-xs leading-relaxed">{body}</div>
    </div>
  );
}
