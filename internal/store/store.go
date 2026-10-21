package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	HopPins        *HopPinRepo
	Contacts       *ContactRepo
	Packets        *PacketRepo
	Messages       *MessageRepo
	Conversations  *ConversationRepo
	Echoes         *EchoRepo
	BlockedSenders *BlockedSenderRepo
	Metrics        *MetricsRepo
	AppConfig      *AppConfigRepo
	Settings       *SettingsRepo
	Sensors        *SensorRepo
	TelemetryMap   *TelemetryMapRepo
	Mqtt           *MqttRepo
	Brokers        *BrokerRepo
	Companions     *CompanionRepo
	Channels       *ChannelRepo
	Triggers       *TriggerRepo
	SignalTests    *SignalTestRepo
	LinkMonitors   *LinkMonitorRepo
	Repeater       *RepeaterRepo
	RepeaterACL    *RepeaterACLRepo
	AppQueue       *AppQueueRepo

	path       string
	writerCh   chan func()
	writerDone chan struct{}
	closing    chan struct{}
	closeOnce  sync.Once
	dropped    atomic.Uint64
	// lastDrop is when the most recent write was dropped, in unix nanos; 0 means never.
	lastDrop atomic.Int64
}

// walSizeLimit is what a checkpoint trims the WAL back to; without it the file stays the size of the largest burst ever written.
const walSizeLimit = 4 << 20

func Open(ctx context.Context, path string) (*Store, error) {
	// Only the "_pragma=" form takes (mattn-style is ignored), and each runs on every pooled connection, so none may write.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)" +
		fmt.Sprintf("&_pragma=journal_size_limit(%d)&_pragma=synchronous(NORMAL)", walSizeLimit)
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
		path:           path,
		Peers:          &PeerRepo{db: db},
		HopPins:        &HopPinRepo{db: db},
		Contacts:       &ContactRepo{db: db},
		Packets:        &PacketRepo{db: db},
		Messages:       &MessageRepo{db: db, maxRows: DefaultMaxMessages},
		Conversations:  &ConversationRepo{db: db},
		Echoes:         &EchoRepo{db: db},
		BlockedSenders: &BlockedSenderRepo{db: db},
		Metrics:        &MetricsRepo{db: db},
		AppConfig:      &AppConfigRepo{db: db},
		Settings:       &SettingsRepo{db: db},
		Sensors:        &SensorRepo{db: db},
		TelemetryMap:   &TelemetryMapRepo{db: db},
		Mqtt:           &MqttRepo{db: db},
		Brokers:        &BrokerRepo{db: db},
		Companions:     &CompanionRepo{db: db},
		Channels:       &ChannelRepo{db: db},
		Triggers:       &TriggerRepo{db: db},
		SignalTests:    &SignalTestRepo{db: db},
		LinkMonitors:   &LinkMonitorRepo{db: db},
		Repeater:       &RepeaterRepo{db: db},
		RepeaterACL:    &RepeaterACLRepo{db: db},
		AppQueue:       &AppQueueRepo{db: db},
		writerCh:       make(chan func(), writerQueueDepth),
		writerDone:     make(chan struct{}),
		closing:        make(chan struct{}),
	}

	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	if err := s.useIncrementalVacuum(ctx); err != nil {
		slog.Warn("space the database frees stays in its file until a later start can rewrite it", "error", err)
	}

	go s.writerLoop()

	return s, nil
}

// WriterStats reports the write queue and its drops; lastDrop is zero when none, which the ever-rising count alone cannot say.
func (s *Store) WriterStats() (queued, capacity int, dropped uint64, lastDrop time.Time) {
	if nanos := s.lastDrop.Load(); nanos != 0 {
		lastDrop = time.Unix(0, nanos)
	}
	return len(s.writerCh), cap(s.writerCh), s.dropped.Load(), lastDrop
}

