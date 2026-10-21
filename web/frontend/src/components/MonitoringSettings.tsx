import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Eye, EyeOff, Loader2, Save } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { RETRY_OPTS, MAX_RETRIES_OPTS } from "@/lib/monitorOptions";
import { apiErrorMessage } from "@/lib/apiError";
import {
  SensorPushSettings,
  pushDeltas,
  pushProblem,
  rowsFromSaved,
  type PushDelta,
  type PushRow,
} from "@/components/SensorPushSettings";

export interface MonitorMetadata {
  isRepeater?: boolean;
  repeaterPassword?: string;
  monitor?: boolean;
  monitorIntervalSecs?: number;
  monitorProbes?: string[] | null;
  monitorRetrySecs?: number;
  monitorMaxRetries?: number;
  monitorPush?: boolean;
  monitorPushDeltas?: PushDelta[] | null;
  // telemPerms is which telemetry classes this contact may read; owned by the telemetry page.
  telemPerms?: number;
}

const INTERVAL_OPTS: { value: string; label: string }[] = [
  { value: "0", label: "Default (6h)" },
  { value: "900", label: "15 min" },
  { value: "1800", label: "30 min" },
  { value: "3600", label: "1 hour" },
  { value: "10800", label: "3 hours" },
  { value: "21600", label: "6 hours" },
  { value: "43200", label: "12 hours" },
  { value: "86400", label: "24 hours" },
];

// A pushing sensor's watch is renewed at each poll and lasts at most 18 h, so its polls are shorter. Mirrors the server.
const MAX_PUSH_INTERVAL = 64800;
const PUSH_INTERVAL_OPTS = INTERVAL_OPTS.filter((o) => Number(o.value) <= MAX_PUSH_INTERVAL).map((o) =>
  o.value === "0" ? { ...o, label: "Default (1h, 6h if refused)" } : o,
);

const PROBES = [
  { key: "status", label: "Status", hint: "battery, signal, traffic, uptime" },
  { key: "telemetry", label: "Telemetry", hint: "voltage, temperature, humidity, sensors" },
  { key: "neighbors", label: "Neighbours", hint: "SNR topology" },
] as const;

type ProbeKey = (typeof PROBES)[number]["key"];

function initialProbes(list?: string[] | null): Record<ProbeKey, boolean> {
  if (!list || list.length === 0)
    return { status: true, telemetry: true, neighbors: true };
  return {
    status: list.includes("status"),
    telemetry: list.includes("telemetry"),
    neighbors: list.includes("neighbors"),
  };
}

interface ContactLike {
  peerPubkey?: string;
  metadata?: MonitorMetadata;
}

// Companions have no firmware login handler: only a sessionless contact-telemetry request. A sensor is polled like one, and logs in only to push.
export type NodeKind = "repeater" | "companion" | "sensor";

