package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// writerQueueDepth bounds pending write closures; the RX goroutine drops on a full queue rather than stalling.
const writerQueueDepth = 1024

type Store struct {
	db             *sql.DB
	Peers          *PeerRepo
	Contacts       *ContactRepo
	Packets        *PacketRepo
	Messages       *MessageRepo
	Conversations  *ConversationRepo
	Echoes         *EchoRepo
	BlockedSenders *BlockedSenderRepo
	Metrics        *MetricsRepo
	AppConfig      *AppConfigRepo
	Settings       *SettingsRepo
	Mqtt           *MqttRepo
	Brokers        *BrokerRepo
	Companions     *CompanionRepo
	Channels       *ChannelRepo
	Triggers       *TriggerRepo
	SignalTests    *SignalTestRepo
	LinkMonitors   *LinkMonitorRepo
	Repeater       *RepeaterRepo
	RepeaterACL    *RepeaterACLRepo

	writerCh   chan func()
	writerDone chan struct{}
	closing    chan struct{}
	closeOnce  sync.Once
	dropped    atomic.Uint64
	// lastDrop is when the most recent write was dropped, in unix nanos; 0 means never.
	lastDrop atomic.Int64
}

func Open(ctx context.Context, path string) (*Store, error) {
	// modernc.org/sqlite only honours the "_pragma=" form; the mattn-style "_journal_mode=WAL" is silently ignored.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	s := &Store{
		db:             db,
		Peers:          &PeerRepo{db: db},
		Contacts:       &ContactRepo{db: db},
		Packets:        &PacketRepo{db: db, maxRows: DefaultMaxPackets},
		Messages:       &MessageRepo{db: db, maxRows: DefaultMaxMessages},
		Conversations:  &ConversationRepo{db: db},
		Echoes:         &EchoRepo{db: db},
		BlockedSenders: &BlockedSenderRepo{db: db},
		Metrics:        &MetricsRepo{db: db},
		AppConfig:      &AppConfigRepo{db: db},
		Settings:       &SettingsRepo{db: db},
		Mqtt:           &MqttRepo{db: db},
		Brokers:        &BrokerRepo{db: db},
		Companions:     &CompanionRepo{db: db},
		Channels:       &ChannelRepo{db: db},
		Triggers:       &TriggerRepo{db: db},
		SignalTests:    &SignalTestRepo{db: db},
		LinkMonitors:   &LinkMonitorRepo{db: db},
		Repeater:       &RepeaterRepo{db: db},
		RepeaterACL:    &RepeaterACLRepo{db: db},
		writerCh:       make(chan func(), writerQueueDepth),
		writerDone:     make(chan struct{}),
		closing:        make(chan struct{}),
	}

	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	go s.writerLoop()

	return s, nil
}

// WriteAsync queues fn on the writer goroutine and never blocks; false means the queue was full and fn was dropped.
// WriterStats reports the write queue and its drops; lastDrop is zero when none, which the ever-rising count alone cannot say.
func (s *Store) WriterStats() (queued, capacity int, dropped uint64, lastDrop time.Time) {
	if nanos := s.lastDrop.Load(); nanos != 0 {
		lastDrop = time.Unix(0, nanos)
	}
	return len(s.writerCh), cap(s.writerCh), s.dropped.Load(), lastDrop
}

func (s *Store) WriteAsync(fn func()) bool {
	if s.closed() {
		return false
	}
	select {
	case s.writerCh <- fn:
		return true
	default:
		s.lastDrop.Store(time.Now().UnixNano())
		dropped := s.dropped.Add(1)
		if dropped == 1 || dropped%100 == 0 {
			slog.Warn("store writer queue full, dropping write", "dropped", dropped)
		}
		return false
	}
}

// WriteSync runs fn on the writer goroutine and blocks; calling it from the RX dispatch thread or inside another writer closure deadlocks.
func (s *Store) WriteSync(fn func()) {
	if s.closed() {
		return
	}
	done := make(chan struct{})
	select {
	case <-s.closing:
		return
	case s.writerCh <- func() {
		defer close(done)
		fn()
	}:
	}
	select {
	case <-done:
	case <-s.closing:
		<-s.writerDone // drain finished: fn has either run or never will
	}
}

func (s *Store) closed() bool {
	select {
	case <-s.closing:
		return true
	default:
		return false
	}
}