// WriteAsync queues fn on the writer goroutine and never blocks; false means the queue was full and fn was dropped.
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
	done := make(chan struct{})
	select {
	case s.writerCh <- func() {
		defer close(done)
		fn()
		s.checkpoint()
	}:
	case <-s.closing:
		s.writeAfterClose(fn)
		return
	}
	select {
	case <-done:
	case <-s.closing:
		<-s.writerDone
		select {
		case <-done:
		default:
			s.writeAfterClose(fn)
		}
	}
}

// writeAfterClose runs fn on the caller once the writer stops; skipping it leaves the caller's error nil, which reads as success.
func (s *Store) writeAfterClose(fn func()) {
	<-s.writerDone
	fn()
}

// checkpoint syncs the WAL and copies it into the main file, which NORMAL otherwise leaves until the WAL fills.
// ponytail: PASSIVE never waits on a reader, so one holding an older snapshot can leave a save in the WAL until the next checkpoint.
func (s *Store) checkpoint() {
	if _, err := s.db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		slog.Warn("store checkpoint failed; the last save may not survive a power cut", "error", err)
	}
}

// useIncrementalVacuum lets ReleaseFreePages give space back a batch at a time, which a file takes on only through one full VACUUM, so Open runs it before anything else writes.
func (s *Store) useIncrementalVacuum(ctx context.Context) error {
	var mode, pages, free, size int64
	if err := s.db.QueryRowContext(ctx, `SELECT auto_vacuum, page_count, freelist_count, page_size
		FROM pragma_auto_vacuum(), pragma_page_count(), pragma_freelist_count(), pragma_page_size()`).Scan(&mode, &pages, &free, &size); err != nil {
		return err
	}
	if mode == 2 {
		return nil
	}
	// VACUUM writes what is left to a temp file, kept beside the database rather than a tmpfs /tmp, and again to the WAL.
	need := 2 * (pages - free) * size
	if avail, ok := s.DiskFreeBytes(); ok && avail < uint64(need) {
		return fmt.Errorf("it needs %d bytes free and the disk has %d", need, avail)
	}
	slog.Info("rewriting the database once so the space it frees can go back to the disk", "bytes", pages*size)
	dir := strings.ReplaceAll(filepath.Dir(s.path), "'", "''")
	defer s.db.ExecContext(context.Background(), "PRAGMA temp_store_directory = ''")
	// One Exec, so the setting and the VACUUM that applies it share a connection.
	if _, err := s.db.ExecContext(ctx, "PRAGMA temp_store_directory = '"+dir+"'; PRAGMA auto_vacuum = INCREMENTAL; VACUUM"); err != nil {
		return err
	}
	s.checkpoint()
	return nil
}

// FreePages is how many of the file's pages are free; SQLite reuses them but gives none back to the disk unless ReleaseFreePages does.
func (s *Store) FreePages(ctx context.Context) (free, total int64, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT freelist_count, page_count FROM pragma_freelist_count(), pragma_page_count()`).Scan(&free, &total); err != nil {
		return 0, 0, fmt.Errorf("counting free pages: %w", err)
	}
	return free, total, nil
}

// ReleaseFreePages gives up to n free pages back to the disk and reports how many are left; run it on the writer, where n bounds the turn.
func (s *Store) ReleaseFreePages(ctx context.Context, n int) (left int64, err error) {
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", n)); err != nil {
		return 0, fmt.Errorf("releasing free pages: %w", err)
	}
	left, _, err = s.FreePages(ctx)
	return left, err
}

// WALBytes is the write-ahead log's size on disk, 0 when there is none; one that keeps growing means checkpoints are not completing.
func (s *Store) WALBytes() int64 {
	fi, err := os.Stat(s.path + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
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

// unixMS scans a time column, which holds UTC unix milliseconds; NULL scans as the zero time.
func unixMS(t *time.Time) sql.Scanner { return msScanner{t} }

type msScanner struct{ t *time.Time }

func (s msScanner) Scan(v any) error {
	switch v := v.(type) {
	case int64:
		*s.t = time.UnixMilli(v)
	case nil:
		*s.t = time.Time{}
	default:
		return fmt.Errorf("time column holds %T %v, want unix milliseconds", v, v)
	}
	return nil
}