// PATCHes the full contact metadata, preserving fields this form doesn't own.
export function MonitoringSettings({
  companionRef,
  pubkey,
  kind = "repeater",
  metadata,
  onSaved,
  className,
}: {
  companionRef: string;
  pubkey: string;
  kind?: NodeKind;
  metadata?: MonitorMetadata;
  onSaved?: (meta: MonitorMetadata) => void;
  className?: string;
}) {
  const [base, setBase] = useState<MonitorMetadata>(metadata || {});
  const [loaded, setLoaded] = useState(!!metadata);
  const [enabled, setEnabled] = useState(false);
  const [intervalSecs, setIntervalSecs] = useState("0");
  const [retrySecs, setRetrySecs] = useState("0");
  const [maxRetries, setMaxRetries] = useState("0");
  const [password, setPassword] = useState("");
  const [probes, setProbes] = useState<Record<ProbeKey, boolean>>(initialProbes());
  const [push, setPush] = useState(false);
  const [pushRows, setPushRows] = useState<PushRow[]>([]);
  const [saving, setSaving] = useState(false);

  const apply = useCallback((m: MonitorMetadata) => {
    setBase(m);
    setEnabled(!!m.monitor);
    setIntervalSecs(String(m.monitorIntervalSecs ?? 0));
    setRetrySecs(String(m.monitorRetrySecs ?? 0));
    setMaxRetries(String(m.monitorMaxRetries ?? 0));
    setPassword(m.repeaterPassword ?? "");
    setProbes(initialProbes(m.monitorProbes));
    setPush(!!m.monitorPush);
    setPushRows(rowsFromSaved(m.monitorPushDeltas));
    setLoaded(true);
  }, []);

  useEffect(() => {
    if (metadata) {
      apply(metadata);
      return;
    }
    fetch(`/api/companions/${encodeURIComponent(companionRef)}/contacts`)
      .then((r) => (r.ok ? r.json() : []))
      .then((cs: ContactLike[]) => {
        const c = (cs || []).find(
          (x) => x.peerPubkey?.toLowerCase() === pubkey.toLowerCase(),
        );
        apply(c?.metadata || {});
      })
      .catch(() => apply({}));
  }, [companionRef, pubkey, metadata, apply]);

  const toggleProbe = (key: ProbeKey) => {
    setProbes((prev) => {
      const next = { ...prev, [key]: !prev[key] };
      // Keep at least one probe enabled.
      if (!next.status && !next.telemetry && !next.neighbors) return prev;
      return next;
    });
  };

  const pushing = kind === "sensor" && push;
  const turnPush = (on: boolean) => {
    setPush(on);
    if (on && Number(intervalSecs) > MAX_PUSH_INTERVAL) setIntervalSecs("0");
  };

  const save = useCallback(async () => {
    const problem = pushing ? pushProblem(pushRows) : null;
    if (problem) {
      toast.error(problem);
      return;
    }
    setSaving(true);
    // Only what this panel owns: the PATCH merges, so a grant saved elsewhere since this loaded survives.
    const patch: MonitorMetadata = {
      monitor: enabled,
      monitorIntervalSecs: Number(intervalSecs),
      monitorRetrySecs: Number(retrySecs),
      monitorMaxRetries: Number(maxRetries),
      monitorProbes: [],
    };
    // Never write isRepeater for a companion: a stray flag routes the node to the login-based repeater collector.
    if (kind === "repeater") {
      const enabledProbes = (Object.keys(probes) as ProbeKey[]).filter((k) => probes[k]);
      patch.isRepeater = base.isRepeater ?? true;
      patch.repeaterPassword = password;
      // Empty when all are on, so it stays the implicit "all" rather than a list.
      patch.monitorProbes = enabledProbes.length === PROBES.length ? [] : enabledProbes;
    }
    // Push fields go only while push is on; left out, the stored choice stands for next time.
    if (kind === "sensor") {
      patch.monitorPush = push;
      patch.repeaterPassword = password;
      if (pushing) patch.monitorPushDeltas = pushDeltas(pushRows);
    }
    try {
      const r = await fetch(
        `/api/companions/${encodeURIComponent(companionRef)}/contacts/${encodeURIComponent(pubkey)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(patch),
        },
      );
      if (!r.ok) throw new Error(await apiErrorMessage(r));
      const meta = { ...base, ...patch };
      setBase(meta);
      toast.success("Monitoring settings saved");
      onSaved?.(meta);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Failed to save monitoring settings");
    } finally {
      setSaving(false);
    }
  }, [base, kind, password, enabled, intervalSecs, retrySecs, maxRetries, probes, push, pushing, pushRows, companionRef, pubkey, onSaved]);

  return (
    <div className={cn("panel", className)}>
      <div className="flex items-center justify-between px-4 py-3 border-b border-border">
        <div className="space-y-0.5">
          <span className="label-overline block">Monitoring</span>
          <h2 className="font-mono text-sm uppercase tracking-widest">
            Analytics &amp; history
          </h2>
        </div>
        <label className="flex items-center gap-2 cursor-pointer">
          <span className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
            {enabled ? "on" : "off"}
          </span>
          <Switch
            checked={enabled}
            disabled={!loaded}
            onCheckedChange={setEnabled}
          />
        </label>
      </div>

      <div
        className={cn(
          "p-4 space-y-4 transition-opacity",
          !enabled && "opacity-60",
        )}
      >
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <SelectField
            label="Fetch interval"
            value={intervalSecs}
            options={pushing ? PUSH_INTERVAL_OPTS : INTERVAL_OPTS}
            onChange={setIntervalSecs}
          />
          <SelectField
            label="Retry delay"
            value={retrySecs}
            options={RETRY_OPTS}
            onChange={setRetrySecs}
          />
          <SelectField
            label="Max retries"
            value={maxRetries}
            options={MAX_RETRIES_OPTS}
            onChange={setMaxRetries}
          />
        </div>
        <p className="font-mono text-[10px] text-muted-foreground/60 -mt-2">
          After a failed poll, re-attempt up to Max retries times at the Retry
          delay, then resume the normal Fetch interval.
        </p>

        {kind === "repeater" ? (
          <>
            <PasswordField value={password} onChange={setPassword}>
              Admin password unlocks full status + telemetry (location, environment
              sensors). Guest password or blank gets base telemetry only.
            </PasswordField>

            <div className="space-y-1.5">
              <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
                Probes to poll
              </Label>
              <div className="grid gap-px bg-border border border-border">
                {PROBES.map((p) => (
                  <label
                    key={p.key}
                    className="flex items-center justify-between gap-3 px-3 py-2 bg-card cursor-pointer"
                  >
                    <div className="min-w-0">
                      <div className="font-mono text-xs uppercase tracking-[0.08em]">
                        {p.label}
                      </div>
                      <div className="font-mono text-[10px] text-muted-foreground/60 truncate">
                        {p.hint}
                      </div>
                    </div>
                    <Switch
                      checked={probes[p.key]}
                      onCheckedChange={() => toggleProbe(p.key)}
                    />
                  </label>
                ))}
              </div>
              <p className="font-mono text-[10px] text-muted-foreground/60">
                Fewer probes = less radio airtime per poll.
              </p>
            </div>
          </>
        ) : kind === "sensor" ? (
          <>
            <SensorPushSettings
              companionRef={companionRef}
              pubkey={pubkey}
              push={push}
              onPushChange={turnPush}
              rows={pushRows}
              onRowsChange={setPushRows}
              savedPush={!!base.monitor && !!base.monitorPush}
            />
            {push && (
              <PasswordField value={password} onChange={setPassword}>
                Monitoring logs in with this to set up the push: the sensor's admin password, or blank if the
                sensor has already given this companion access.
              </PasswordField>
            )}
          </>
        ) : (
          <div className="space-y-1">
            <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
              What gets polled
            </Label>
            <p className="font-mono text-[10px] text-muted-foreground/60 leading-relaxed">
              Companions answer one sessionless telemetry request — battery plus
              any onboard sensors. No password or login exists for chat nodes.
              What's shared (base / location / environment) is controlled by the
              remote node's own telemetry-sharing settings, and it must have this
              companion in its contacts to respond.
            </p>
          </div>
        )}

        <div className="flex justify-end pt-1">
          <Button
            size="sm"
            onClick={save}
            disabled={saving || !loaded}
            className="rounded-none font-mono text-[10px] uppercase tracking-[0.12em]"
          >
            {saving ? (
              <Loader2 className="size-3 animate-spin" />
            ) : (
              <Save className="size-3" />
            )}
            save
          </Button>
        </div>
      </div>
    </div>
  );
}

function SelectField({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange: (v: string) => void;
}) {
  return (
    <div className="space-y-1">
      <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
        {label}
      </Label>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger className="rounded-none font-mono text-xs border-border bg-background w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent className="rounded-none font-mono text-xs">
          {options.map((o) => (
            <SelectItem
              key={o.value}
              value={o.value}
              className="rounded-none font-mono text-xs"
            >
              {o.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

function PasswordField({
  value,
  onChange,
  children,
}: {
  value: string;
  onChange: (v: string) => void;
  children: ReactNode;
}) {
  const [show, setShow] = useState(false);
  return (
    <div className="space-y-1">
      <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
        Access password
      </Label>
      <div className="relative">
        <Input
          type={show ? "text" : "password"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="(blank = guest / anonymous)"
          className="rounded-none font-mono text-base md:text-xs border-border bg-background pr-9"
        />
        <button
          type="button"
          onClick={() => setShow((s) => !s)}
          className="absolute inset-y-0 right-0 px-2.5 text-muted-foreground hover:text-foreground"
          tabIndex={-1}
        >
          {show ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
        </button>
      </div>
      <p className="font-mono text-[10px] text-muted-foreground/60 leading-relaxed">{children}</p>
    </div>
  );
}
