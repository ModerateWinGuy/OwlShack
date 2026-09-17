import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  ChevronLeft,
  ChevronRight,
} from "lucide-react";
import { TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { PeerPicker } from "@/components/PeerPicker";
import { cn } from "@/lib/utils";

// Shared by RepeaterDetailPage (remote) and RepeaterNodePage (local).

// The height override must be orientation-scoped or `tabs:h-9` wins; pb-1.5 absorbs the active indicator's overhang.
export function RepeaterTabsList({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const [edges, setEdges] = useState({ left: false, right: false });

  const sync = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    const max = el.scrollWidth - el.clientWidth;
    const left = el.scrollLeft > 1;
    const right = el.scrollLeft < max - 1;
    // Bail on an unchanged value so the every-render effect below can't loop.
    setEdges((p) => (p.left === left && p.right === right ? p : { left, right }));
  }, []);

  // No dep array: an admin login changes the tab count without a resize the observer would see.
  useEffect(sync);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.addEventListener("scroll", sync, { passive: true });
    const ro = new ResizeObserver(sync);
    ro.observe(el);
    return () => {
      el.removeEventListener("scroll", sync);
      ro.disconnect();
    };
  }, [sync]);

  return (
    <div className="relative">
      <TabsList
        ref={ref}
        variant="line"
        className="w-full justify-start gap-0 p-0 pb-1.5 bg-transparent rounded-none border-b border-border flex-nowrap overflow-x-auto group-data-[orientation=horizontal]/tabs:h-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
      >
        {children}
      </TabsList>
      <ScrollEdge side="left" show={edges.left} />
      <ScrollEdge side="right" show={edges.right} />
    </div>
  );
}

// Sits outside the scroll container — a child would scroll with the tabs.
function ScrollEdge({ side, show }: { side: "left" | "right"; show: boolean }) {
  const Icon = side === "left" ? ChevronLeft : ChevronRight;
  return (
    <div
      aria-hidden
      className={cn(
        "pointer-events-none absolute top-0 bottom-px flex w-10 items-center transition-opacity duration-150",
        side === "left"
          ? "left-0 justify-start bg-gradient-to-r"
          : "right-0 justify-end bg-gradient-to-l",
        "from-background via-background/80 to-transparent",
        show ? "opacity-100" : "opacity-0",
      )}
    >
      <Icon className="size-3.5 text-muted-foreground/70" />
    </div>
  );
}

export function RepeaterTab({
  value,
  icon,
  children,
}: {
  value: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <TabsTrigger
      value={value}
      className={cn(
        "rounded-none flex-none min-h-10 sm:min-h-0 px-3 sm:px-4 py-2.5 font-mono text-[11px] uppercase tracking-widest gap-1.5 border-b-2 border-transparent hover:bg-muted/30",
        // ! is required: the primitive's dark: + group-data-[variant=line] rules outrank plain data-[state=active].
        "data-[state=active]:!bg-primary/10 data-[state=active]:!text-primary data-[state=active]:border-b-2 data-[state=active]:!border-primary data-[state=active]:after:!opacity-0",
      )}
    >
      <span className="hidden sm:inline-flex">{icon}</span>
      {children}
    </TabsTrigger>
  );
}

export function StatTile({
  label,
  value,
  icon,
  accent,
}: {
  label: string;
  value: string;
  icon?: ReactNode;
  accent?: boolean;
}) {
  return (
    <div
      className={cn(
        "bg-card relative px-4 py-3 flex flex-col gap-1.5 group",
        accent && "bg-linear-to-br from-primary/5 via-card to-card",
      )}
    >
      <div className="flex items-center justify-between">
        <span className="label-overline">{label}</span>
        {icon && (
          <span className="text-muted-foreground/50 group-hover:text-primary transition-colors">
            {icon}
          </span>
        )}
      </div>
      <span
        className={cn(
          "font-mono text-lg font-semibold tabular-nums leading-none",
          accent && "text-primary",
        )}
      >
        {value}
      </span>
    </div>
  );
}

// ACL permission byte: lower 2 bits are the role.
export const PERM_ROLE_MASK = 0x03;
export const PERM_GUEST = 0;
export const PERM_READ_ONLY = 1;
export const PERM_READ_WRITE = 2;
export const PERM_ADMIN = 3;
// Sensor alert bits (SensorMesh.h PERM_RECV_ALERTS_*); setperm writes the byte whole, so a role change must carry them.
export const PERM_ALERTS_LO = 0x40;
export const PERM_ALERTS_HI = 0x80;
export const PERM_ALERTS_MASK = PERM_ALERTS_LO | PERM_ALERTS_HI;

