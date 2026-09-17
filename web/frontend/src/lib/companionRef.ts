// A companion's URL token: its id, plus a slug of its name so the link stays readable. The id is
// the authority and the slug is decoration, so a rename never breaks a link the user already has.
// The backend accepts this, a bare id, or a plain name, the last for links made before this existed.

export function companionRef(c: { id: number; name: string }): string {
  const slug = c.name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug ? `${c.id}-${slug}` : String(c.id);
}

export function companionIdFromRef(ref: string): number | null {
  const m = /^(\d+)(?:-|$)/.exec(ref);
  return m ? Number(m[1]) : null;
}

export function companionPath(
  c: { id: number; name: string },
  rest = "",
): string {
  return `/companions/${companionRef(c)}${rest}`;
}

// Resolve a :ref segment the way the server does, or the two disagree about which companion a URL
// addresses: an exact name wins, so a companion genuinely named "7" keeps its own URL even when
// another companion has id 7, and only then is the segment read as an id.
export function findByRef<T extends { id: number; name: string }>(
  seg: string,
  companions: T[],
): T | undefined {
  const named = companions.find((c) => c.name === seg);
  if (named) return named;
  const id = companionIdFromRef(seg);
  return id == null ? undefined : companions.find((c) => c.id === id);
}
