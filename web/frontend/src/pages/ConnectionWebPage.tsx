import {
  useCallback,
  useDeferredValue,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import L from "leaflet";
import { RefreshCw, TriangleAlert, X } from "lucide-react";
import { toast } from "sonner";
import { useApiObject } from "@/hooks/useApiObject";
import { useWebSocket } from "@/hooks/useWebSocket";
import { useResume } from "@/lib/resume";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetTitle,
} from "@/components/ui/sheet";
import { LoadErrorAlert } from "@/components/LoadErrorAlert";
import { PageHeader } from "@/components/PageHeader";
import { ConnectionPill, PEER_TYPE_HEX } from "@/components/StatusIndicator";
import { snrFill, snrTextClass } from "@/components/SignalStrength";
import { originIcon } from "@/components/DiscoverMap";
import {
  peerLatLon,
  themeTileLayer,
  useThemeTiles,
  wrapLon,
} from "@/lib/leaflet";
import { timeAgo } from "@/lib/format";
import { cn } from "@/lib/utils";
import {
  type ConnectionWeb,
  type WebLink,
  type WebNode,
  foldLinks,
  isUncertain,
  linkKey,
  pinHop,
  routesThrough,
  SELF_ID,
  unpinHop,
} from "@/lib/connectionWeb";

const WINDOWS = [
  { hours: 1, label: "1h" },
  { hours: 6, label: "6h" },
  { hours: 24, label: "24h" },
  { hours: 72, label: "3d" },
  { hours: 168, label: "7d" },
  { hours: 720, label: "30d" },
];
// The slider hides a link carrying less than this percent of the busiest link's packets. Relative,
// so the same setting thins a quiet hour and a busy week alike, and the busiest link always survives.
const MAX_HIDE_PCT = 25;
// The page reads the packet log, not the radio, so following live traffic costs no airtime.
const REFETCH_MS = 60_000;
// A relay beside us can sit on hundreds of routes; rendering them all at once froze the sheet.
const ROUTES_PAGE = 10;
const NOT_MEASURED = "var(--muted-foreground)";
// Dash then gap; the flow animation in index.css shifts by exactly this period.
const LINK_DASH = "10 8";

type Selection =
  { kind: "link"; a: string; b: string } | { kind: "node"; id: string } | null;

function dotIcon(color: string, ambiguous: boolean): L.DivIcon {
  const ring = ambiguous
    ? "outline:2px dashed var(--foreground);outline-offset:3px;"
    : "";
  return L.divIcon({
    className: "meshcore-web-dot",
    html: `<span style="display:grid;place-items:center;width:24px;height:24px;"><span style="display:block;width:12px;height:12px;border-radius:9999px;background:${color};box-shadow:0 0 0 2px rgba(0,0,0,0.55);${ring}"></span></span>`,
    iconSize: [24, 24],
    iconAnchor: [12, 12],
  });
}

const ORIGIN_ICON = originIcon();

const located = (n: WebNode | undefined) => !!n && (n.lat !== 0 || n.lon !== 0);
const pct = (v: number) => `${Math.round(v * 100)}%`;

function km(a: [number, number] | null, b: [number, number] | null): string {
  if (!a || !b) return "unknown";
  const d = L.latLng(a).distanceTo(L.latLng(b)) / 1000;
  return `${d < 10 ? d.toFixed(1) : Math.round(d)} km`;
}

