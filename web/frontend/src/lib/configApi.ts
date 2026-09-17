// Client for internal/api/routes_config_rest.go: secrets read back as `*Set` booleans, and on write omit = keep, "" = clear.

// --- read shapes (GET DTOs) ---

export interface Settings {
  logLevel: string | null;
  connectionType: string;
  connection: string | null;
  baudRate: number | null;
  spiBoard: string | null;
  freq: number | null;
  bw: number | null;
  sf: number | null;
  cr: number | null;
  tx: number | null;
  listenAddr: string | null;
  mapTileKey: string | null;
  // The openHop modem token is a secret: reads report only whether one is stored.
  modemTokenSet: boolean;
  pathHashSize: number | null;
  // TX airtime cap as a percentage, the unit the firmware uses. null = 50%.
  dutyCycle: number | null;
  setupComplete: boolean;
}

// One SPI radio hat this build knows about, from GET /api/spi/boards.
export interface SpiBoard {
  name: string;
  label: string;
  chip: string;
  spiPort: string;
  maxTxPower: number;
  // "hardware" means run on the physical hat; "community" means never tested here.
  verified: string;
  notes?: string;
  // Why this build refuses the board; such boards are still listed.
  unsupported?: string;
  hasLeds: boolean;
}

// One serial device the host can see, from GET /api/serial/ports.
export interface SerialPort {
  // What to store: a stable by-id path where the OS has one, else the tty itself.
  path: string;
  // The tty `path` currently resolves to.
  device: string;
  label?: string;
  stable: boolean;
}

// boardOption labels a hat for the picker, flagging one that cannot be trusted
// blind: finding out afterwards means the antenna is already up.
export function boardOption(b: SpiBoard): { value: string; label: string } {
  let label = b.label;
  if (b.unsupported) label += " - unsupported";
  else if (b.verified !== "hardware") label += " - unverified";
  return { value: b.name, label };
}

// boardHint describes the selected hat, leading with whatever would stop it working.
export function boardHint(b: SpiBoard | undefined): string {
  if (!b) return "Pick the board this host has fitted.";
  if (b.unsupported) return `Cannot be driven by this build: ${b.unsupported}`;
  const parts = [`${b.chip}, up to ${b.maxTxPower} dBm`];
  if (b.hasLeds) parts.push("activity LEDs");
  if (b.verified !== "hardware") {
    parts.push("wiring taken from a community board list and never tested here");
  }
  if (b.notes) parts.push(b.notes);
  return parts.join(". ");
}

// defaultBoard preselects a usable hat: boards sort by name, so the first entry
// is alphabetical and may well be one this build refuses.
export function defaultBoard(boards: SpiBoard[]): SpiBoard | undefined {
  return (
    boards.find((b) => !b.unsupported && b.verified === "hardware") ??
    boards.find((b) => !b.unsupported) ??
    boards[0]
  );
}

export interface MqttSettings {
  enabled: boolean | null;
  nodeCompanionId: number | null;
  iataCode: string | null;
  statusInterval: number | null;
  owner: string | null;
  email: string | null;
}

export interface Broker {
  id: number;
  name: string;
  enabled: boolean;
  dedup: boolean;
  transport: string;
  host: string;
  port: number;
  packetTopic: string | null;
  statusTopic: string | null;
  disallowedPacketTypes: string[] | null;
  retainStatus: boolean;
  tlsEnabled: boolean;
  tlsInsecure: boolean;
  authType: string;
  username: string;
  passwordSet: boolean;
  path: string;
  audience: string;
}

export interface ConfigCompanion {
  id: number;
  name: string;
  pubkey: string;
  privateKeySet: boolean;
  latitude: number | null;
  longitude: number | null;
  advertInterval: number | null;
  pathHashSize: number | null; // null = inherit the global default
  dmPolicy: string; // contacts | allowlist | anyone
  dmAllow: string[] | null;
}

export interface ConfigChannel {
  id: number;
  companionId: number;
  name: string;
  privateKeySet: boolean;
}

export interface Trigger {
  id: number;
  companionId: number;
  type: string;
  template: string;
  charLimitBehaviour: string | null;
  match: string[] | null;
  contacts: string[] | null;
  channelIds: number[] | null;
  failoverPattern: string;
  failoverTimeout: number;
  retryTimeout: number | null;
  maxRetries: number | null;
  pathHashSize: number | null;
  schedule: string | null;
  url: string;
}

