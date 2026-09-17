import type { ReactNode } from "react";
import { Plus, Trash2 } from "lucide-react";
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

// An entry is "<prefix>:<rest>" when prefixOptions is set; the prefix is everything before the
// first colon, which the rest may otherwise contain freely.
function splitEntry(entry: string): [string, string] {
  const i = entry.indexOf(":");
  return i === -1 ? ["", entry] : [entry.slice(0, i), entry.slice(i + 1)];
}

export function StringListField({
  label,
  values,
  onChange,
  placeholder,
  addLabel,
  emptyHint,
  hint,
  action,
  prefixOptions,
}: {
  label: string;
  values: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  addLabel: string;
  emptyHint?: string;
  hint?: ReactNode;
  action?: ReactNode;
  // When set, each row gains a leading picker and the entry is stored as "<prefix>:<value>".
  prefixOptions?: { value: string; label: string }[];
}) {
  const setEntry = (i: number, entry: string) =>
    onChange(values.map((v, j) => (j === i ? entry : v)));
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        <Label className="font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
          {label}
        </Label>
        {action}
      </div>
      <div className="space-y-2">
        {values.length === 0 && emptyHint && (
          <p className="font-mono text-xs text-muted-foreground/50">
            {emptyHint}
          </p>
        )}
        {values.map((value, i) => {
          const [prefix, rest] = prefixOptions
            ? splitEntry(value)
            : ["", value];
          return (
            <div key={i} className="flex items-center gap-2">
              {prefixOptions && (
                <Select
                  value={prefix}
                  onValueChange={(p) => setEntry(i, `${p}:${rest}`)}
                >
                  <SelectTrigger className="h-9 w-32 shrink-0 rounded-none border-border bg-background font-mono text-sm">
                    <SelectValue placeholder="field" />
                  </SelectTrigger>
                  <SelectContent className="rounded-sm">
                    {prefixOptions.map((o) => (
                      <SelectItem
                        key={o.value}
                        value={o.value}
                        className="font-mono text-sm"
                      >
                        {o.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              <Input
                value={prefixOptions ? rest : value}
                onChange={(e) =>
                  setEntry(
                    i,
                    prefixOptions
                      ? `${prefix}:${e.target.value}`
                      : e.target.value,
                  )
                }
                placeholder={placeholder}
                className="h-9 flex-1 rounded-none border-border bg-background font-mono text-sm"
              />
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                onClick={() => onChange(values.filter((_, j) => j !== i))}
                aria-label={`Remove ${label.toLowerCase()} entry`}
                className="shrink-0 rounded-none text-muted-foreground/60 hover:text-destructive"
              >
                <Trash2 className="size-3.5" />
              </Button>
            </div>
          );
        })}
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() =>
            onChange([
              ...values,
              prefixOptions ? `${prefixOptions[0].value}:` : "",
            ])
          }
          className="rounded-none font-mono text-[11px] uppercase tracking-[0.12em]"
        >
          <Plus className="size-3.5" />
          {addLabel}
        </Button>
      </div>
      {hint && (
        <p className="font-mono text-[10px] text-muted-foreground/60">{hint}</p>
      )}
    </div>
  );
}