export function ConnectionWebPage() {
  const [hours, setHours] = useState(24);
  // Starts strict: unfiltered, a busy mesh is a tangle of one-off links.
  const [minShare, setMinShare] = useState(0.1);
  const [via, setVia] = useState<string | null>(null);
  const [selection, setSelection] = useState<Selection>(null);

  const {
    item: web,
    loading,
    error,
    reload,
  } = useApiObject<ConnectionWeb>(
    `/api/connection-web?hours=${hours}`,
    "Failed to load the connection web",
  );
  useResume(reload);

  const pending = useRef<number | null>(null);
  const handleMessage = useCallback(
    (topic: string) => {
      if (topic !== "packets" || pending.current != null) return;
      pending.current = window.setTimeout(() => {
        pending.current = null;
        reload();
      }, REFETCH_MS);
    },
    [reload],
  );
  useEffect(() => () => window.clearTimeout(pending.current ?? undefined), []);
  const { connected } = useWebSocket(["packets"], handleMessage);

  const nodes = useMemo(() => {
    const m = new Map<string, WebNode>();
    for (const n of web?.nodes ?? []) m.set(n.id, n);
    return m;
  }, [web]);
  const chains = useMemo(() => web?.chains ?? [], [web]);
  const links = useMemo(
    () => foldLinks(chains, via ?? undefined),
    [chains, via],
  );

  // One line per node pair, drawn in the direction most of its traffic travels; a pair carries
  // traffic both ways when routes differ, and then the busier direction wins the arrow of the dashes.
  const allPairs = useMemo(() => {
    const out = new Map<
      string,
      {
        a: string;
        b: string;
        count: number;
        heavier: number;
        share: number;
        snr: number | null;
      }
    >();
    for (const l of links.values()) {
      const key = linkKey(...([l.from, l.to].sort() as [string, string]));
      const p = out.get(key) ?? {
        a: l.from,
        b: l.to,
        count: 0,
        heavier: 0,
        share: 0,
        snr: null,
      };
      p.count += l.count;
      p.share = Math.max(p.share, l.shareOut);
      if (l.count > p.heavier) {
        p.heavier = l.count;
        p.a = l.from;
        p.b = l.to;
      }
      if (l.snrN > 0) p.snr = l.snrSum / l.snrN;
      out.set(key, p);
    }
    return [...out.values()];
  }, [links]);

  // Deferred so dragging the slider stays smooth while a large mesh redraws behind it.
  const filterShare = useDeferredValue(minShare);
  const pairs = useMemo(() => {
    const busiest = Math.max(...allPairs.map((p) => p.count), 0);
    return allPairs.filter((p) => p.count >= filterShare * busiest);
  }, [allPairs, filterShare]);

  const shownNodeIds = useMemo(() => {
    const s = new Set<string>();
    for (const p of pairs) {
      s.add(p.a);
      s.add(p.b);
    }
    s.delete(SELF_ID);
    return s;
  }, [pairs]);

  const unplaced = useMemo(
    () =>
      [...shownNodeIds]
        .map((id) => nodes.get(id))
        .filter((n): n is WebNode => !!n && !located(n))
        .sort((a, b) => b.observations - a.observations),
    [shownNodeIds, nodes],
  );

  const uncertain = useMemo(
    () =>
      [...shownNodeIds]
        .map((id) => nodes.get(id))
        .filter((n): n is WebNode => !!n && isUncertain(n))
        .sort((a, b) => b.observations - a.observations),
    [shownNodeIds, nodes],
  );

  const openNode = useCallback((id: string) => {
    setVia(id);
    setSelection({ kind: "node", id });
  }, []);

  // pubkey: a candidate's key, null for "none of these", undefined to hand the hash back to the
  // distance pick. The node's id changes with its owner, so the sheet and filter follow it.
  const repin = useCallback(
    async (oldId: string, hash: string, pubkey: string | null | undefined) => {
      try {
        if (pubkey === undefined) await unpinHop(hash);
        else await pinHop(hash, pubkey);
      } catch (e) {
        toast.error(
          `Could not change the match: ${e instanceof Error ? e.message : "error"}`,
        );
        return;
      }
      const newId = pubkey === undefined ? null : (pubkey ?? `h:${hash}`);
      setVia((v) => (v === oldId ? newId : v));
      setSelection(newId ? { kind: "node", id: newId } : null);
      reload();
    },
    [reload],
  );

  const totalObservations = useMemo(
    () => chains.reduce((s, c) => s + c.count, 0),
    [chains],
  );

  const posOf = useCallback(
    (id: string): [number, number] | null => {
      // Wrapped like every peer: a configured -185 is 175E, and unwrapped it lands a world copy away.
      if (id === SELF_ID)
        return web?.self ? [web.self.lat, wrapLon(web.self.lon)] : null;
      const n = nodes.get(id);
      return located(n) ? peerLatLon(n!.lat, n!.lon) : null;
    },
    [nodes, web],
  );

  const containerRef = useRef<HTMLDivElement | null>(null);
  const mapRef = useRef<L.Map | null>(null);
  const tileLayerRef = useRef<L.TileLayer | null>(null);
  const layerRef = useRef<L.LayerGroup | null>(null);
  const fittedRef = useRef(false);

  useEffect(() => {
    if (!containerRef.current || mapRef.current) return;
    const map = L.map(containerRef.current, {
      center: [0, 0],
      zoom: 2,
      worldCopyJump: true,
    });
    tileLayerRef.current = themeTileLayer().addTo(map);
    layerRef.current = L.layerGroup().addTo(map);
    mapRef.current = map;
    return () => {
      map.remove();
      mapRef.current = null;
      tileLayerRef.current = null;
      layerRef.current = null;
      fittedRef.current = false;
    };
  }, []);

  useThemeTiles(mapRef, tileLayerRef);

  // Full clear-and-redraw: a refetch replaces every number, so there is nothing to diff against.
  useEffect(() => {
    const map = mapRef.current;
    const layer = layerRef.current;
    if (!map || !layer) return;
    layer.clearLayers();

    // Brightness carries volume on a log scale, because one busy link can out-count the rest of
    // the mesh together and a linear scale would leave everything else invisible.
    const busiest = Math.max(...pairs.map((p) => p.count), 1);
    const drawn = [...pairs].sort((x, y) => x.share - y.share);
    for (const p of drawn) {
      const a = posOf(p.a);
      const b = posOf(p.b);
      if (!a || !b) continue;
      const weight = 1.5 + 5 * p.share;
      L.polyline([a, b], {
        color: p.snr != null ? snrFill(p.snr) : NOT_MEASURED,
        weight,
        opacity: 0.25 + 0.65 * (Math.log(p.count + 1) / Math.log(busiest + 1)),
        dashArray: LINK_DASH,
        // Round caps would swell each dash into a bead on a wide line.
        lineCap: "butt",
        className: "meshcore-web-link",
        interactive: false,
      }).addTo(layer);
      // An invisible wide twin is the tap target: the drawn line can be 2px on a phone.
      L.polyline([a, b], {
        color: "#000",
        opacity: 0,
        weight: Math.max(16, weight + 12),
      })
        .on("click", () => setSelection({ kind: "link", a: p.a, b: p.b }))
        .addTo(layer);
    }

    const points: [number, number][] = [];
    for (const id of shownNodeIds) {
      const n = nodes.get(id);
      const at = posOf(id);
      if (!n || !at) continue;
      points.push(at);
      L.marker(at, {
        icon: dotIcon(
          PEER_TYPE_HEX[n.type ?? ""] ?? PEER_TYPE_HEX.NONE,
          isUncertain(n),
        ),
      })
        .bindTooltip(nodeName(n))
        .on("click", () => openNode(id))
        .addTo(layer);
    }
    const self = posOf(SELF_ID);
    if (self) {
      points.push(self);
      L.marker(self, { icon: ORIGIN_ICON, zIndexOffset: 1000 })
        .bindTooltip("You")
        .addTo(layer);
    }

    if (!fittedRef.current && points.length > 0) {
      map.fitBounds(L.latLngBounds(points), { padding: [40, 40], maxZoom: 12 });
      fittedRef.current = true;
    }
  }, [pairs, shownNodeIds, nodes, posOf, openNode]);

  const windows = useMemo(() => {
    const max = (web?.retentionDays ?? 7) * 24;
    const opts = WINDOWS.filter((w) => w.hours < max);
    return [...opts, { hours: max, label: `all (${max / 24}d)` }];
  }, [web?.retentionDays]);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Connection Web"
        meta={
          <span className="font-mono text-sm text-muted-foreground tabular-nums">
            {totalObservations} packets · {chains.length} routes
          </span>
        }
        actions={
          <>
            <Button
              variant="ghost"
              size="sm"
              onClick={reload}
              className="h-7 gap-1.5 px-2 font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground hover:text-primary"
            >
              <RefreshCw className={cn("size-3", loading && "animate-spin")} />
              refresh
            </Button>
            <ConnectionPill connected={connected} />
          </>
        }
      />

      {error && <LoadErrorAlert message={error} onRetry={reload} />}
      {web && !web.self && (
        <p className="panel px-4 py-3 font-mono text-xs text-muted-foreground">
          Set a position on your repeater or companion so links into you can be
          drawn.
        </p>
      )}

      <section className="panel overflow-hidden">
        <div className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-3">
          <label className="flex items-center gap-2">
            <span className="label-overline">Window</span>
            <Select
              value={String(hours)}
              onValueChange={(v) => setHours(Number(v))}
            >
              <SelectTrigger
                size="sm"
                className="w-28 rounded-none font-mono text-xs"
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="rounded-none font-mono text-xs">
                {windows.map((w) => (
                  <SelectItem
                    key={w.hours}
                    value={String(w.hours)}
                    className="rounded-none font-mono text-xs"
                  >
                    {w.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </label>
          <label className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="label-overline whitespace-nowrap">Hide under</span>
            <input
              type="range"
              min={0}
              max={MAX_HIDE_PCT}
              step={0.5}
              value={minShare * 100}
              onChange={(e) => setMinShare(Number(e.target.value) / 100)}
              aria-label="Hide links carrying less than this percent of the busiest link"
              className="w-32 accent-primary"
            />
            <span className="w-10 text-right font-mono text-xs tabular-nums">
              {+(minShare * 100).toFixed(1)}%
            </span>
            <span className="label-overline whitespace-nowrap">
              of busiest · {pairs.length}/{allPairs.length} links
            </span>
          </label>
          {via && (
            <button
              type="button"
              onClick={() => setVia(null)}
              className="inline-flex items-center gap-1.5 border border-primary/60 px-2 py-1 font-mono text-[10px] uppercase tracking-[0.12em] text-primary"
            >
              routes from {nodeName(nodes.get(via), via)}
              <X className="size-3" />
            </button>
          )}
          <Legend />
        </div>

        <div
          ref={containerRef}
          className="h-[calc(100dvh-300px-var(--bottom-nav))] min-h-105 w-full"
        />

        {uncertain.length > 0 && (
          <details className="border-t border-border px-4 py-3">
            <summary className="label-overline">
              {uncertain.length} hop{uncertain.length === 1 ? "" : "s"} matching
              more than one repeater (check these)
            </summary>
            <ul className="mt-2 divide-y divide-border">
              {uncertain.map((n) => (
                <li key={n.id}>
                  <button
                    type="button"
                    onClick={() => openNode(n.id)}
                    className="flex w-full items-center justify-between gap-3 py-2 text-left font-mono text-xs hover:text-primary"
                  >
                    <span className="truncate">
                      {nodeName(n)} · hash {n.hash?.toUpperCase()}
                    </span>
                    <span className="tabular-nums text-muted-foreground">
                      {n.candidates.length} matches · {n.observations} pkts
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          </details>
        )}

        {unplaced.length > 0 && (
          <details className="border-t border-border px-4 py-3">
            <summary className="label-overline">
              {unplaced.length} relay{unplaced.length === 1 ? "" : "s"} with no
              position (not drawn)
            </summary>
            <ul className="mt-2 divide-y divide-border">
              {unplaced.map((n) => (
                <li key={n.id}>
                  <button
                    type="button"
                    onClick={() => openNode(n.id)}
                    className="flex w-full items-center justify-between gap-3 py-2 text-left font-mono text-xs hover:text-primary"
                  >
                    <span className="truncate">{nodeName(n)}</span>
                    <span className="tabular-nums text-muted-foreground">
                      {n.observations} pkts
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          </details>
        )}
      </section>

      <Sheet
        open={selection != null}
        onOpenChange={(open) => !open && setSelection(null)}
      >
        <SheetContent
          side="right"
          className="w-full max-w-[100vw] overflow-y-auto border-l border-border bg-card p-0 sm:max-w-md"
        >
          <SheetTitle className="sr-only">Connection detail</SheetTitle>
          <SheetDescription className="sr-only">
            Statistics for the selected link or node.
          </SheetDescription>
          {selection?.kind === "link" && (
            <LinkDetail
              a={selection.a}
              b={selection.b}
              links={links}
              nodes={nodes}
              posOf={posOf}
              onOpenNode={openNode}
            />
          )}
          {selection?.kind === "node" && web && (
            <NodeDetail
              key={selection.id}
              id={selection.id}
              web={web}
              nodes={nodes}
              self={posOf(SELF_ID)}
              onRepin={repin}
            />
          )}
        </SheetContent>
      </Sheet>
    </div>
  );
}

function nodeName(n: WebNode | undefined, id?: string): string {
  if (id === SELF_ID) return "You";
  if (!n) return id ?? "?";
  return n.name || (n.hash ? `hash ${n.hash.toUpperCase()}` : n.id.slice(0, 8));
}

function Legend() {
  const swatch = (color: string, label: string) => (
    <span className="inline-flex items-center gap-1.5">
      <span
        className="inline-block h-0 w-5 border-t-2 border-dashed"
        style={{ borderColor: color }}
        aria-hidden
      />
      {label}
    </span>
  );
  return (
    <div className="ml-auto flex flex-wrap items-center gap-3 font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
      <span>width = how often a node picks this hop</span>
      <span>brightness = packets carried</span>
      <span>dashes flow toward the receiver</span>
      {swatch("var(--signal-strong)", "≥ 0 dB")}
      {swatch("var(--signal-weak)", "≥ −10 dB")}
      {swatch("var(--signal-dead)", "< −10 dB")}
      {swatch(NOT_MEASURED, "no SNR")}
      <span className="inline-flex items-center gap-1.5">
        <span
          className="inline-block size-2.5 rounded-full outline-2 outline-offset-2 outline-dashed outline-foreground"
          aria-hidden
        />
        several matches
      </span>
    </div>
  );
}

function Stat({
  label,
  children,
  className,
}: {
  label: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className="flex items-baseline justify-between gap-3 py-1">
      <span className="label-overline">{label}</span>
      <span className={cn("font-mono text-sm tabular-nums", className)}>
        {children}
      </span>
    </div>
  );
}

function LinkDetail({
  a,
  b,
  links,
  nodes,
  posOf,
  onOpenNode,
}: {
  a: string;
  b: string;
  links: Map<string, WebLink>;
  nodes: Map<string, WebNode>;
  posOf: (id: string) => [number, number] | null;
  onOpenNode: (id: string) => void;
}) {
  const dirs = [links.get(linkKey(a, b)), links.get(linkKey(b, a))].filter(
    (l): l is WebLink => !!l,
  );
  const name = (id: string) => nodeName(nodes.get(id), id);
  return (
    <div className="space-y-5 p-5">
      <div className="space-y-1">
        <h2 className="font-mono text-sm">
          {name(a)} ↔ {name(b)}
        </h2>
        <Stat label="Distance">{km(posOf(a), posOf(b))}</Stat>
        {[a, b]
          .map((id) => nodes.get(id))
          .filter((n): n is WebNode => !!n?.hash)
          .map((n) => (
            <button
              key={n.id}
              type="button"
              onClick={() => onOpenNode(n.id)}
              className={cn(
                "flex w-full items-center gap-2 py-1 text-left font-mono text-xs hover:text-primary",
                isUncertain(n) ? "text-warning" : "text-muted-foreground",
              )}
            >
              {isUncertain(n) && <TriangleAlert className="size-3 shrink-0" />}
              <span className="truncate">
                {nodeName(n)} is only known by hash {n.hash?.toUpperCase()} ·{" "}
                {n.candidates.length} match
                {n.candidates.length === 1 ? "" : "es"}
                {n.pinned ? " · pinned" : ""} · change
              </span>
            </button>
          ))}
      </div>
      {dirs.map((l) => {
        const feeders = [...links.values()]
          .filter((f) => f.to === l.from)
          .sort((x, y) => y.count - x.count);
        return (
          <section
            key={linkKey(l.from, l.to)}
            className="space-y-1 border-t border-border pt-3"
          >
            <h3 className="label-overline text-foreground">
              {name(l.from)} → {name(l.to)}
            </h3>
            <Stat label="Packets">{l.count}</Stat>
            <Stat label={`${name(l.from)}'s traffic sent here`}>
              {pct(l.shareOut)}
            </Stat>
            <Stat label={`Share of what reaches ${name(l.to)}`}>
              {pct(l.shareIn)}
            </Stat>
            <Stat label="Arrived first">{pct(l.first / l.count)}</Stat>
            <Stat label="Last seen">{timeAgo(l.lastSeen)}</Stat>
            {l.snrN > 0 ? (
              <>
                <Stat
                  label="SNR avg"
                  className={snrTextClass(l.snrSum / l.snrN)}
                >
                  {(l.snrSum / l.snrN).toFixed(1)} dB
                </Stat>
                <Stat label="SNR min / max">
                  {l.snrMin?.toFixed(1)} / {l.snrMax?.toFixed(1)} dB
                </Stat>
                {l.rssiN > 0 && (
                  <Stat label="RSSI avg">
                    {Math.round(l.rssiSum / l.rssiN)} dBm
                  </Stat>
                )}
              </>
            ) : (
              <p className="py-1 font-mono text-xs text-muted-foreground">
                No SNR: packets only carry the signal of the last hop into you.
              </p>
            )}
            {feeders.length > 0 && (
              <div className="pt-2">
                <span className="label-overline">Feeding {name(l.from)}</span>
                <ul className="mt-1 space-y-0.5">
                  {feeders.slice(0, 5).map((f) => (
                    <li
                      key={f.from}
                      className="flex justify-between font-mono text-xs"
                    >
                      <span className="truncate">{name(f.from)}</span>
                      <span className="tabular-nums text-muted-foreground">
                        {f.count} · {pct(f.shareIn)}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}

function NodeDetail({
  id,
  web,
  nodes,
  self,
  onRepin,
}: {
  id: string;
  web: ConnectionWeb;
  nodes: Map<string, WebNode>;
  self: [number, number] | null;
  onRepin: (
    oldId: string,
    hash: string,
    pubkey: string | null | undefined,
  ) => Promise<void>;
}) {
  const n = nodes.get(id);
  const routes = useMemo(() => routesThrough(web.chains, id), [web, id]);
  const name = (nid: string) => nodeName(nodes.get(nid), nid);
  const [busy, setBusy] = useState(false);
  const [shown, setShown] = useState(ROUTES_PAGE);
  const hidden = routes.slice(shown);
  const repin = async (pubkey: string | null | undefined) => {
    if (!n?.hash) return;
    setBusy(true);
    await onRepin(id, n.hash, pubkey);
    setBusy(false);
  };
  return (
    <div className="space-y-5 p-5">
      <div>
        <h2 className="font-mono text-sm">{name(id)}</h2>
        <p className="font-mono text-xs text-muted-foreground">
          {n?.type ?? "unknown type"}
          {n?.hash && ` · hash ${n.hash.toUpperCase()}`}
        </p>
      </div>
      {n?.hash && (
        <section className="space-y-2 border-t border-border pt-3">
          <h3 className="label-overline">
            Which repeater is hash {n.hash.toUpperCase()}?
          </h3>
          <p className="font-mono text-xs text-muted-foreground">
            {identityNote(n)}
          </p>
          <ul className="divide-y divide-border">
            {n.candidates.map((c) => (
              <li
                key={c.id}
                className="flex items-center justify-between gap-3 py-2"
              >
                <div className="min-w-0">
                  <div className="truncate font-mono text-xs">
                    {c.name || c.id.slice(0, 8)}
                  </div>
                  <div className="font-mono text-[10px] text-muted-foreground">
                    {km(
                      self,
                      c.lat !== 0 || c.lon !== 0
                        ? peerLatLon(c.lat, c.lon)
                        : null,
                    )}{" "}
                    from you · heard {timeAgo(c.lastSeen)}
                  </div>
                </div>
                {c.id === n.id ? (
                  <span className="label-overline shrink-0 text-primary">
                    {n.pinned ? "pinned" : "picked"}
                  </span>
                ) : (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() => repin(c.id)}
                    className="shrink-0 rounded-none font-mono text-[10px] uppercase tracking-[0.12em]"
                  >
                    use this
                  </Button>
                )}
              </li>
            ))}
          </ul>
          <div className="flex flex-wrap gap-2">
            {n.candidates.length > 0 &&
              !(n.pinned && n.id.startsWith("h:")) && (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy}
                  onClick={() => repin(null)}
                  className="rounded-none font-mono text-[10px] uppercase tracking-[0.12em]"
                >
                  none of these
                </Button>
              )}
            {n.pinned && (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => repin(undefined)}
                className="rounded-none font-mono text-[10px] uppercase tracking-[0.12em]"
              >
                back to automatic
              </Button>
            )}
          </div>
        </section>
      )}
      {n && (
        <div className="border-t border-border pt-3">
          <Stat label="Copies heard">{n.observations}</Stat>
          <Stat label="Unique packets">{n.packets}</Stat>
          <Stat label="Copies per packet">
            {(n.observations / (n.packets || 1)).toFixed(2)}
          </Stat>
          <Stat label="Distinct routes">{routes.length}</Stat>
        </div>
      )}
      <section className="space-y-2 border-t border-border pt-3">
        <h3 className="label-overline">Routes to you through this node</h3>
        <p className="font-mono text-xs text-muted-foreground">
          Copies counts every copy heard, duplicates included. First counts only
          the copy that reached you first. A route with more copies than firsts
          is usually a slower backup, delivering after another route already
          did.
        </p>
        {routes.slice(0, shown).map((r) => (
          <div key={r.nodes.join(">")} className="space-y-1">
            <div className="font-mono text-xs">
              {r.nodes.map((nid, i) => (
                <span key={nid}>
                  {i > 0 && <span className="text-muted-foreground"> → </span>}
                  <span className={cn(nid === id && "text-primary")}>
                    {name(nid)}
                  </span>
                </span>
              ))}
            </div>
            <RouteBar
              label="copies"
              value={r.share}
              detail={`${r.count} pkts`}
              className="bg-primary"
            />
            <RouteBar
              label="first"
              value={r.winShare}
              detail={`${r.first} pkts`}
              className="bg-foreground/50"
            />
          </div>
        ))}
        {hidden.length > 0 && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setShown((n) => n + ROUTES_PAGE)}
            className="w-full rounded-none font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground"
          >
            show {Math.min(ROUTES_PAGE, hidden.length)} more · {hidden.length}{" "}
            left, {pct(hidden.reduce((sum, r) => sum + r.share, 0))} of copies
          </Button>
        )}
      </section>
    </div>
  );
}

function RouteBar({
  label,
  value,
  detail,
  className,
}: {
  label: string;
  value: number;
  detail: string;
  className: string;
}) {
  return (
    <div className="grid grid-cols-[3.5rem_1fr_6rem] items-center gap-2 font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground tabular-nums">
      <span>{label}</span>
      <div className="h-1.5 bg-muted">
        <div
          className={cn("h-full", className)}
          style={{ width: pct(value) }}
        />
      </div>
      <span className="text-right">
        {pct(value)} · {detail}
      </span>
    </div>
  );
}

function identityNote(n: WebNode): string {
  const unknown = n.id.startsWith("h:");
  if (n.pinned && unknown)
    return "You marked this hash as none of the known repeaters, so its links are not drawn.";
  if (n.pinned) return "You pinned this hash to this repeater.";
  if (n.candidates.length > 1)
    return `${n.candidates.length} repeaters share this hash. The one nearest the next hop toward you was picked. If its links stretch impossibly far, pick the right one.`;
  if (n.candidates.length === 1)
    return "Only one known repeater has this hash. If its links stretch impossibly far, the real relay is probably one we have never heard an advert from: choose none of these.";
  return "No known repeater has this hash, so this relay cannot be placed.";
}
