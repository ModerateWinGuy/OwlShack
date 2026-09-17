import { useEffect, useState } from "react";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

export type ContactType = "CHAT" | "REPEATER" | "ROOM" | "SENSOR";

const HEX64 = /^[0-9a-fA-F]{64}$/;

const TYPE_LABELS: Record<ContactType, string> = {
  CHAT: "Chat / Companion",
  REPEATER: "Repeater",
  ROOM: "Room",
  SENSOR: "Sensor",
};

export interface ContactPrefill {
  pubkey?: string;
  name?: string;
  type?: ContactType;
}

/** POSTs to the companion's contacts endpoint itself, then calls `onAdded`. */
export function AddContactDialog({
  companion,
  companionName,
  open,
  onOpenChange,
  initial,
  existingPubkeys,
  ownPubkey,
  onAdded,
}: {
  companion: string;
  companionName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  initial?: ContactPrefill;
  existingPubkeys?: string[];
  ownPubkey?: string;
  onAdded?: () => void;
}) {
  const [type, setType] = useState<ContactType>("CHAT");
  const [name, setName] = useState("");
  const [pubkey, setPubkey] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const initialPubkey = initial?.pubkey ?? "";
  const initialName = initial?.name ?? "";
  const initialType = initial?.type ?? "CHAT";

  useEffect(() => {
    if (open) {
      setType(initialType);
      setName(initialName);
      setPubkey(initialPubkey);
      setSubmitting(false);
    }
  }, [open, initialPubkey, initialName, initialType]);

  const trimmedKey = pubkey.trim().toLowerCase();
  const valid = HEX64.test(trimmedKey);
  const isSelf = valid && !!ownPubkey && trimmedKey === ownPubkey.toLowerCase();
  const alreadyAdded =
    valid &&
    (existingPubkeys ?? []).some((k) => k.toLowerCase() === trimmedKey);

  const canSubmit = valid && !isSelf && !alreadyAdded && !submitting;

  const submit = async () => {
    if (!canSubmit) return;
    setSubmitting(true);
    try {
      const res = await fetch(
        `/api/companions/${encodeURIComponent(companion)}/contacts`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ pubkey: trimmedKey, name: name.trim(), type }),
        },
      );
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        throw new Error(err.error || `HTTP ${res.status}`);
      }
      toast.success("Contact added");
      onOpenChange(false);
      onAdded?.();
    } catch (e) {
      toast.error(
        `Failed to add contact: ${e instanceof Error ? e.message : "failed"}`,
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="rounded-none border-border bg-card max-w-md">
        <DialogHeader>
          <DialogTitle className="font-mono uppercase tracking-[0.08em] text-sm">
            Add contact
          </DialogTitle>
          <DialogDescription className="text-xs text-muted-foreground">
            Manually add a contact by public key for{" "}
            {companionName || "this companion"}.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label className="label-overline">Type</Label>
            <Select
              value={type}
              onValueChange={(v) => setType(v as ContactType)}
            >
              <SelectTrigger className="w-full rounded-none font-mono text-xs uppercase tracking-[0.06em]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="rounded-none font-mono text-xs">
                {(Object.keys(TYPE_LABELS) as ContactType[]).map((t) => (
                  <SelectItem
                    key={t}
                    value={t}
                    className="font-mono text-xs uppercase tracking-[0.06em]"
                  >
                    {TYPE_LABELS[t]}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="add-contact-name" className="label-overline">
              Name
            </Label>
            <Input
              id="add-contact-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="display name (optional)"
              className="rounded-none text-sm h-9"
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="add-contact-key" className="label-overline">
              Public Key
            </Label>
            <Input
              id="add-contact-key"
              value={pubkey}
              onChange={(e) => setPubkey(e.target.value)}
              placeholder="64-character hex…"
              spellCheck={false}
              autoCorrect="off"
              autoCapitalize="off"
              aria-invalid={trimmedKey.length > 0 && !valid}
              className="rounded-none font-mono text-xs h-9"
            />
            {trimmedKey.length > 0 && !valid && (
              <p className="text-[10px] text-destructive font-mono">
                must be 64 hex characters
              </p>
            )}
            {isSelf && (
              <p className="text-[10px] text-warning font-mono">
                this is your own key — you can’t add yourself
              </p>
            )}
            {alreadyAdded && (
              <p className="text-[10px] text-warning font-mono">
                already a contact
              </p>
            )}
          </div>

          <div className="flex justify-end gap-2 pt-1">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onOpenChange(false)}
              className="font-mono uppercase tracking-widest"
            >
              Cancel
            </Button>
            <Button
              size="sm"
              onClick={submit}
              disabled={!canSubmit}
              className="font-mono uppercase tracking-widest"
            >
              {submitting ? "Adding…" : "Add contact"}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