export function roleLabel(perms: number): string {
  switch (perms & PERM_ROLE_MASK) {
    case PERM_GUEST:
      return "Guest";
    case PERM_READ_ONLY:
      return "Read only";
    case PERM_READ_WRITE:
      return "Read / Write";
    case PERM_ADMIN:
      return "Admin";
    default:
      return `0x${perms.toString(16)}`;
  }
}

export function rolePillClass(perms: number): string {
  switch (perms & PERM_ROLE_MASK) {
    case PERM_ADMIN:
      return "border-warning/40 text-warning bg-warning/5";
    case PERM_READ_WRITE:
      return "border-primary/40 text-primary bg-primary/5";
    case PERM_READ_ONLY:
      return "border-info/40 text-info bg-info/5";
    default:
      return "border-border text-muted-foreground";
  }
}

// The three the official app offers, so a role set there can be set here too.
export const ROLE_OPTIONS: { value: string; label: string }[] = [
  { value: String(PERM_READ_ONLY), label: "Read only" },
  { value: String(PERM_READ_WRITE), label: "Read / Write" },
  { value: String(PERM_ADMIN), label: "Admin" },
];

// Radix aligns the popover on the selected item, so a value with no item renders it off-screen.
export function CurrentRoleItem({ perms }: { perms: number }) {
  const role = String(perms & PERM_ROLE_MASK);
  if (ROLE_OPTIONS.some((o) => o.value === role)) return null;
  return (
    <SelectItem value={role} disabled className="rounded-none font-mono text-xs">
      {roleLabel(perms)}
    </SelectItem>
  );
}

// knownPrefixes: lowercase 12-hex prefixes already in the ACL.
export function AddAccessDialog({
  open,
  onOpenChange,
  peers,
  knownPrefixes,
  onAdd,
  kind = "repeater",
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  peers: { pubkey: string; name: string; type?: string }[];
  knownPrefixes: Set<string>;
  onAdd: (pubkey: string, perms: number) => Promise<void>;
  kind?: "repeater" | "sensor" | "room";
}) {
  const [role, setRole] = useState<string>(String(PERM_READ_ONLY));
  // Sensors: what a password login grants, so an added client behaves the same.
  const [alertHi, setAlertHi] = useState(true);
  const [alertLo, setAlertLo] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const alertBits =
    kind === "sensor" ? (alertHi ? PERM_ALERTS_HI : 0) | (alertLo ? PERM_ALERTS_LO : 0) : 0;

  useEffect(() => {
    if (!open) {
      setRole(String(PERM_READ_ONLY));
      setAlertHi(true);
      setAlertLo(true);
      setSubmitting(false);
    }
  }, [open]);

  const submitWith = async (pubkey: string) => {
    setSubmitting(true);
    try {
      await onAdd(pubkey.toLowerCase(), parseInt(role, 10) | alertBits);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="rounded-none border-border bg-card max-w-2xl">
        <DialogHeader>
          <DialogTitle className="font-mono uppercase tracking-[0.08em] text-sm">
            Grant access
          </DialogTitle>
          <DialogDescription className="text-xs text-muted-foreground">
            Add a peer to this {kind}'s ACL. They'll be able to log in without
            a password at the assigned role.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-2">
            <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
              Role
            </Label>
            <Select value={role} onValueChange={setRole}>
              <SelectTrigger className="rounded-none font-mono text-xs border-border bg-background w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="rounded-none font-mono text-xs">
                {ROLE_OPTIONS.map((opt) => (
                  <SelectItem
                    key={opt.value}
                    value={opt.value}
                    className="rounded-none font-mono text-xs"
                  >
                    {opt.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {kind === "sensor" && (
            <div className="space-y-2">
              <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
                Alerts
              </Label>
              <div className="grid grid-cols-2 gap-px bg-border border border-border">
                <AlertBitToggle label="high priority" checked={alertHi} onChange={setAlertHi} />
                <AlertBitToggle label="low priority" checked={alertLo} onChange={setAlertLo} />
              </div>
            </div>
          )}

          <PeerPicker
            peers={peers}
            knownPrefixes={knownPrefixes}
            onPick={submitWith}
            disabled={submitting}
            manualInputId="acl-manual-pubkey"
            alreadyAddedLabel="already in ACL"
          />
        </div>
      </DialogContent>
    </Dialog>
  );
}

// AlertBitToggle is one sensor alert-subscription bit as a labelled toggle.
export function AlertBitToggle({
  label,
  checked,
  onChange,
  disabled,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <label className="flex items-center justify-between gap-2 bg-card px-3 py-2 cursor-pointer">
      <span className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
        {label}
      </span>
      <Switch size="sm" checked={checked} onCheckedChange={onChange} disabled={disabled} />
    </label>
  );
}