func (s *Store) QueueLen() int {
	return len(s.writerCh)
}

func (s *Store) Dropped() uint64 {
	return s.dropped.Load()
}

// writerLoop runs queued closures until Close, then drains the queue; writerCh is never closed (many senders).
func (s *Store) writerLoop() {
	defer close(s.writerDone)
	for {
		select {
		case <-s.closing:
			for {
				select {
				case fn := <-s.writerCh:
					fn()
				default:
					return
				}
			}
		case fn := <-s.writerCh:
			fn()
		}
	}
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.closing)
		<-s.writerDone
	})
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	// A shipped slot is frozen — a released DB has stamped its version and will skip it — so append, never merge, renumber or edit.
	for i := version; i < len(migrations); i++ {
		if err := s.runMigration(ctx, i+1, migrations[i]); err != nil {
			return err
		}
	}

	return nil
}

var migrations = []func(context.Context, dbExecer) error{
	migrateV1,   // user_version 1
	migrateNoop, // 2 — squashed into migrateV1
	migrateNoop, // 3 — squashed into migrateV1
	migrateV2,   // 4 — indexed packet_hash/path columns
	migrateV3,   // 5 — signal_tests, signal_test_runs, link_monitors
	migrateV4,   // 6 — repeater node tables (config + ACL)
	migrateV5,   // 7 — repeater flood_max_unscoped + default_region columns
	migrateV6,   // 8 — map tile key, home region, advert clamp, path hash size, relay timing
	migrateV7,   // 9 — settings.duty_cycle_pct (TX airtime budget)
	migrateV8,   // 10 — settings.spi_board (SPI radio hat wiring)
	migrateV9,   // 11 — repeater.admin_password backfilled off blank (blank granted admin)
	migrateV10,  // 12 — companions.dm_policy + dm_allow (who may DM this companion)
	migrateV11,  // 13 — triggers.url (the feed an rss/cap trigger polls)
	migrateV12,  // 14 — clamp triggers.path_hash_size to the 3-byte maximum the rest of the app uses
	migrateV13,  // 15 — settings.modem_token (the openHop modem's access token)
	migrateV14,  // 16 — optional group bot failover
}

// dbExecer is the subset of *sql.DB / *sql.Tx a migration needs.
type dbExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// runMigration applies one migration and its user_version bump atomically, so a crash mid-way can't leave a half-applied schema.
func (s *Store) runMigration(ctx context.Context, version int, fn func(context.Context, dbExecer) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %d: begin: %w", version, err)
	}
	defer tx.Rollback()

	if err := fn(ctx, tx); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("setting schema version %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %d: commit: %w", version, err)
	}
	return nil
}

