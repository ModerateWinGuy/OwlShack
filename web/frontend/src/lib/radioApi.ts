// Counters the modem itself reports. A field typed `?` is absent when this transport cannot measure
// it at all, which is a different fact from a zero: render it as "—", never as 0.
export interface RadioStatus {
  transport: string;
  freqHz: number;
  bwHz: number;
  sf: number;
  cr: number;
  txPower: number;

  inboundDroppedNew: number;
  handlerSlow: number;

  inboundDroppedOldest?: number;
  rxMetaTimeouts?: number;
  rxMetaMisattributed?: number;
  hwErrors?: number;
  txOutcomeLost?: number;

  txSent: number;
  txFailed: number;
  txRequeued: number;
  txDroppedBusy: number;
  txDroppedQueue: number;
  txQueueLen: number;

  uptimeSecs: number;
  noiseFloor?: number;
  batteryMv?: number;
  mcuTempC?: number;

  packetsRecv?: number;
  packetsSent?: number;
  hwDecodeErrors?: number; // KISS only: a malformed SETHARDWARE frame
  crcErrors?: number;
  recvErrors?: number; // SPI only: the driver could not read a packet it knew had arrived
  driverErrors?: number;
  recvRecoveries?: number;
}

export async function fetchRadioStatus(): Promise<RadioStatus | null> {
  const res = await fetch("/api/radio/status");
  if (!res.ok) return null;
  return (await res.json()) as RadioStatus;
}

export async function resetModem(): Promise<void> {
  const res = await fetch("/api/radio/reset", { method: "POST" });
  if (!res.ok) {
    const err = (await res.json().catch(() => null)) as { error?: string } | null;
    throw new Error(err?.error || `reset failed (${res.status})`);
  }
}
