import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type KeyboardEvent,
} from "react";
import { Send } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cliCommandsFor, cliConfigKeysFor, type CliRole } from "@/lib/cliCatalog";
import { cn } from "@/lib/utils";

interface CliEntry {
  kind: "input" | "output" | "error";
  text: string;
  ts: number;
}

// Curated CLI catalogue, mirrored from MeshCore/src/helpers/CommonCLI.cpp.
interface Suggestion {
  text: string;
  hint?: string;
}

function buildSuggestions(input: string, role: CliRole): Suggestion[] {
  const CLI_CONFIG_KEYS = cliConfigKeysFor(role);
  const CLI_TOPLEVEL_COMMANDS = cliCommandsFor(role);
  const lower = input.toLowerCase();
  // get / set sub-completion: rank prefix matches first, then substring matches.
  if (lower.startsWith("get ") || lower.startsWith("set ")) {
    const prefix = lower.slice(0, 4);
    const tail = lower.slice(4).trim();
    if (tail === "") {
      return CLI_CONFIG_KEYS.map((k) => ({
        text: `${prefix}${k.key}`,
        hint: k.hint,
      }));
    }
    const starts = CLI_CONFIG_KEYS.filter((k) => k.key.startsWith(tail));
    const others = CLI_CONFIG_KEYS.filter(
      (k) => !k.key.startsWith(tail) && k.key.includes(tail),
    );
    return [...starts, ...others].map((k) => ({
      text: `${prefix}${k.key}`,
      hint: k.hint,
    }));
  }
  if (lower === "") {
    return CLI_TOPLEVEL_COMMANDS.map((c) => ({
      text: c.command,
      hint: c.hint,
    }));
  }
  const starts = CLI_TOPLEVEL_COMMANDS.filter((c) =>
    c.command.toLowerCase().startsWith(lower),
  );
  const others = CLI_TOPLEVEL_COMMANDS.filter(
    (c) =>
      !c.command.toLowerCase().startsWith(lower) &&
      c.command.toLowerCase().includes(lower),
  );
  return [...starts, ...others].map((c) => ({
    text: c.command,
    hint: c.hint,
  }));
}

