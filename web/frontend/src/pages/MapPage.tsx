import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import L from "leaflet";
import { ChevronDown, MapPin, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { useWebSocket } from "@/hooks/useWebSocket";
import { useApiList } from "@/hooks/useApiList";
import { useResume } from "@/lib/resume";
import { useCompanions } from "@/hooks/useCompanions";
import { usePeerDetailSheet } from "@/hooks/usePeerDetailSheet";
import { isPeerDelete } from "@/lib/peerWs";
import { Button } from "@/components/ui/button";
import { LoadErrorAlert } from "@/components/LoadErrorAlert";
import { PageHeader } from "@/components/PageHeader";
import { ConnectionPill, PEER_TYPE_HEX } from "@/components/StatusIndicator";
import { InlineConfirm } from "@/components/InlineConfirm";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { PeerDetailSheet } from "@/components/PeerDetailSheet";
import { deletePeers, deletedPeersMessage } from "@/lib/peerApi";
import { peerLatLon, themeTileLayer, useThemeTiles } from "@/lib/leaflet";
import { drawLink, LINK_STAGGER } from "@/lib/mapLinks";
import { cn } from "@/lib/utils";

interface Peer {
  pubkey: string;
  name: string;
  type: string;
  lat: number;
  lon: number;
  lastSeen: string;
  snr: number | null;
  rssi: number | null;
}

interface NeighborLink {
  aPubkey: string;
  aName: string;
  aLat: number;
  aLon: number;
  bPubkey: string;
  bName: string;
  bLat: number;
  bLon: number;
  snrAtoB?: number;
  snrBtoA?: number;
  ts: number;
}

const TYPE_FILTERS = ["CHAT", "REPEATER", "ROOM", "SENSOR", "NONE"] as const;

function isPeer(value: unknown): value is Peer {
  if (!value || typeof value !== "object") return false;
  const v = value as Record<string, unknown>;
  return typeof v.pubkey === "string" && typeof v.name === "string";
}

function dotIcon(color: string): L.DivIcon {
  return L.divIcon({
    className: "meshcore-peer-dot",
    html: `<span style="display:grid; place-items:center; width:24px; height:24px;"><span style="display:block; width:10px; height:10px; border-radius:9999px; background:${color}; box-shadow: 0 0 0 2px rgba(0,0,0,0.55), 0 0 6px ${color};"></span></span>`,
    iconSize: [24, 24],
    iconAnchor: [12, 12],
    popupAnchor: [0, -8],
  });
}

// A distinct target marker for a deep-linked coordinate, so it stands out from the peer dots.
function focusIcon(): L.DivIcon {
  return L.divIcon({
    className: "meshcore-focus-pin",
    html: `<span style="display:flex; align-items:center; justify-content:center; width:20px; height:20px; border-radius:9999px; border:2px solid var(--primary); box-shadow: 0 0 0 2px rgba(0,0,0,0.55), 0 0 12px var(--primary);"><span style="display:block; width:6px; height:6px; border-radius:9999px; background:var(--primary);"></span></span>`,
    iconSize: [20, 20],
    iconAnchor: [10, 10],
    popupAnchor: [0, -12],
  });
}

// One DivIcon per peer type, built once — the markers-sync effect runs on every WS peer update.
const PEER_ICONS: Record<string, L.DivIcon> = Object.fromEntries(
  Object.entries(PEER_TYPE_HEX).map(([type, color]) => [type, dotIcon(color)]),
);
const FOCUS_ICON = focusIcon();

const NO_PEERS: Peer[] = [];

export function MapPage() {
  const {
    items,
    setItems: setPeers,
    loading,
    error,
    reload,
    refresh,
  } = useApiList<Peer>("/api/peers", "Failed to load peers");
  useResume(refresh);
  const peers = items ?? NO_PEERS;
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const companions = useCompanions();
  const { selectPeer, sheetProps } = usePeerDetailSheet(peers);

  const [showLinks, setShowLinks] = useState(false);
  const { items: links } = useApiList<NeighborLink>(
    showLinks ? "/api/nodes/neighbor-links" : null,
    "Failed to load neighbor links",
  );

  const [searchParams] = useSearchParams();
  const focus = useMemo(() => {
    const la = parseFloat(searchParams.get("lat") ?? "");
    const lo = parseFloat(searchParams.get("lon") ?? "");
    if (!Number.isFinite(la) || !Number.isFinite(lo)) return null;
    if (la < -90 || la > 90 || lo < -180 || lo > 180) return null;
    return { lat: la, lon: lo };
  }, [searchParams]);

  const containerRef = useRef<HTMLDivElement | null>(null);
  const mapRef = useRef<L.Map | null>(null);
  const tileLayerRef = useRef<L.TileLayer | null>(null);
  const markersRef = useRef<Map<string, L.Marker>>(new Map());
  const focusMarkerRef = useRef<L.Marker | null>(null);
  const linksLayerRef = useRef<L.LayerGroup | null>(null);
  const fittedRef = useRef(false);
  // Bumped on zoom so the links redraw: whether a label fits depends on the current scale.
  const [zoomTick, setZoomTick] = useState(0);

  const handleMessage = useCallback(
    (topic: string, data: unknown) => {
      if (topic !== "peers") return;
      if (isPeerDelete(data)) {
        const gone = new Set(data.pubkeys.map((k) => k.toLowerCase()));
        setPeers((prev) =>
          (prev ?? []).filter((p) => !gone.has(p.pubkey.toLowerCase())),
        );
        return;
      }
      if (!isPeer(data)) return;
      setPeers((prev) => {
        const arr = prev ?? [];
        const idx = arr.findIndex((p) => p.pubkey === data.pubkey);
        if (idx === -1) return [data, ...arr];
        const next = arr.slice();
        next[idx] = { ...next[idx], ...data };
        return next;
      });
    },
    [setPeers],
  );

  const { connected } = useWebSocket(["peers"], handleMessage);

  useEffect(() => {
    if (!containerRef.current || mapRef.current) return;
    const map = L.map(containerRef.current, {
      center: [0, 0],
      zoom: 2,
      worldCopyJump: true,
      zoomControl: true,
      attributionControl: true,
    });
    tileLayerRef.current = themeTileLayer().addTo(map);
    linksLayerRef.current = L.layerGroup().addTo(map);
    map.on("zoomend", () => setZoomTick((t) => t + 1));
    mapRef.current = map;

    return () => {
      map.remove();
      mapRef.current = null;
      tileLayerRef.current = null;
      linksLayerRef.current = null;
      markersRef.current.clear();
      focusMarkerRef.current = null;
      fittedRef.current = false;
    };
  }, []);

  useThemeTiles(mapRef, tileLayerRef);

  // A ?lat=&lon= deep link marks the view fitted so the peer auto-fit can't yank it away.
  useEffect(() => {
    const map = mapRef.current;
    if (!map) return;
    if (!focus) {
      if (focusMarkerRef.current) {
        map.removeLayer(focusMarkerRef.current);
        focusMarkerRef.current = null;
      }
      return;
    }
    map.setView([focus.lat, focus.lon], 15);
    fittedRef.current = true;
    if (focusMarkerRef.current) {
      focusMarkerRef.current.setLatLng([focus.lat, focus.lon]);
    } else {
      focusMarkerRef.current = L.marker([focus.lat, focus.lon], {
        icon: FOCUS_ICON,
        zIndexOffset: 1000,
      })
        .addTo(map)
        .bindPopup(
          `<div style="font-family: var(--font-mono); font-size: 11px; text-transform: uppercase; letter-spacing: 0.08em;">shared location</div><div style="font-family: var(--font-mono); font-size: 11px; color: var(--muted-foreground);">${focus.lat}, ${focus.lon}</div>`,
        );
    }
    focusMarkerRef.current.openPopup();
  }, [focus]);

  const located = useMemo(
    () => peers.filter((p) => p.lat !== 0 || p.lon !== 0),
    [peers],
  );

  const visible = useMemo(
    () => located.filter((p) => !hidden.has(p.type)),
    [located, hidden],
  );

  const [confirmClear, setConfirmClear] = useState(false);
  const [clearing, setClearing] = useState(false);

  // Deletes exactly what's plotted, so the type pills double as a cleanup scope.
  const deleteShown = useCallback(async () => {
    setClearing(true);
    try {
      const result = await deletePeers(visible.map((p) => p.pubkey));
      toast.success(deletedPeersMessage(result));
      setConfirmClear(false);
    } catch (e) {
      toast.error(`Delete failed: ${e instanceof Error ? e.message : "error"}`);
    } finally {
      setClearing(false);
    }
  }, [visible]);

  useEffect(() => {
    const map = mapRef.current;
    if (!map) return;

    const next = new Set(visible.map((p) => p.pubkey));
    const markers = markersRef.current;

    for (const [key, marker] of markers.entries()) {
      if (!next.has(key)) {
        map.removeLayer(marker);
        markers.delete(key);
      }
    }

    for (const p of visible) {
      const [lat, lon] = peerLatLon(p.lat, p.lon);
      const icon = PEER_ICONS[p.type] || PEER_ICONS.NONE;
      const existing = markers.get(p.pubkey);
      if (existing) {
        existing.setLatLng([lat, lon]);
        // setIcon rebuilds the marker's DOM element — skip when unchanged.
        if (existing.getIcon() !== icon) existing.setIcon(icon);
      } else {
        const marker = L.marker([lat, lon], { icon }).addTo(map);
        // pubkey is stable for a marker, so this closure never goes stale.
        marker.on("click", () => selectPeer(p.pubkey));
        markers.set(p.pubkey, marker);
      }
    }

    if (!fittedRef.current && visible.length > 0) {
      const bounds = L.latLngBounds(
        visible.map((p) => peerLatLon(p.lat, p.lon)),
      );
      if (bounds.isValid()) {
        map.fitBounds(bounds, { padding: [40, 40], maxZoom: 12 });
        fittedRef.current = true;
      }
    }
  }, [visible]);

  // Full clear-and-redraw: links are cheap and have no per-link identity to diff against.
  useEffect(() => {
    const map = mapRef.current;
    const layer = linksLayerRef.current;
    if (!map || !layer) return;
    layer.clearLayers();
    if (!showLinks || !links) return;

    links.forEach((l, i) => {
      drawLink(
        map,
        linksLayerRef.current!,
        peerLatLon(l.aLat, l.aLon),
        peerLatLon(l.bLat, l.bLon),
        l.snrAtoB ?? null,
        l.snrBtoA ?? null,
        LINK_STAGGER[i % LINK_STAGGER.length],
      );
    });
  }, [links, showLinks, zoomTick]);

  const toggleType = useCallback((type: string) => {
    setHidden((prev) => {
      const next = new Set(prev);
      if (next.has(type)) next.delete(type);
      else next.add(type);
      return next;
    });
  }, []);

  const typeCounts = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const p of located) counts[p.type] = (counts[p.type] || 0) + 1;
    return counts;
  }, [located]);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Map"
        meta={
          <span className="font-mono text-sm text-muted-foreground tabular-nums">
            {visible.length}/{peers.length} with location
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

      <section className="panel overflow-hidden">
        <div className="flex flex-wrap items-center gap-2 px-4 py-3 border-b border-border">
          <span className="label-overline mr-2">Filter</span>
          <FilterMenu
            hidden={hidden}
            counts={typeCounts}
            onToggleType={toggleType}
            showLinks={showLinks}
            onToggleLinks={() => setShowLinks((v) => !v)}
          />
          {TYPE_FILTERS.map((t) => {
            const isHidden = hidden.has(t);
            const color = PEER_TYPE_HEX[t] || PEER_TYPE_HEX.NONE;
            const count = typeCounts[t] || 0;
            return (
              <button
                key={t}
                type="button"
                onClick={() => toggleType(t)}
                className={cn(
                  "hidden sm:inline-flex items-center gap-1.5 px-2 py-1 border border-border bg-card font-mono text-[10px] uppercase tracking-[0.12em] transition-all hover:border-foreground/40",
                  isHidden && "opacity-40 line-through",
                )}
              >
                <span
                  className="size-2 rounded-full"
                  style={{ background: color }}
                  aria-hidden
                />
                {t}
                <span className="tabular-nums text-muted-foreground/70">
                  {count}
                </span>
              </button>
            );
          })}
          <button
            type="button"
            onClick={() => setShowLinks((v) => !v)}
            className={cn(
              "ml-1 hidden sm:inline-flex items-center gap-1.5 border border-border bg-card px-2 py-1 pl-3 font-mono text-[10px] uppercase tracking-[0.12em] transition-all hover:border-foreground/40 border-l-2",
              showLinks ? "border-primary/60 text-primary" : "text-muted-foreground",
            )}
          >
            links
          </button>
          <div className="ml-auto flex items-center gap-3">
            {visible.length > 0 &&
              (clearing ? (
                <span className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
                  deleting…
                </span>
              ) : (
                <InlineConfirm
                  confirming={confirmClear}
                  onAskRemove={() => setConfirmClear(true)}
                  onCancel={() => setConfirmClear(false)}
                  onConfirm={deleteShown}
                  triggerLabel={`delete ${visible.length} shown`}
                />
              ))}
            <span className="flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
              <MapPin className="size-3" />
              <span className="tabular-nums">
                {visible.length}/{located.length}
              </span>
              <span className="text-muted-foreground/60">located</span>
            </span>
          </div>
        </div>

        <div
          ref={containerRef}
          className="h-[calc(100dvh-260px-var(--bottom-nav))] min-h-105 w-full"
        />
      </section>

      <PeerDetailSheet {...sheetProps} companions={companions} />
    </div>
  );
}

