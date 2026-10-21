import { useCallback, useEffect, useState, type Dispatch, type SetStateAction } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { apiErrorMessage } from "@/lib/apiError";
import { useWebSocket } from "@/hooks/useWebSocket";
import type { TelemetryReading } from "@/components/TelemetryPanel";

export interface PushDelta {
  channel: number;
  type: number;
  value: number;
  name: string;
  unit: string;
}

export interface PushRow {
  channel: number;
  type: number;
  name: string;
  unit: string;
  on: boolean;
  value: string;
}

// What a 1.18 sensor can compare, per LPP type: the multiplier and largest raw value it stores, and the bytes it takes. Mirrors the server.
// Humidity is out because 1.18 misreads it, on/off types because a change of 1 never beats the firmware's strict "> change".
const RANGE: Record<number, { mult: number; max: number; size: number }> = {
  120: { mult: 1, max: 255, size: 1 },
  2: { mult: 100, max: 32767, size: 2 },
  103: { mult: 10, max: 32767, size: 2 },
  121: { mult: 1, max: 32767, size: 2 },
  101: { mult: 1, max: 65535, size: 2 },
  115: { mult: 10, max: 65535, size: 2 },
  116: { mult: 100, max: 65535, size: 2 },
  117: { mult: 1000, max: 65535, size: 2 },
  125: { mult: 1, max: 65535, size: 2 },
  128: { mult: 1, max: 65535, size: 2 },
  132: { mult: 1, max: 65535, size: 2 },
  100: { mult: 1, max: 2 ** 24, size: 4 },
  130: { mult: 1000, max: 2 ** 24, size: 4 },
  131: { mult: 1000, max: 2 ** 24, size: 4 },
};
const TEMPERATURE = 103;
const DEFAULT_DELTA: Record<number, string> = { [TEMPERATURE]: "0.5", 116: "0.1" };
export const MAX_PUSH_READINGS = 3;
const MAX_PUSH_BYTES = 14; // the sensor's min_deltas buffer

// LPP record sizes as written; the firmware's LPPData::getDataSize (LPPDataHelpers.h:68) agrees on all but humidity, which it reads as 2.
const SIZE: Record<number, number> = {
  0: 1, 1: 1, 102: 1, 104: 1, 120: 1, 142: 1, 135: 3, 113: 6, 134: 6, 136: 9,
  ...Object.fromEntries([2, 3, 101, 103, 115, 116, 117, 121, 125, 128, 132].map((t) => [t, 2])),
  ...Object.fromEntries([100, 118, 130, 131, 133].map((t) => [t, 4])),
};
const HUMIDITY = 104;

const rowKey = (r: { channel: number; type: number }) => `${r.channel}-${r.type}`;

// The sensor walks its telemetry with its own sizes to find a watched reading (SensorMesh.cpp:84). Past the first humidity it is out of
// step, and where it lands then depends on the live values, so only the readings before it can be watched.
function findable(raw: string): Set<string> {
  const b = (raw.match(/../g) ?? []).map((h) => parseInt(h, 16));
  const out = new Set<string>();
  for (let i = 0; i + 2 < b.length && b[i] !== 0; ) {
    const size = SIZE[b[i + 1]];
    if (size === undefined || b[i + 1] === HUMIDITY) break;
    out.add(rowKey({ channel: b[i], type: b[i + 1] }));
    i += 2 + size;
  }
  return out;
}

const bytesOf = (rows: PushRow[]) => rows.reduce((n, r) => n + (r.on ? 2 + (RANGE[r.type]?.size ?? 0) : 0), 0);

export function rowsFromSaved(saved?: PushDelta[] | null): PushRow[] {
  return (saved ?? []).map((d) => ({ ...d, on: true, value: String(d.value) }));
}

export function pushDeltas(rows: PushRow[]): PushDelta[] {
  return rows
    .filter((r) => r.on)
    .map(({ channel, type, name, unit, value }) => ({ channel, type, name, unit, value: Number(value) }));
}

// The first ticked reading the sensor can't take, in words; the server checks the same.
export function pushProblem(rows: PushRow[]): string | null {
  const on = rows.filter((r) => r.on);
  if (on.length === 0) return "Tick at least one reading for the sensor to watch";
  for (const r of on) {
    const f = RANGE[r.type];
    if (!f) return `${r.name} can't be watched; untick it`;
    const raw = Number(r.value) * f.mult;
    if (!(raw >= 1 && raw <= f.max)) return `${r.name}: the change must be from ${1 / f.mult} to ${f.max / f.mult}`;
  }
  return null;
}

