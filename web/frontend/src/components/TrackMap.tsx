import { useEffect, useRef } from "react";
import L from "leaflet";
import { labelMarker, themeTileLayer, useThemeTiles } from "@/lib/leaflet";
import { cn } from "@/lib/utils";

export interface TrackPoint {
  ts: number;
  lat: number;
  lon: number;
  alt?: number;
}

// Gradient endpoints for altitude colouring, blue low → green high; the legend mirrors them.
const ALT_LOW = "hsl(220, 80%, 55%)";
const ALT_HIGH = "hsl(110, 80%, 55%)";

// A track with no altitude data gets the midpoint colour.
function altColor(t: number): string {
  const hue = 220 - 110 * Math.max(0, Math.min(1, t));
  return `hsl(${hue}, 80%, 55%)`;
}

function dotIcon(color: string, size: number): L.DivIcon {
  return L.divIcon({
    className: "meshcore-track-dot",
    html: `<span style="display:block;width:${size}px;height:${size}px;border-radius:9999px;background:${color};box-shadow:0 0 0 2px rgba(0,0,0,0.55),0 0 6px ${color};"></span>`,
    iconSize: [size, size],
    iconAnchor: [size / 2, size / 2],
  });
}

// The parent supplies the ordered points; alt is optional.
export function TrackMap({
  points,
  height = 300,
  className,
}: {
  points: TrackPoint[];
  height?: number;
  className?: string;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const mapRef = useRef<L.Map | null>(null);
  const tileRef = useRef<L.TileLayer | null>(null);
  const layerRef = useRef<L.LayerGroup | null>(null);

  const alts = points
    .map((p) => p.alt)
    .filter((a): a is number => a !== undefined && Number.isFinite(a));
  const altMin = alts.length ? Math.min(...alts) : undefined;
  const altMax = alts.length ? Math.max(...alts) : undefined;

  // Init once.
  useEffect(() => {
    if (!containerRef.current || mapRef.current) return;
    const map = L.map(containerRef.current, {
      zoomControl: true,
      attributionControl: true,
    }).setView([0, 0], 2);
    tileRef.current = themeTileLayer().addTo(map);
    layerRef.current = L.layerGroup().addTo(map);
    mapRef.current = map;
    return () => {
      map.remove();
      mapRef.current = null;
      tileRef.current = null;
      layerRef.current = null;
    };
  }, []);

  useThemeTiles(mapRef, tileRef);

  useEffect(() => {
    const map = mapRef.current;
    const layer = layerRef.current;
    if (!map || !layer) return;
    layer.clearLayers();

    const valid = points.filter(
      (p) => Number.isFinite(p.lat) && Number.isFinite(p.lon),
    );
    if (valid.length === 0) return;

    const span = altMin !== undefined && altMax !== undefined ? altMax - altMin : 0;
    for (let i = 0; i < valid.length - 1; i++) {
      const a = valid[i];
      const color =
        a.alt !== undefined && altMin !== undefined && span > 0
          ? altColor((a.alt - altMin) / span)
          : altColor(0.5);
      L.polyline(
        [
          [a.lat, a.lon],
          [valid[i + 1].lat, valid[i + 1].lon],
        ],
        { color, weight: 3, opacity: 0.9 },
      ).addTo(layer);
    }

    const first = valid[0];
    const last = valid[valid.length - 1];
    L.marker([first.lat, first.lon], {
      icon: dotIcon("var(--muted-foreground)", 8),
      keyboard: false,
    }).addTo(layer);
    labelMarker(L.marker([last.lat, last.lon], { icon: dotIcon("var(--primary)", 12) }).addTo(layer), "Last position")
      .bindPopup(
        `${last.lat.toFixed(5)}, ${last.lon.toFixed(5)}` +
          (last.alt !== undefined ? ` · ${last.alt.toFixed(0)} m` : ""),
      );

    if (valid.length === 1) {
      map.setView([first.lat, first.lon], 14);
    } else {
      map.fitBounds(
        L.latLngBounds(valid.map((p) => [p.lat, p.lon] as [number, number])),
        { padding: [24, 24] },
      );
    }
  }, [points, altMin, altMax]);

  return (
    <div className={cn("relative", className)} style={{ height }}>
      <div ref={containerRef} className="absolute inset-0" />
      {altMin !== undefined && altMax !== undefined && altMax > altMin && (
        <div className="absolute bottom-2 left-2 z-500 flex items-center gap-1.5 border border-border bg-card/85 px-2 py-1 font-mono text-[9px] uppercase tracking-widest text-muted-foreground backdrop-blur-sm">
          <span className="tabular-nums">{altMin.toFixed(0)}m</span>
          <span
            className="h-1.5 w-16 rounded-sm"
            style={{ background: `linear-gradient(to right, ${ALT_LOW}, ${ALT_HIGH})` }}
          />
          <span className="tabular-nums">{altMax.toFixed(0)}m</span>
        </div>
      )}
    </div>
  );
}
