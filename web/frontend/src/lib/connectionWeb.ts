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

// The traffic a node exchanged with one neighbour, in one direction. A null id on the feeding side
// is traffic the node heard straight from a client.
export interface WebNeighbour {
  id: string | null;
  count: number;
  first: number;
  // Of the copies heard through this node, the part that took this route; duplicates count.
  share: number;
  // The part of those copies that beat every other path to us; the rest arrived as duplicates,
  // after another path had already delivered that packet. Same denominator as share, so the two
  // stack into one bar.
  firstShare: number;
  // Only the hop into us has a measured signal, so these stay 0 on every link further out.
  snrSum: number;
  snrN: number;
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

// nodeNeighbours splits the traffic through a node by the node next to it: who handed each packet
// over, and who it went to next. Grouping by whole half-routes instead produces a row per distinct
// combination of hops, which runs to thousands on a relay near us and answers a question nobody
// asked; the hops further out are one click away on that neighbour's own sheet.
export function nodeNeighbours(
  chains: WebChain[],
  nodeId: string,
): { feeding: WebNeighbour[]; reaching: WebNeighbour[] } {
  const feeding = new Map<string | null, WebNeighbour>();
  const reaching = new Map<string | null, WebNeighbour>();
  let total = 0;
  const add = (
    into: Map<string | null, WebNeighbour>,
    id: string | null,
    c: WebChain,
    // The chain's SNR belongs to its last link, so only that link may take it.
    last: boolean,
  ) => {
    let hop = into.get(id);
    if (!hop) {
      hop = {
        id,
        count: 0,
        first: 0,
        share: 0,
        firstShare: 0,
        snrSum: 0,
        snrN: 0,
      };
      into.set(id, hop);
    }
    hop.count += c.count;
    hop.first += c.first;
    if (last) {
      hop.snrSum += c.snrSum;
      hop.snrN += c.snrN;
    }
  };
  for (const c of chains) {
    const at = c.nodes.indexOf(nodeId);
    if (at < 0) continue;
    const end = c.nodes.length - 1;
    // A null feeder is a packet this node heard from a client: a path names repeaters only.
    add(feeding, at > 0 ? c.nodes[at - 1] : null, c, at === end);
    if (at < end) add(reaching, c.nodes[at + 1], c, at + 1 === end);
    total += c.count;
  }
  const finish = (m: Map<string | null, WebNeighbour>) => {
    const out = [...m.values()];
    for (const hop of out) {
      hop.share = hop.count / (total || 1);
      hop.firstShare = hop.first / (total || 1);
    }
    return out.sort((a, b) => b.count - a.count);
  };
  return { feeding: finish(feeding), reaching: finish(reaching) };
}