// The readings a watch can find, each once (the sensor finds only the first of a channel and type), and every key the sensor sent.
function watchable(readings: TelemetryReading[], raw: string): { fresh: PushRow[]; sent: Set<string> } {
  const found = findable(raw);
  const seen = new Set<string>();
  const fresh = readings
    .filter((r) => {
      const k = rowKey(r);
      if (seen.has(k) || !RANGE[r.type] || typeof r.value !== "number" || !found.has(k)) return false;
      seen.add(k);
      return true;
    })
    .map((r) => ({ channel: r.channel, type: r.type, name: r.name, unit: r.unit ?? "", on: false, value: DEFAULT_DELTA[r.type] ?? "1" }));
  return { fresh, sent: new Set(readings.map(rowKey)) };
}

// Keeps what was ticked, even a reading missing from this reply (a part that didn't start), but not one the sensor sent and can't find;
// with nothing ticked, ticks temperature first and then the sensor's order.
function mergeReadings(rows: PushRow[], fresh: PushRow[], sent: Set<string>): PushRow[] {
  const had = new Map(rows.map((r) => [rowKey(r), r]));
  const next = fresh.map((r) => {
    const old = had.get(rowKey(r));
    return old ? { ...r, on: old.on, value: old.value } : r;
  });
  const seen = new Set(next.map(rowKey));
  next.push(...rows.filter((r) => r.on && !seen.has(rowKey(r)) && !sent.has(rowKey(r))));
  if (next.some((r) => r.on)) return next;
  const pick: PushRow[] = [];
  for (const r of [...next.filter((r) => r.type === TEMPERATURE), ...next.filter((r) => r.type !== TEMPERATURE)]) {
    if (pick.length < MAX_PUSH_READINGS && bytesOf([...pick, { ...r, on: true }]) <= MAX_PUSH_BYTES) pick.push({ ...r, on: true });
  }
  const ticked = new Set(pick.map(rowKey));
  return next.map((r) => ({ ...r, on: ticked.has(rowKey(r)) }));
}

const untilText = (iso: string) => {
  const at = new Date(iso);
  const time = at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return at.toDateString() === new Date().toDateString() ? time : `${at.toLocaleDateString([], { weekday: "short" })} ${time}`;
};

interface PushStatus {
  active: boolean;
  expires?: string;
  scope?: string;
  refused: boolean;
}