// migrateV1 is the shipped baseline schema: never edit it, append later slots instead.
func migrateV1(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS discovered_peers (
			pubkey             BLOB PRIMARY KEY,
			name               TEXT NOT NULL DEFAULT '',
			type               TEXT NOT NULL DEFAULT 'NONE',
			lat                INTEGER NOT NULL DEFAULT 0,
			lon                INTEGER NOT NULL DEFAULT 0,
			feat1              INTEGER NOT NULL DEFAULT 0,
			feat2              INTEGER NOT NULL DEFAULT 0,
			out_path           BLOB,
			last_advert_ts     INTEGER NOT NULL DEFAULT 0,
			last_seen          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			snr                REAL,
			rssi               INTEGER,
			out_path_hash_size INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS companions (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT    NOT NULL,
			private_key     TEXT    NOT NULL DEFAULT '',
			pubkey          TEXT    NOT NULL DEFAULT '',
			latitude        REAL,
			longitude       REAL,
			advert_interval INTEGER
		);

		CREATE TABLE IF NOT EXISTS companion_contacts (
			companion_id       INTEGER  NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			peer_pubkey        BLOB     NOT NULL,
			name               TEXT     NOT NULL DEFAULT '',
			type               TEXT     NOT NULL DEFAULT '',
			lat                INTEGER  NOT NULL DEFAULT 0,
			lon                INTEGER  NOT NULL DEFAULT 0,
			feat1              INTEGER  NOT NULL DEFAULT 0,
			feat2              INTEGER  NOT NULL DEFAULT 0,
			out_path           BLOB,
			out_path_hash_size INTEGER  NOT NULL DEFAULT 0,
			last_seen          DATETIME,
			last_advert_ts     INTEGER  NOT NULL DEFAULT 0,
			added_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			metadata           TEXT     NOT NULL DEFAULT '{}',
			PRIMARY KEY (companion_id, peer_pubkey)
		);

		CREATE TABLE IF NOT EXISTS packets (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			received_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			direction     TEXT NOT NULL,
			raw           BLOB NOT NULL,
			route_type    INTEGER,
			payload_type  INTEGER,
			snr           REAL,
			rssi          INTEGER
		);

		CREATE TABLE IF NOT EXISTS messages (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			companion_id   INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			channel        TEXT NOT NULL,
			channel_hash   INTEGER NOT NULL,
			sender         TEXT NOT NULL DEFAULT '',
			text           TEXT NOT NULL DEFAULT '',
			direction      TEXT NOT NULL,
			timestamp      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			snr            REAL,
			rssi           INTEGER,
			confirmed      INTEGER,
			path_hashes    BLOB,
			path_hash_size INTEGER,
			hops           INTEGER,
			status         TEXT
		);

		CREATE TABLE IF NOT EXISTS message_echoes (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id      INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			received_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			path_hashes     BLOB,
			path_hash_size  INTEGER NOT NULL DEFAULT 1,
			hops            INTEGER NOT NULL DEFAULT 0,
			snr             REAL,
			rssi            INTEGER
		);

		CREATE TABLE IF NOT EXISTS conversation_reads (
			companion_id    INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			conversation_id TEXT NOT NULL,
			last_read_id    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (companion_id, conversation_id)
		);

		CREATE TABLE IF NOT EXISTS blocked_senders (
			companion_id    INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			conversation_id TEXT NOT NULL,
			sender          TEXT NOT NULL,
			blocked_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (companion_id, conversation_id, sender)
		);

		CREATE TABLE IF NOT EXISTS node_metrics (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts      INTEGER NOT NULL,
			pubkey  BLOB    NOT NULL,
			metric  TEXT    NOT NULL,
			channel INTEGER NOT NULL DEFAULT 0,
			value   REAL    NOT NULL
		);

		CREATE TABLE IF NOT EXISTS node_state (
			pubkey       BLOB PRIMARY KEY,
			kind         TEXT    NOT NULL DEFAULT '',
			name         TEXT    NOT NULL DEFAULT '',
			last_poll_ts INTEGER NOT NULL DEFAULT 0,
			last_ok_ts   INTEGER NOT NULL DEFAULT 0,
			last_error   TEXT    NOT NULL DEFAULT '',
			state        TEXT    NOT NULL DEFAULT '{}',
			companion_id TEXT    NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS node_neighbors (
			ts              INTEGER NOT NULL,
			pubkey          BLOB    NOT NULL,
			neighbor_pubkey BLOB    NOT NULL,
			snr             REAL,
			PRIMARY KEY (ts, pubkey, neighbor_pubkey)
		);

		CREATE TABLE IF NOT EXISTS app_config (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			config     TEXT NOT NULL,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS settings (
			id              INTEGER PRIMARY KEY CHECK (id = 1),
			log_level       TEXT,
			connection_type TEXT NOT NULL DEFAULT 'kiss',
			connection      TEXT,
			baud_rate       INTEGER,
			freq            REAL,
			bw              REAL,
			sf              INTEGER,
			cr              INTEGER,
			tx              INTEGER,
			listen_addr     TEXT,
			setup_complete  INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS companion_channels (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			companion_id INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			name         TEXT    NOT NULL,
			private_key  TEXT    NOT NULL DEFAULT '',
			UNIQUE (companion_id, name)
		);

		CREATE TABLE IF NOT EXISTS triggers (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			companion_id         INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			type                 TEXT    NOT NULL,
			template             TEXT    NOT NULL DEFAULT '',
			char_limit_behaviour TEXT,
			match_patterns       TEXT    NOT NULL DEFAULT '',
			contacts             TEXT    NOT NULL DEFAULT '',
			retry_timeout        INTEGER,
			max_retries          INTEGER,
			path_hash_size       INTEGER,
			schedule             TEXT
		);

		CREATE TABLE IF NOT EXISTS trigger_channels (
			trigger_id INTEGER NOT NULL REFERENCES triggers(id) ON DELETE CASCADE,
			channel_id INTEGER NOT NULL REFERENCES companion_channels(id) ON DELETE CASCADE,
			PRIMARY KEY (trigger_id, channel_id)
		);

		CREATE TABLE IF NOT EXISTS mqtt_settings (
			id                INTEGER PRIMARY KEY CHECK (id = 1),
			enabled           INTEGER,
			node_companion_id INTEGER REFERENCES companions(id) ON DELETE SET NULL,
			iata_code         TEXT,
			status_interval   INTEGER,
			owner             TEXT,
			email             TEXT
		);

		CREATE TABLE IF NOT EXISTS mqtt_brokers (
			id                      INTEGER PRIMARY KEY AUTOINCREMENT,
			name                    TEXT    NOT NULL,
			enabled                 INTEGER NOT NULL DEFAULT 1,
			dedup                   INTEGER NOT NULL DEFAULT 0,
			transport               TEXT    NOT NULL DEFAULT 'tcp',
			host                    TEXT    NOT NULL DEFAULT '',
			port                    INTEGER NOT NULL DEFAULT 0,
			packet_topic            TEXT,
			status_topic            TEXT,
			disallowed_packet_types TEXT    NOT NULL DEFAULT '',
			retain_status           INTEGER NOT NULL DEFAULT 0,
			tls_enabled             INTEGER NOT NULL DEFAULT 0,
			tls_insecure            INTEGER NOT NULL DEFAULT 0,
			auth_type               TEXT    NOT NULL DEFAULT 'none',
			username                TEXT    NOT NULL DEFAULT '',
			password                TEXT    NOT NULL DEFAULT '',
			path                    TEXT    NOT NULL DEFAULT '',
			audience                TEXT    NOT NULL DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_packets_received_at ON packets(received_at);
		CREATE INDEX IF NOT EXISTS idx_companion_contacts_companion ON companion_contacts(companion_id);
		CREATE INDEX IF NOT EXISTS idx_messages_companion_channel ON messages(companion_id, channel);
		CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
		CREATE INDEX IF NOT EXISTS idx_message_echoes_message ON message_echoes(message_id);
		CREATE INDEX IF NOT EXISTS idx_node_metrics_q ON node_metrics(pubkey, metric, ts);
		CREATE INDEX IF NOT EXISTS idx_node_neighbors_pubkey ON node_neighbors(pubkey, ts);
		CREATE INDEX IF NOT EXISTS idx_companion_channels_companion ON companion_channels(companion_id);
		CREATE INDEX IF NOT EXISTS idx_triggers_companion ON triggers(companion_id);
		CREATE INDEX IF NOT EXISTS idx_trigger_channels_trigger ON trigger_channels(trigger_id);
	`)
	return err
}

// migrateNoop holds a slot migrateV1 already covers, keeping later slots at the version index pre-squash DBs expect.
func migrateNoop(context.Context, dbExecer) error { return nil }

// migrateV2 adds the indexed packet_hash/path columns and backfills rows inserted before they existed.
func migrateV2(ctx context.Context, tx dbExecer) error {
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE packets ADD COLUMN packet_hash TEXT NOT NULL DEFAULT '';
		ALTER TABLE packets ADD COLUMN path TEXT NOT NULL DEFAULT '';
	`); err != nil {
		return fmt.Errorf("adding packet columns: %w", err)
	}

	rows, err := tx.QueryContext(ctx, "SELECT id, raw FROM packets")
	if err != nil {
		return fmt.Errorf("reading packets for backfill: %w", err)
	}
	type row struct {
		id  int64
		raw []byte
	}
	var batch []row
	for rows.Next() {
		var rr row
		if err := rows.Scan(&rr.id, &rr.raw); err != nil {
			rows.Close()
			return fmt.Errorf("scanning packet for backfill: %w", err)
		}
		batch = append(batch, rr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating packets for backfill: %w", err)
	}

	for _, rr := range batch {
		packetHash, path := derivePacketFields(rr.raw)
		if packetHash == "" && path == "" {
			continue // unparseable row; leave the '' defaults
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE packets SET packet_hash = ?, path = ? WHERE id = ?",
			packetHash, path, rr.id); err != nil {
			return fmt.Errorf("backfilling packet %d: %w", rr.id, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_packets_packet_hash ON packets(packet_hash);
		CREATE INDEX IF NOT EXISTS idx_packets_path ON packets(path);
	`); err != nil {
		return fmt.Errorf("indexing packet columns: %w", err)
	}
	return nil
}

// migrateV3 adds the signal-test runner and link-monitor tables.
func migrateV3(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS signal_tests (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			companion_id   INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			label          TEXT    NOT NULL DEFAULT '',
			notes          TEXT    NOT NULL DEFAULT '',
			path           BLOB    NOT NULL,
			path_hash_size INTEGER NOT NULL DEFAULT 1,
			count          INTEGER NOT NULL,
			interval_secs  INTEGER NOT NULL,
			status         TEXT    NOT NULL DEFAULT 'running',
			started_at     INTEGER NOT NULL,
			finished_at    INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS signal_test_runs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			test_id    INTEGER NOT NULL REFERENCES signal_tests(id) ON DELETE CASCADE,
			seq        INTEGER NOT NULL,
			sent_at    INTEGER NOT NULL,
			ok         INTEGER NOT NULL DEFAULT 0,
			hop_snrs   TEXT    NOT NULL DEFAULT '[]',
			snr        REAL,
			elapsed_ms INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_signal_test_runs_test ON signal_test_runs(test_id, seq);

		CREATE TABLE IF NOT EXISTS link_monitors (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			key              BLOB    NOT NULL UNIQUE,
			companion_id     INTEGER NOT NULL REFERENCES companions(id) ON DELETE CASCADE,
			label            TEXT    NOT NULL DEFAULT '',
			path             BLOB    NOT NULL,
			path_hash_size   INTEGER NOT NULL DEFAULT 1,
			interval_secs    INTEGER NOT NULL DEFAULT 900,
			enabled          INTEGER NOT NULL DEFAULT 1,
			-- Display-only UI toggles; the collector keeps recording both readings.
			ignore_first_hop INTEGER NOT NULL DEFAULT 0,
			hide_last_snr    INTEGER NOT NULL DEFAULT 0,
			-- 0 = use the node-monitoring poller's built-in default.
			retry_secs       INTEGER NOT NULL DEFAULT 0,
			max_retries      INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return fmt.Errorf("creating signal test / link monitor tables: %w", err)
	}
	return nil
}

// migrateV4 creates the repeater tables; an absent "*" region means unscoped flood is not relayed, so it is seeded to preserve prior behaviour.
func migrateV4(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS repeater (
			id                    INTEGER PRIMARY KEY CHECK (id = 1),
			name                  TEXT    NOT NULL DEFAULT '',
			private_key           TEXT    NOT NULL DEFAULT '',
			pubkey                TEXT    NOT NULL DEFAULT '',
			latitude              REAL,
			longitude             REAL,
			advert_interval       INTEGER,
			flood_advert_interval INTEGER,
			disable_fwd           INTEGER,
			flood_max             INTEGER,
			flood_max_advert      INTEGER,
			loop_detect           TEXT,
			path_hash_mode        INTEGER,
			owner_info            TEXT    NOT NULL DEFAULT '',
			admin_password        TEXT    NOT NULL DEFAULT '',
			guest_password        TEXT    NOT NULL DEFAULT '',
			regions               TEXT    NOT NULL DEFAULT '[]'
		);
		CREATE TABLE IF NOT EXISTS repeater_acl (
			pubkey         TEXT    PRIMARY KEY,
			permissions    INTEGER NOT NULL DEFAULT 0,
			last_timestamp INTEGER NOT NULL DEFAULT 0,
			last_seen      INTEGER NOT NULL DEFAULT 0
		);
		UPDATE repeater SET regions = '[{"name":"*"}]'
			WHERE id = 1 AND (regions IS NULL OR regions = '' OR regions = '[]');
	`)
	return err
}

// migrateV5 adds flood_max_unscoped (firmware NodePrefs hop cap) and default_region ("" = unscoped adverts).
func migrateV5(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE repeater ADD COLUMN flood_max_unscoped INTEGER;
		ALTER TABLE repeater ADD COLUMN default_region TEXT NOT NULL DEFAULT '';
	`)
	return err
}

// migrateV9 closes a blank admin password. A blank one compared equal to the blank a login sends,
// so any node in range was granted admin (permAdmin) on a repeater created before this. The
// firmware never has a blank one — simple_repeater seeds ADMIN_PASSWORD "password" — so this
// backfills to that same value rather than inventing one, and an operator changes it as they would
// on any MeshCore repeater. Guest is left alone: blank guest grants PERM_ACL_GUEST (0), which is
// what the firmware does too.
func migrateV9(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx,
		`UPDATE repeater SET admin_password = 'password' WHERE admin_password = ''`)
	return err
}

// migrateV10 adds the DM acceptance policy; 'contacts' is what every install did before it, when a DM only decrypted against companion_contacts.
func migrateV10(ctx context.Context, db dbExecer) error {
	for _, q := range []string{
		`ALTER TABLE companions ADD COLUMN dm_policy TEXT NOT NULL DEFAULT 'contacts'`,
		`ALTER TABLE companions ADD COLUMN dm_allow TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// migrateV12 clamps trigger path hash sizes to the 3-byte maximum every other config already used.
// The bot form alone offered 4, and Config.Validate now rejects it — which would wall off every
// later config save, since a save validates the whole assembled config, not just what changed.
func migrateV12(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `UPDATE triggers SET path_hash_size = 3 WHERE path_hash_size > 3`)
	return err
}

// migrateV13 adds the openHop modem's access token. It is a password, so it lives in its own column
// rather than inside the connection string, which the config REST reads hand out in full.
func migrateV13(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE settings ADD COLUMN modem_token TEXT`)
	return err
}

// migrateV11 adds the feed URL an rss or cap trigger polls; empty for every trigger type that has none.
func migrateV11(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE triggers ADD COLUMN url TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateV8 adds settings.spi_board; NULL for a KISS modem, which is every pre-existing install.
func migrateV8(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE settings ADD COLUMN spi_board TEXT`)
	return err
}

// migrateV7 stores the TX duty cycle as a PERCENTAGE (the firmware `set dutycycle` unit); NULL = library default 50%.
func migrateV7(ctx context.Context, db dbExecer) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE settings ADD COLUMN duty_cycle_pct REAL`)
	return err
}

func migrateV6(ctx context.Context, db dbExecer) error {
	for _, q := range []string{
		// CARTO basemap API key.
		`ALTER TABLE settings ADD COLUMN map_tile_key TEXT`,

		// Repeater home region (stored and reported, never routed on).
		`ALTER TABLE repeater ADD COLUMN home_region TEXT NOT NULL DEFAULT ''`,

		// Clamp advert intervals to the firmware's legal ranges; below the minimum becomes 0 (off), matching savePrefs.
		`UPDATE repeater SET
			advert_interval = CASE
				WHEN advert_interval > 0 AND advert_interval < 3600 THEN 0
				WHEN advert_interval > 14400 THEN 14400
				ELSE advert_interval END,
			flood_advert_interval = CASE
				WHEN flood_advert_interval > 0 AND flood_advert_interval < 10800 THEN 0
				WHEN flood_advert_interval > 604800 THEN 604800
				ELSE flood_advert_interval END`,

		// Path hash width is BYTES internally; the shipped path_hash_mode column (bytes-1) must be read here before it is dropped.
		`ALTER TABLE settings ADD COLUMN path_hash_size INTEGER`,
		`ALTER TABLE companions ADD COLUMN path_hash_size INTEGER`,
		`ALTER TABLE repeater ADD COLUMN path_hash_size INTEGER`,
		`UPDATE repeater SET path_hash_size = path_hash_mode + 1 WHERE path_hash_mode IS NOT NULL`,
		`ALTER TABLE repeater DROP COLUMN path_hash_mode`,

		// Relay timing (firmware txdelay / direct.txdelay / rxdelay / multi.acks).
		`ALTER TABLE repeater ADD COLUMN tx_delay_factor REAL`,
		`ALTER TABLE repeater ADD COLUMN direct_tx_delay_factor REAL`,
		`ALTER TABLE repeater ADD COLUMN rx_delay_base REAL`,
		`ALTER TABLE repeater ADD COLUMN multi_acks INTEGER`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// migrateV14 adds optional group bot failover.
func migrateV14(ctx context.Context, db dbExecer) error {
	for _, q := range []string{
		`ALTER TABLE triggers ADD COLUMN failover_pattern TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE triggers ADD COLUMN failover_timeout INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