// The single repeater NODE (the relay we run), not a remote one being administered.
export interface ConfigRepeater {
  configured: boolean;
  running: boolean;
  name: string;
  pubkey: string;
  privateKeySet: boolean;
  latitude: number | null;
  longitude: number | null;
  advertInterval: number | null;
  floodAdvertInterval: number | null;
  disableFwd: boolean | null;
  floodMax: number | null;
  floodMaxUnscoped: number | null;
  floodMaxAdvert: number | null;
  loopDetect: string | null;
  pathHashSize: number | null;
  txDelayFactor: number | null;
  directTxDelayFactor: number | null;
  rxDelayBase: number | null;
  multiAcks: number | null;
  defaultRegion: string;
  adminPasswordSet: boolean;
  guestPasswordSet: boolean;
  ownerInfo: string;
  regions: RepeaterRegion[];
}

// A transport scope the repeater relays; its key derives from the name as SHA256(name)[:16].
export interface RepeaterRegion {
  name: string;
  denyFlood: boolean;
}

// Live relay stats from the running repeater node (GET /api/repeater/status).
export interface RepeaterNodeStats {
  name: string;
  pubkey: string;
  uptimeSecs: number;
  packetsReceived: number;
  packetsForwarded: number;
  txQueueLen: number;
  neighbors: number;
  latitude: number;
  longitude: number;
  lastSnr: number | null;
  lastRssi: number | null;
  noiseFloor: number | null;
  batteryMv: number | null;
  rxAirSecs: number;
  txAirSecs: number;
  floodTx: number;
  directTx: number;
  floodRx: number;
  directRx: number;
  floodDups: number;
  directDups: number;
}

export interface RepeaterNodeNeighbor {
  pubkey: string;
  name: string;
  snr: number;
  secsAgo: number;
}

// An admin client in the repeater's ACL (GET /api/repeater/acl).
export interface RepeaterAclEntry {
  pubkey: string;
  name: string;
  permission: number; // 0=guest 1=read-only 2=read-write 3=admin
  lastSeen: number; // unix seconds
}

// --- write shapes (request bodies) ---

export interface SettingsInput {
  logLevel?: string | null;
  connectionType?: string | null;
  connection?: string | null;
  baudRate?: number | null;
  spiBoard?: string | null; // omit = keep
  freq?: number | null;
  bw?: number | null;
  sf?: number | null;
  cr?: number | null;
  tx?: number | null;
  listenAddr?: string | null;
  mapTileKey?: string | null; // omit = keep, "" = clear
  modemToken?: string; // omit = keep the stored token
  pathHashSize?: number | null;
  dutyCycle?: number | null;
  // Only set by the first-run wizard; omit elsewhere so a radio edit never re-opens setup.
  setupComplete?: boolean;
}

export interface MqttInput {
  enabled?: boolean | null;
  nodeCompanionId?: number | null;
  iataCode?: string | null;
  statusInterval?: number | null;
  owner?: string | null;
  email?: string | null;
}

export interface BrokerInput {
  name: string;
  enabled: boolean;
  dedup: boolean;
  transport: string;
  host: string;
  port: number;
  packetTopic?: string | null;
  statusTopic?: string | null;
  disallowedPacketTypes?: string[] | null;
  retainStatus: boolean;
  tlsEnabled: boolean;
  tlsInsecure: boolean;
  authType: string;
  username: string;
  password?: string;
  path: string;
  audience: string;
}

export interface CompanionInput {
  name: string;
  privateKey?: string; // omit = keep (update) / generate (create)
  latitude?: number | null;
  longitude?: number | null;
  advertInterval?: number | null;
  pathHashSize?: number | null; // null = inherit the global default
  dmPolicy?: string;
  dmAllow?: string[] | null;
}

export interface ChannelInput {
  name: string;
  privateKey?: string;
}

// Repeater node config is edited per-section (no whole-config bulk write).
export interface RepeaterCreateInput {
  name: string;
  // Required and non-blank: a blank admin password lets any node in range log in as admin.
  adminPassword: string;
  privateKey?: string; // omit/blank = generate
}

export interface RepeaterNodeInput {
  name: string;
  privateKey?: string; // omit = keep identity; a value rotates it
  latitude?: number | null;
  longitude?: number | null;
}

export interface RepeaterRelayInput {
  disableFwd?: boolean | null;
  floodMax?: number | null;
  floodMaxUnscoped?: number | null;
  floodMaxAdvert?: number | null;
  loopDetect?: string | null;
  pathHashSize?: number | null;
  txDelayFactor?: number | null;
  directTxDelayFactor?: number | null;
  rxDelayBase?: number | null;
  multiAcks?: number | null;
  defaultRegion?: string; // "" = unscoped flood adverts
  advertInterval?: number | null;
  floodAdvertInterval?: number | null;
}

export interface RepeaterAdminInput {
  ownerInfo: string;
  adminPassword?: string; // omit = keep, "" = clear
  guestPassword?: string;
}