// role picks the catalogue for autocomplete; without one there is none.
export function CliTerminal({
  sendCli,
  role,
}: {
  sendCli: (cmd: string) => Promise<string>;
  role?: CliRole;
}) {
  const [history, setHistory] = useState<CliEntry[]>([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [hIdx, setHIdx] = useState(-1);
  const [suggestionsOpen, setSuggestionsOpen] = useState(false);
  const [sIdx, setSIdx] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const endRef = useRef<HTMLDivElement>(null);

  const inputs = useMemo(
    () => history.filter((h) => h.kind === "input").map((h) => h.text),
    [history],
  );

  const suggestions = useMemo(
    () =>
      suggestionsOpen && role && input.trim() !== "" ? buildSuggestions(input, role) : [],
    [input, suggestionsOpen, role],
  );

  useEffect(() => {
    setSIdx(0);
  }, [input]);

  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [history]);

  const applySuggestion = useCallback((s: Suggestion) => {
    setInput(s.text);
    setSuggestionsOpen(false);
    requestAnimationFrame(() => inputRef.current?.focus());
  }, []);

  const submit = useCallback(async () => {
    const cmd = input.trim();
    if (!cmd || busy) return;
    setBusy(true);
    setHistory((prev) => [...prev, { kind: "input", text: cmd, ts: Date.now() }]);
    setInput("");
    setHIdx(-1);
    try {
      const out = await sendCli(cmd);
      setHistory((prev) => [
        ...prev,
        { kind: "output", text: out || "(empty)", ts: Date.now() },
      ]);
    } catch (e) {
      setHistory((prev) => [
        ...prev,
        {
          kind: "error",
          text: e instanceof Error ? e.message : "error",
          ts: Date.now(),
        },
      ]);
    } finally {
      setBusy(false);
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [input, busy, sendCli]);

  const onKey = useCallback(
    (e: KeyboardEvent<HTMLInputElement>) => {
      const sugOpen = suggestionsOpen && suggestions.length > 0;
      if (e.key === "Enter") {
        e.preventDefault();
        setSuggestionsOpen(false);
        submit();
        return;
      }
      if (e.key === "Tab") {
        e.preventDefault();
        if (suggestions.length === 0) {
          setSuggestionsOpen(true);
          return;
        }
        if (sugOpen) {
          applySuggestion(suggestions[sIdx] || suggestions[0]);
        } else {
          // first Tab opens the menu and pre-selects index 0
          setSuggestionsOpen(true);
          setSIdx(0);
        }
        return;
      }
      if (e.key === "Escape") {
        if (sugOpen) {
          e.preventDefault();
          setSuggestionsOpen(false);
        }
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        if (sugOpen) {
          setSIdx((i) => (i <= 0 ? suggestions.length - 1 : i - 1));
          return;
        }
        if (inputs.length === 0) return;
        const next = hIdx === -1 ? inputs.length - 1 : Math.max(0, hIdx - 1);
        setHIdx(next);
        setInput(inputs[next] ?? "");
      } else if (e.key === "ArrowDown") {
        e.preventDefault();
        if (sugOpen) {
          setSIdx((i) => (i >= suggestions.length - 1 ? 0 : i + 1));
          return;
        }
        if (inputs.length === 0) return;
        if (hIdx === -1) return;
        const next = hIdx + 1;
        if (next >= inputs.length) {
          setHIdx(-1);
          setInput("");
        } else {
          setHIdx(next);
          setInput(inputs[next] ?? "");
        }
      }
    },
    [submit, hIdx, inputs, suggestions, sIdx, suggestionsOpen, applySuggestion],
  );

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="label-overline">cli session</span>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setHistory([])}
          className="rounded-none font-mono text-[10px] uppercase tracking-[0.12em] text-muted-foreground"
        >
          clear
        </Button>
      </div>
      <pre
        className="bg-background border border-border text-xs font-mono p-3 min-h-72 max-h-112 overflow-y-auto whitespace-pre-wrap wrap-break-word"
      >
        {history.length === 0 ? (
          <span className="text-muted-foreground/50">
            // type a command. try `ver`, `{role === "repeater" ? "neighbors" : role && role !== "companion" ? "clock" : "get radio"}`, `get name`.{role && " tab = autocomplete."}
          </span>
        ) : (
          history.map((h, i) => (
            <div
              key={i}
              className={cn(
                "py-0.5",
                h.kind === "input" && "text-primary",
                h.kind === "error" && "text-destructive",
                h.kind === "output" && "text-foreground",
              )}
            >
              {h.kind === "input" ? `> ${h.text}` : h.text}
            </div>
          ))
        )}
        <div ref={endRef} />
      </pre>
      <div className="relative">
        {suggestionsOpen && suggestions.length > 0 && (
          <div className="absolute bottom-full left-0 right-0 mb-1 bg-popover border border-border shadow-md z-20 max-h-60 overflow-y-auto">
            {suggestions.map((s, i) => (
              <button
                key={s.text}
                type="button"
                onMouseDown={(e) => {
                  e.preventDefault();
                  applySuggestion(s);
                }}
                onMouseEnter={() => setSIdx(i)}
                className={cn(
                  "w-full text-left px-3 py-1.5 min-h-10 md:min-h-0 flex items-center gap-3 font-mono text-xs",
                  i === sIdx
                    ? "bg-primary/15 text-primary"
                    : "text-foreground hover:bg-muted/40",
                )}
              >
                <span className="shrink-0 whitespace-pre">{s.text}</span>
                {s.hint && (
                  <span className="min-w-0 flex-1 text-right text-[10px] text-muted-foreground/70 truncate">
                    {s.hint}
                  </span>
                )}
              </button>
            ))}
            <div className="px-3 py-1 border-t border-border font-mono text-[9px] uppercase tracking-[0.12em] text-muted-foreground/60 flex items-center gap-3">
              <span>↑↓ select</span>
              <span>tab apply</span>
              <span>esc close</span>
            </div>
          </div>
        )}
        <div className="flex items-center gap-2">
          <span className="font-mono text-xs text-primary select-none">
            {">"}
          </span>
          <Input
            ref={inputRef}
            value={input}
            onChange={(e: ChangeEvent<HTMLInputElement>) => {
              setInput(e.target.value);
              setSuggestionsOpen(true);
            }}
            onKeyDown={onKey}
            onFocus={() => setSuggestionsOpen(true)}
            onBlur={() => {
              // give the click on a suggestion a chance to fire
              setTimeout(() => setSuggestionsOpen(false), 120);
            }}
            disabled={busy}
            placeholder={busy ? "executing…" : role ? "command (tab to autocomplete)" : "command"}
            className="rounded-none font-mono text-base md:text-xs border-border bg-background flex-1"
            autoComplete="off"
            spellCheck={false}
          />
          <Button
            size="sm"
            onClick={submit}
            disabled={busy || !input.trim()}
            className="rounded-none font-mono text-[11px] uppercase tracking-[0.12em]"
          >
            <Send className="size-3" /> run
          </Button>
        </div>
      </div>
    </div>
  );
}
