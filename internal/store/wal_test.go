package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openAt(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// A checkpoint rewinds the WAL but never shrinks it, so one burst (a restore, a retention trim) kept its size for good.
func TestOpen_TheWALShrinksBackAfterABurst(t *testing.T) {
	t.Parallel()
	st, path := openAt(t)
	ctx := t.Context()
	blob := strings.Repeat("x", 3000)
	st.WriteSync(func() {
		if _, err := st.db.ExecContext(ctx, `CREATE TABLE junk (b TEXT)`); err != nil {
			t.Error(err)
			return
		}
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		for range 4000 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO junk VALUES (?)`, blob); err != nil {
				t.Error(err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	})
	if n := fileSize(t, path+"-wal"); n <= walSizeLimit {
		t.Fatalf("the burst left a %d byte WAL, so it proves nothing about the limit", n)
	}
	st.WriteSync(func() { st.db.ExecContext(ctx, `INSERT INTO junk VALUES ('a')`) })
	if n := fileSize(t, path+"-wal"); n > walSizeLimit {
		t.Errorf("WAL is %d bytes after the next write, want at most %d", n, walSizeLimit)
	}
}

// NORMAL only syncs at a checkpoint, which saves a flush per commit on an SD card; the pool must hand it to every connection.
func TestOpen_SyncsAtCheckpointsNotEveryCommit(t *testing.T) {
	t.Parallel()
	st, _ := openAt(t)
	conns := make([]interface{ Close() error }, 0, 3)
	for range 3 {
		c, err := st.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var mode int
		if err := c.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != 1 {
			t.Errorf("synchronous = %d on a pooled connection, want 1 (NORMAL)", mode)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

// Under NORMAL a commit can sit unsynced in the WAL, so a save the page reported as done could come back after a power cut unless WriteSync checkpoints it.
func TestWriteSync_ASaveIsInTheMainFileWhenItReturns(t *testing.T) {
	t.Parallel()
	st, path := openAt(t)
	var id int64
	st.WriteSync(func() { id = mkCompanion(t, st, "durable") })

	// Only the main file, as if the WAL's unsynced tail were lost.
	copyPath := filepath.Join(t.TempDir(), "main-only.db")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openWritableDB(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRowContext(t.Context(), `SELECT name FROM companions WHERE id = ?`, id).Scan(&name); err != nil {
		t.Fatalf("the saved companion is not in the main file: %v", err)
	}
}

// A file only gives free pages back once switched to incremental vacuum, which an older one takes on at its next start, data intact.
func TestOpen_SwitchesToIncrementalVacuum(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "older.db")
	markedDB(t, path, LatestSchemaVersion()-1, "kept")
	for _, p := range []string{path, ""} {
		var st *Store
		if p == "" {
			st, p = openAt(t)
		} else {
			var err error
			if st, err = Open(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			defer st.Close()
		}
		var mode int
		if err := st.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
			t.Errorf("%s: auto_vacuum %d (%v), want 2 (incremental)", filepath.Base(p), mode, err)
		}
	}
	if got := markedName(t, path); got != "kept" {
		t.Errorf("the switched database holds %q, want kept", got)
	}
}

// The rewrite's temp copy goes beside the database, where the free space was checked, not to a tmpfs /tmp that a large file fills.
func TestOpen_TheRewriteCopyStaysBesideTheDatabase(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc/self/fd")
	}
	path := filepath.Join(t.TempDir(), "big.db")
	db, err := openWritableDB(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `CREATE TABLE junk (b BLOB)`)
	exec(t, db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1500)
		INSERT INTO junk SELECT randomblob(4000) FROM n`)
	db.Close()

	done := make(chan struct{})
	dirs := make(chan map[string]bool)
	go func() {
		seen := map[string]bool{}
		for {
			select {
			case <-done:
				dirs <- seen
				return
			default:
			}
			fds, _ := os.ReadDir("/proc/self/fd")
			for _, fd := range fds {
				if l, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && strings.Contains(l, "etilqs_") {
					seen[filepath.Dir(l)] = true
				}
			}
		}
	}()
	st, err := Open(t.Context(), path)
	close(done)
	seen := <-dirs
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !seen[filepath.Dir(path)] {
		t.Errorf("the rewrite's temp copy went to %v, want %s", seen, filepath.Dir(path))
	}
}

// Each call is one bounded writer turn, so it gives back at most the pages asked for, and the file shrinks by them.
func TestReleaseFreePages_ABatchPerCall(t *testing.T) {
	t.Parallel()
	st, path := openAt(t)
	ctx := t.Context()
	exec(t, st.db, `CREATE TABLE junk (b BLOB)`)
	exec(t, st.db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
		INSERT INTO junk SELECT randomblob(3000) FROM n`)
	st.WriteSync(func() { exec(t, st.db, `DELETE FROM junk`) })
	free, _, err := st.FreePages(ctx)
	if err != nil || free < 1000 {
		t.Fatalf("%d pages free after the delete (%v), want over 1000", free, err)
	}
	full := fileSize(t, path)

	var left int64
	st.WriteSync(func() { left, err = st.ReleaseFreePages(ctx, 100) })
	if err != nil || left != free-100 {
		t.Fatalf("%d pages left free (%v), want %d", left, err, free-100)
	}
	if got := fileSize(t, path); got != full-100*4096 {
		t.Errorf("the file is %d bytes, want %d after giving back 100 pages", got, full-100*4096)
	}
}

func TestStore_WALBytesReportsTheWAL(t *testing.T) {
	t.Parallel()
	st, path := openAt(t)
	mkCompanion(t, st, "a")
	if got, want := st.WALBytes(), fileSize(t, path+"-wal"); got != want || got == 0 {
		t.Errorf("WALBytes = %d, want the file's %d (and not 0 after a write)", got, want)
	}
}
