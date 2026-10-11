import L from "leaflet";
import { snrFill } from "@/components/SignalStrength";
import { wrapLon } from "@/lib/leaflet";

// Cycling the label position keeps lines that share an endpoint from stacking their labels.
export const LINK_STAGGER = [0.35, 0.5, 0.65] as const;

// A label shorter than this has nowhere to sit: it would cover both endpoints rather than the line.
const MIN_LABEL_PX = 70;

// Draws one link and its two-way SNR label. snrAtoB reads "→", snrBtoA reads "←"; the label is
// rotated to lie along the line, so both are relabelled if it would come out upside-down. Two nodes
// a street apart land within a few pixels of each other at low zoom, so the label is dropped until
// there is room for it -- callers redraw on zoom.
export function drawLink(
  map: L.Map,
  layer: L.LayerGroup,
  a: [number, number],
  b: [number, number],
  snrAtoB: number | null,
  snrBtoA: number | null,
  stagger: number,
) {
  const worstSnr = Math.min(snrAtoB ?? Infinity, snrBtoA ?? Infinity);

  // Mercator-corrected screen-space angle, so the label aligns at any latitude.
  const cosLat = Math.cos(((a[0] + b[0]) / 2) * (Math.PI / 180));
  const dxScreen = (b[1] - a[1]) * cosLat;
  const dyScreen = -(b[0] - a[0]); // screen Y is inverted vs latitude
  let angleDeg = Math.atan2(dyScreen, dxScreen) * (180 / Math.PI);

  // A right-to-left line would render the label upside-down, so flip it 180°.
  let flipped = false;
  if (angleDeg > 90 || angleDeg < -90) {
    angleDeg += angleDeg > 0 ? -180 : 180;
    flipped = true;
  }

  // After a flip the label's local "→" points toward A, not B, so swap.
  const snrFwd = flipped ? snrBtoA : snrAtoB;
  const snrBwd = flipped ? snrAtoB : snrBtoA;

  let rows: string;
  if (snrFwd != null && snrBwd != null) {
    rows = `<div>→ ${snrFwd.toFixed(1)} dB</div><div>← ${snrBwd.toFixed(1)} dB</div>`;
  } else if (snrFwd != null) {
    rows = `<div>→ ${snrFwd.toFixed(1)} dB</div>`;
  } else {
    rows = `<div>← ${(snrBwd ?? 0).toFixed(1)} dB</div>`;
  }

  L.polyline([a, b], {
    color: snrFill(worstSnr),
    weight: 2,
    opacity: 0.7,
    interactive: false,
  }).addTo(layer);

  if (map.latLngToContainerPoint(a).distanceTo(map.latLngToContainerPoint(b)) < MIN_LABEL_PX) {
    return;
  }

  const labelLat = a[0] + (b[0] - a[0]) * stagger;
  const labelLon = wrapLon(a[1] + (b[1] - a[1]) * stagger);

  L.marker([labelLat, labelLon], {
    icon: L.divIcon({
      className: "",
      html: `<div class="meshcore-link-label" style="transform:translate(-50%,-50%) rotate(${angleDeg.toFixed(1)}deg)">${rows}</div>`,
      iconSize: [0, 0],
      iconAnchor: [0, 0],
    }),
    interactive: false,
    keyboard: false,
  }).addTo(layer);
}
