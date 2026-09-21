import { useEffect, useRef, useState } from "react";
import { Smile } from "lucide-react";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { ensureEmojiInit } from "@/lib/emoji";
import { cn } from "@/lib/utils";

// One trigger for both breakpoints; props spread last so Radix's asChild handlers and ref win.
export function EmojiButton({
  active = false,
  ...props
}: React.ComponentProps<"button"> & { active?: boolean }) {
  return (
    <button
      type="button"
      aria-label="Insert emoji"
      className={cn(
        "absolute bottom-1 right-1 size-8 grid place-items-center hover:bg-muted/60 before:absolute before:-inset-1.5 before:content-[''] sm:before:hidden",
        active ? "text-primary" : "text-muted-foreground hover:text-foreground",
      )}
      {...props}
    >
      <Smile className="size-5" strokeWidth={1.6} />
    </button>
  );
}

// Desktop: an anchored popover (doesn't cover the composer).
export function EmojiPicker({ onSelect }: { onSelect: (emoji: string) => void }) {
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <EmojiButton active={open} />
      </PopoverTrigger>
      <PopoverContent
        align="end"
        side="top"
        sideOffset={8}
        className="w-auto rounded-none border-border p-0"
      >
        {open && <EmojiMartPanel onSelect={onSelect} />}
      </PopoverContent>
    </Popover>
  );
}

// Loaded lazily so the emoji-mart dataset stays out of the main bundle.
export function EmojiMartPanel({
  onSelect,
  fullWidth = false,
}: {
  onSelect: (emoji: string) => void;
  fullWidth?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const onSelectRef = useRef(onSelect);
  onSelectRef.current = onSelect;

  useEffect(() => {
    let cancelled = false;
    let el: HTMLElement | null = null;
    (async () => {
      await ensureEmojiInit();
      const [{ Picker }, data] = await Promise.all([
        import("emoji-mart"),
        import("@emoji-mart/data"),
      ]);
      if (cancelled || !ref.current) return;
      const isDark = document.documentElement.classList.contains("dark");
      const opts: Record<string, unknown> = {
        data: data.default,
        theme: isDark ? "dark" : "light",
        navPosition: "bottom",
        previewPosition: "none",
        skinTonePosition: "search",
        onEmojiSelect: (e: { native: string }) => onSelectRef.current(e.native),
      };
      if (fullWidth) {
        // Fill the screen width with larger cells (no scroll, no stretch).
        const w = window.innerWidth - 16;
        const perLine = Math.max(7, Math.min(9, Math.round(w / 46)));
        const emojiButtonSize = Math.floor((w - 8) / perLine);
        opts.perLine = perLine;
        opts.emojiButtonSize = emojiButtonSize;
        opts.emojiSize = Math.round(emojiButtonSize * 0.62);
      }
      el = new Picker(opts) as unknown as HTMLElement;
      ref.current.appendChild(el);
    })();
    return () => {
      cancelled = true;
      el?.remove();
    };
  }, [fullWidth]);

  return (
    <div
      ref={ref}
      className={fullWidth ? "w-full flex justify-center" : "max-w-[calc(100vw-0.5rem)]"}
    />
  );
}
