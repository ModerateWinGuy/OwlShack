import { cn } from "@/lib/utils";

interface ConnectionDotProps {
  connected: boolean;
  // Distinguishes a handshake still in flight from a link that dropped; without it every mount paints red.
  pending?: boolean;
  className?: string;
}

export function ConnectionPill({
  connected,
  pending,
  className,
}: ConnectionDotProps) {
  const state = connected ? "live" : pending ? "connecting" : "offline";
  return (
    <div
      className={cn(
        "inline-flex items-center gap-1.5 px-2 py-0.5 border font-mono text-[10px] uppercase tracking-[0.12em]",
        state === "live" && "border-success/40 text-success bg-success/5",
        state === "connecting" &&
          "border-border text-muted-foreground bg-muted/20",
        state === "offline" &&
          "border-destructive/40 text-destructive bg-destructive/5",
        className,
      )}
    >
      <span
        className={cn(
          "h-1.5 w-1.5 rounded-full",
          state === "live" && "bg-success scan-pulse",
          state === "connecting" && "bg-muted-foreground",
          state === "offline" && "bg-destructive",
        )}
      />
      {state}
    </div>
  );
}

interface PeerTypePillProps {
  type: string;
  className?: string;
}

const TYPE_TONE: Record<string, string> = {
  CHAT: "text-emerald-400 border-emerald-500/30 bg-emerald-500/5",
  REPEATER: "text-amber-400 border-amber-500/30 bg-amber-500/5",
  ROOM: "text-violet-400 border-violet-500/30 bg-violet-500/5",
  SENSOR: "text-cyan-400 border-cyan-500/30 bg-cyan-500/5",
  NONE: "text-muted-foreground border-border bg-muted/40",
};

export function PeerTypePill({ type, className }: PeerTypePillProps) {
  const tone = TYPE_TONE[type] ?? TYPE_TONE.NONE;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 px-1.5 py-0.5 border font-mono text-[10px] uppercase tracking-[0.08em]",
        tone,
        className,
      )}
    >
      {type}
    </span>
  );
}

export const PEER_TYPE_HEX: Record<string, string> = {
  CHAT: "#34d399",
  REPEATER: "#fbbf24",
  ROOM: "#a78bfa",
  SENSOR: "#22d3ee",
  NONE: "#94a3b8",
};