// Phone-sized form of the type pills, below sm only — the pills stay on wider screens.
function FilterMenu({
  hidden,
  counts,
  onToggleType,
  showLinks,
  onToggleLinks,
}: {
  hidden: Set<string>;
  counts: Record<string, number>;
  onToggleType: (type: string) => void;
  showLinks: boolean;
  onToggleLinks: () => void;
}) {
  const shown = TYPE_FILTERS.filter((t) => !hidden.has(t)).length;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          className="sm:hidden relative inline-flex items-center gap-1.5 border border-border bg-card px-2 py-1 font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground before:absolute before:inset-x-0 before:-inset-y-2 before:content-['']"
        >
          types
          <span className="tabular-nums text-foreground">
            {shown}/{TYPE_FILTERS.length}
          </span>
          {showLinks && <span className="text-primary">· links</span>}
          <ChevronDown className="size-3" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="rounded-sm">
        {TYPE_FILTERS.map((t) => (
          <DropdownMenuCheckboxItem
            key={t}
            checked={!hidden.has(t)}
            onCheckedChange={() => onToggleType(t)}
            onSelect={(e) => e.preventDefault()}
            className="font-mono text-[11px] uppercase tracking-[0.08em]"
          >
            <span
              className="size-2 rounded-full"
              style={{ background: PEER_TYPE_HEX[t] || PEER_TYPE_HEX.NONE }}
              aria-hidden
            />
            {t}
            <span className="ml-auto pl-3 tabular-nums text-muted-foreground/70">
              {counts[t] || 0}
            </span>
          </DropdownMenuCheckboxItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuCheckboxItem
          checked={showLinks}
          onCheckedChange={onToggleLinks}
          onSelect={(e) => e.preventDefault()}
          className="font-mono text-[11px] uppercase tracking-[0.08em]"
        >
          Neighbour links
        </DropdownMenuCheckboxItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
