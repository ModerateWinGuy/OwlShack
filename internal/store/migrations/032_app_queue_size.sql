-- How many messages each companion holds while the MeshCore app is away; 256 is most firmware builds' OFFLINE_QUEUE_SIZE.
ALTER TABLE companions ADD COLUMN app_queue_size INTEGER NOT NULL DEFAULT 256;