export interface TriggerInput {
  companionId: number;
  type: string;
  template: string;
  charLimitBehaviour?: string | null;
  match?: string[] | null;
  contacts?: string[] | null;
  channelIds?: number[] | null;
  failoverPattern?: string;
  failoverTimeout?: number;
  retryTimeout?: number | null;
  maxRetries?: number | null;
  pathHashSize?: number | null;
  schedule?: string | null;
  url?: string | null;
}

// --- request helper ---

// Validation failures come back as 422 { error }; throw the server's message.
async function request(
  url: string,
  method: string,
  body?: unknown,
): Promise<Response> {
  const res = await fetch(url, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    const err = (await res.json().catch(() => null)) as { error?: string } | null;
    throw new Error(err?.error || `request failed (${res.status})`);
  }
  return res;
}

// Write handlers that create a resource return { id } of the new/affected row.
async function requestJSON<T>(url: string, method: string, body?: unknown): Promise<T> {
  const res = await request(url, method, body);
  return (await res.json()) as T;
}

async function requestId(url: string, method: string, body?: unknown): Promise<number> {
  const res = await request(url, method, body);
  const json = (await res.json().catch(() => null)) as { id?: number } | null;
  return json?.id ?? 0;
}

// --- write functions ---

export const configApi = {
  putSettings: (input: SettingsInput) => request("/api/config/settings", "PUT", input),
  // The radio hats this binary knows how to wire. Empty until the backend is
  // up, which the UI shows as "no boards" rather than an empty picker.
  getSpiBoards: () => requestJSON<SpiBoard[]>("/api/spi/boards", "GET"),
  getSerialPorts: () => requestJSON<SerialPort[]>("/api/serial/ports", "GET"),
  putMqtt: (input: MqttInput) => request("/api/config/mqtt", "PUT", input),

  saveBroker: (input: BrokerInput, id?: number) =>
    id
      ? requestId(`/api/config/mqtt/brokers/${id}`, "PUT", input)
      : requestId("/api/config/mqtt/brokers", "POST", input),
  deleteBroker: (id: number) => request(`/api/config/mqtt/brokers/${id}`, "DELETE"),

  saveCompanion: (input: CompanionInput, id?: number) =>
    id
      ? requestId(`/api/config/companions/${id}`, "PUT", input)
      : requestId("/api/config/companions", "POST", input),
  deleteCompanion: (id: number) => request(`/api/config/companions/${id}`, "DELETE"),

  createChannel: (companionId: number, input: ChannelInput) =>
    requestId(`/api/config/companions/${companionId}/channels`, "POST", input),
  saveChannel: (id: number, input: ChannelInput) =>
    request(`/api/config/channels/${id}`, "PUT", input),
  deleteChannel: (id: number) => request(`/api/config/channels/${id}`, "DELETE"),

  saveTrigger: (input: TriggerInput, id?: number) =>
    id
      ? requestId(`/api/config/triggers/${id}`, "PUT", input)
      : requestId("/api/config/triggers", "POST", input),
  deleteTrigger: (id: number) => request(`/api/config/triggers/${id}`, "DELETE"),

  createRepeater: (input: RepeaterCreateInput) =>
    request("/api/config/repeater", "POST", input),
  updateRepeaterNode: (input: RepeaterNodeInput) =>
    request("/api/config/repeater/node", "PUT", input),
  updateRepeaterRelay: (input: RepeaterRelayInput) =>
    request("/api/config/repeater/relay", "PUT", input),
  updateRepeaterAdmin: (input: RepeaterAdminInput) =>
    request("/api/config/repeater/admin", "PUT", input),
  addRepeaterRegion: (name: string, denyFlood: boolean) =>
    request("/api/config/repeater/regions", "POST", { name, denyFlood }),
  setRepeaterRegionFlood: (name: string, denyFlood: boolean) =>
    request(`/api/config/repeater/regions/${encodeURIComponent(name)}`, "PATCH", { denyFlood }),
  removeRepeaterRegion: (name: string) =>
    request(`/api/config/repeater/regions/${encodeURIComponent(name)}`, "DELETE"),
  deleteRepeater: () => request("/api/config/repeater", "DELETE"),
  repeaterAdvert: (flood: boolean) => request("/api/repeater/advert", "POST", { flood }),
  repeaterDiscover: () => request("/api/repeater/discover", "POST"),
  clearRepeaterStats: () => request("/api/repeater/stats", "DELETE"),
  setRepeaterAcl: (pubkey: string, permission: number) =>
    request(`/api/repeater/acl/${encodeURIComponent(pubkey)}`, "PUT", { permission }),
  revokeRepeaterAcl: (pubkey: string) =>
    request(`/api/repeater/acl/${encodeURIComponent(pubkey)}`, "DELETE"),
};