// Push for a monitored firmware 1.18+ sensor: which readings it watches, how much change sends one, and how it is going.
export function SensorPushSettings({
  companionRef,
  pubkey,
  push,
  onPushChange,
  rows,
  onRowsChange,
  savedPush,
}: {
  companionRef: string;
  pubkey: string;
  push: boolean;
  onPushChange: (on: boolean) => void;
  rows: PushRow[];
  onRowsChange: Dispatch<SetStateAction<PushRow[]>>;
  savedPush: boolean;
}) {
  const [loading, setLoading] = useState(false);
  const [status, setStatus] = useState<PushStatus | null>(null);
  const companion = encodeURIComponent(companionRef);

  const loadReadings = useCallback(async () => {
    setLoading(true);
    try {
      const r = await fetch(`/api/companions/${companion}/contacts/${encodeURIComponent(pubkey)}/telemetry`);
      if (!r.ok) throw new Error(await apiErrorMessage(r));
      const body: { readings: TelemetryReading[]; raw: string } = await r.json();
      const { fresh, sent } = watchable(body.readings ?? [], body.raw ?? "");
      if (fresh.length === 0) toast.error("The sensor sent no readings it can watch");
      onRowsChange((prev) => mergeReadings(prev, fresh, sent));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn't read the sensor");
    } finally {
      setLoading(false);
    }
  }, [companion, pubkey, onRowsChange]);

  const loadStatus = useCallback(() => {
    fetch(`/api/companions/${companion}/repeaters/${encodeURIComponent(pubkey)}/subscription`)
      .then((r) => (r.ok ? r.json() : null))
      .then((s: PushStatus | null) => setStatus(s))
      .catch(() => setStatus(null));
  }, [companion, pubkey]);

  useEffect(() => {
    if (savedPush) loadStatus();
  }, [savedPush, loadStatus]);

  // Each finished poll or push may have started, renewed or been refused a watch.
  const onMetrics = useCallback(
    (_topic: string, payload: unknown) => {
      if ((payload as { pubkey?: string })?.pubkey === pubkey) loadStatus();
    },
    [pubkey, loadStatus],
  );
  useWebSocket(savedPush ? ["metrics"] : [], onMetrics);

  const turn = (on: boolean) => {
    onPushChange(on);
    if (on && rows.length === 0 && !loading) loadReadings();
  };
  const ticked = rows.filter((r) => r.on).length;
  const bytes = bytesOf(rows);
  const update = (key: string, change: Partial<PushRow>) =>
    onRowsChange((prev) => prev.map((r) => (rowKey(r) === key ? { ...r, ...change } : r)));

  return (
    <div className="space-y-3">
      <label className="flex items-start justify-between gap-4">
        <div className="space-y-1 min-w-0">
          <span className="font-mono text-xs uppercase tracking-[0.08em] block">Push changes</span>
          <span className="block font-mono text-[10px] text-muted-foreground/60 leading-relaxed">
            Between polls, the sensor sends its readings within a minute of a ticked one changing by more than your amount
            since its last push. The sensor needs MeshCore 1.18 or later and a default region, set on its Regions tab.
          </span>
        </div>
        <Switch checked={push} onCheckedChange={turn} />
      </label>

      {push && (
        <div className="space-y-2">
          <div className="flex items-center justify-between gap-2">
            <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
              Readings to watch ({ticked} of {MAX_PUSH_READINGS})
            </Label>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={loadReadings}
              disabled={loading}
              className="rounded-none font-mono text-[10px] uppercase tracking-[0.12em]"
            >
              {loading ? <Loader2 className="size-3 animate-spin" /> : <RefreshCw className="size-3" />}
              {rows.length ? "reload readings" : "load readings"}
            </Button>
          </div>
          {rows.length > 0 ? (
            <div className="divide-y divide-border border border-border">
              {rows.map((r) => {
                const key = rowKey(r);
                return (
                  <div key={key} className="flex min-h-10 items-center gap-3 px-3 py-2 md:min-h-0">
                    <label className="relative flex flex-1 items-center gap-2 min-w-0 text-xs before:absolute before:inset-x-0 before:-inset-y-3 md:before:hidden">
                      <input
                        type="checkbox"
                        className="size-3.5 accent-primary"
                        checked={r.on}
                        disabled={!r.on && (ticked >= MAX_PUSH_READINGS || bytes + 2 + (RANGE[r.type]?.size ?? 0) > MAX_PUSH_BYTES)}
                        onChange={() => update(key, { on: !r.on })}
                      />
                      <span className="truncate">
                        {r.name} <span className="text-muted-foreground">ch{r.channel}</span>
                      </span>
                    </label>
                    {r.on && (
                      <div className="flex items-center gap-1.5">
                        <Label className="sr-only" htmlFor={`push-${key}`}>
                          Change in {r.name} that sends a push
                        </Label>
                        <Input
                          id={`push-${key}`}
                          type="number"
                          min={1 / (RANGE[r.type]?.mult ?? 1)}
                          max={(RANGE[r.type]?.max ?? 0) / (RANGE[r.type]?.mult ?? 1)}
                          step={1 / (RANGE[r.type]?.mult ?? 1)}
                          value={r.value}
                          onChange={(e) => update(key, { value: e.target.value })}
                          className="w-20 rounded-none font-mono text-base md:text-xs"
                        />
                        <span className="w-8 font-mono text-[10px] text-muted-foreground">{r.unit}</span>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          ) : (
            <p className="font-mono text-[10px] text-muted-foreground/60">
              {loading ? "Asking the sensor for its readings..." : "Load the sensor's readings to choose which to watch."}
            </p>
          )}
          <p className="font-mono text-[10px] text-muted-foreground/60 leading-relaxed">
            Only readings the sensor can track are listed: MeshCore 1.18 misreads humidity and loses its place in the
            readings after it. A sensor has room for three readings, or two when two of them are generic, distance or
            energy readings.
          </p>
        </div>
      )}

      {savedPush && status && <PushStatusLine status={status} />}
    </div>
  );
}

function PushStatusLine({ status }: { status: PushStatus }) {
  if (status.refused) {
    return (
      <p role="status" className="border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
        The sensor refused to push because it has no default region. Set one on its Regions tab. Until then it is
        polled as usual, every 6 hours unless you set an interval, and each poll asks again.
        {status.active && status.expires
          ? ` Its last watch keeps pushing until ${untilText(status.expires)}.`
          : ""}
      </p>
    );
  }
  if (status.active && status.expires) {
    return (
      <p role="status" className="text-xs text-muted-foreground">
        Pushing{status.scope ? ` in region ${status.scope}` : ""}, renewed at each poll (this watch lasts until{" "}
        {untilText(status.expires)}).
      </p>
    );
  }
  return (
    <p role="status" className="text-xs text-muted-foreground">
      Not pushing yet. It starts at the next poll that reaches the sensor.
    </p>
  );
}
