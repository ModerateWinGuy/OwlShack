package app

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OwlShack/OwlShack/internal/store"
)

// Lowering the retention freed pages SQLite only reuses, so a 45 MB file stayed 45 MB; a prune that frees most of it gives the disk back, and a small one leaves the pages for the next writes.
func TestPrunePackets_GivesTheDiskBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		old, fresh int
		shrinks    bool
		unswitched bool
	}{
		{"a fifth pruned", 1000, 4000, false, false},
		{"all pruned", 4000, 0, true, false},
		{"a file Open could not switch", 4000, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "prune.db")
			st, err := store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, err := initConfigTables(ctx, st); err != nil {
				t.Fatal(err)
			}
			if tc.unswitched {
				db, err := sql.Open("sqlite", "file:"+path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("PRAGMA auto_vacuum = NONE; VACUUM"); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			raw := bytes.Repeat([]byte{0xAB}, 1000)
			st.WriteSync(func() {
				for i := range tc.old + tc.fresh {
					at := time.Now().Add(-time.Hour)
					if i < tc.old {
						at = at.AddDate(0, 0, -30)
					}
					if err := st.Packets.Insert(ctx, &store.PacketRecord{ReceivedAt: at, Direction: "rx", Raw: raw}); err != nil {
						t.Fatal(err)
					}
				}
			})
			size := func() int64 {
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return fi.Size()
			}
			full := size()
			if full < 4<<20 {
				t.Fatalf("the filled database is %d bytes, too small to show anything", full)
			}

			prunePackets(ctx, st)
			if ctx.Err() != nil {
				t.Fatal("the prune ran until the deadline")
			}
			if n, err := st.Packets.Count(ctx); err != nil || n != int64(tc.fresh) {
				t.Fatalf("%d packets left (%v), want the %d fresh ones", n, err, tc.fresh)
			}
			got := size()
			if tc.shrinks && got > full/10 {
				t.Errorf("the database is %d bytes after pruning every packet, was %d", got, full)
			}
			if !tc.shrinks && got != full {
				t.Errorf("the database is %d bytes after the prune, want its pages kept for reuse at %d", got, full)
			}
		})
	}
}
