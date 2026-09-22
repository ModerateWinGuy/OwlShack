// Folds the routes /api/connection-web returns into links and per-node route breakdowns.

export const SELF_ID = "self";

// A repeater whose key starts with a hop's hash, so it could be the node that relayed it.
export interface WebCandidate {
  id: string;
  name: string;
  lat: number;
  lon: number;
  lastSeen: string;
}

export interface WebNode {
  id: string;
  name?: string;
  type?: string;
  lat: number; // degrees x 1e6; 0,0 = no position
  lon: number;
  hash?: string;
  candidates: WebCandidate[];
  // The operator chose this hash's owner; the distance pick no longer applies.
  pinned: boolean;
  observations: number;
  packets: number;
}

// One distinct route, source side first and ending at "self"; the SNR/RSSI belong to its last link.
export interface WebChain {
  nodes: string[];
  count: number;
  first: number;
  lastSeen: string;
  snrSum: number;
  snrN: number;
  snrMin: number | null;
  snrMax: number | null;
  rssiSum: number;
  rssiN: number;
}

export interface ConnectionWeb {
  self: { lat: number; lon: number } | null;
  hours: number;
  retentionDays: number;
  nodes: WebNode[];
  chains: WebChain[];
}

export interface WebLink {
  from: string;
  to: string;
  count: number;
  first: number;
  lastSeen: string;
  // Of the traffic `from` passed on toward us, the part that went to `to` next: how strongly it
  // prefers this hop over its alternatives.
  shareOut: number;
  // Of everything reaching `to` from a known node, the part that came over this link.
  shareIn: number;
  snrSum: number;
  snrN: number;
  snrMin: number | null;
  snrMax: number | null;
  rssiSum: number;
  rssiN: number;
}

export interface WebRoute {
  nodes: string[];
  count: number;
  first: number;
  // Of the copies heard through this node, the part that took this route; duplicates count.
  share: number;
  // Of the packets whose first copy reached us through this node, the part that took this route.
  // Differs from share whenever a route delivers duplicates after a faster one already arrived.
  winShare: number;
}

// chainThrough keeps a whole route when `via` is on it, or drops it. The route is kept whole, not
// trimmed to the part after `via`: what reaches us through a relay is the question, and the hops
// feeding it are half the answer.
function chainThrough(chain: WebChain, via?: string): string[] | null {
  if (!via) return chain.nodes;
  return chain.nodes.includes(via) ? chain.nodes : null;
}

// A hop is uncertain while several repeaters share its hash and nobody has said which one it is.
export const isUncertain = (n: WebNode) => n.candidates.length > 1 && !n.pinned;

async function send(url: string, init: RequestInit): Promise<void> {
  const res = await fetch(url, init);
  if (!res.ok) {
    const e = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(e.error || `HTTP ${res.status}`);
  }
}

// pinHop says which peer owns a hash; null says none of the known repeaters does.
export const pinHop = (hash: string, pubkey: string | null) =>
  send(`/api/connection-web/pins/${hash}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ pubkey }),
  });

export const unpinHop = (hash: string) =>
  send(`/api/connection-web/pins/${hash}`, { method: "DELETE" });

export const linkKey = (from: string, to: string) => `${from}>${to}`;

export function foldLinks(
  chains: WebChain[],
  via?: string,
): Map<string, WebLink> {
  const links = new Map<string, WebLink>();
  const into = new Map<string, number>();
  const outOf = new Map<string, number>();
  for (const c of chains) {
    const nodes = chainThrough(c, via);
    if (!nodes) continue;
    for (let i = 0; i + 1 < nodes.length; i++) {
      const from = nodes[i];
      const to = nodes[i + 1];
      const key = linkKey(from, to);
      let l = links.get(key);
      if (!l) {
        l = {
          from,
          to,
          count: 0,
          first: 0,
          lastSeen: "",
          shareOut: 0,
          shareIn: 0,
          snrSum: 0,
          snrN: 0,
          snrMin: null,
          snrMax: null,
          rssiSum: 0,
          rssiN: 0,
        };
        links.set(key, l);
      }
      l.count += c.count;
      l.first += c.first;
      if (c.lastSeen > l.lastSeen) l.lastSeen = c.lastSeen;
      into.set(to, (into.get(to) ?? 0) + c.count);
      outOf.set(from, (outOf.get(from) ?? 0) + c.count);
      if (i + 2 === nodes.length) {
        l.snrSum += c.snrSum;
        l.snrN += c.snrN;
        if (c.snrMin != null && (l.snrMin == null || c.snrMin < l.snrMin))
          l.snrMin = c.snrMin;
        if (c.snrMax != null && (l.snrMax == null || c.snrMax > l.snrMax))
          l.snrMax = c.snrMax;
        l.rssiSum += c.rssiSum;
        l.rssiN += c.rssiN;
      }
    }
  }
  for (const l of links.values()) {
    l.shareOut = l.count / (outOf.get(l.from) || 1);
    l.shareIn = l.count / (into.get(l.to) || 1);
  }
  return links;
}

// routesThrough lists the whole routes that carried packets through a node, busiest first.
export function routesThrough(chains: WebChain[], nodeId: string): WebRoute[] {
  const byKey = new Map<string, WebRoute>();
  let total = 0;
  let firsts = 0;
  for (const c of chains) {
    const nodes = chainThrough(c, nodeId);
    if (!nodes) continue;
    const key = nodes.join(">");
    let r = byKey.get(key);
    if (!r) {
      r = { nodes, count: 0, first: 0, share: 0, winShare: 0 };
      byKey.set(key, r);
    }
    r.count += c.count;
    r.first += c.first;
    total += c.count;
    firsts += c.first;
  }
  const routes = [...byKey.values()];
  for (const r of routes) {
    r.share = r.count / (total || 1);
    r.winShare = r.first / (firsts || 1);
  }
  return routes.sort((a, b) => b.count - a.count);
}
