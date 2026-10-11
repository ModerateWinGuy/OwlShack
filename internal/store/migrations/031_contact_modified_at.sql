-- When each contact last changed, by any writer, which the MeshCore app syncs its contacts since; existing rows take when they were added or last heard.
ALTER TABLE companion_contacts ADD COLUMN modified_at INTEGER NOT NULL DEFAULT 0;
UPDATE companion_contacts SET modified_at = max(added_at, coalesce(last_seen, 0));
