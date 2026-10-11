package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func appQueueStore(t *testing.T, enabled bool) (*Store, int64) {
	t.Helper()
	st := newTestStore(t)
	c := Companion{Name: "home", AppEnabled: enabled, AppPort: 5000}
	if err := st.Companions.Create(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	return st, c.ID
}

func pop(t *testing.T, st *Store, id int64) []byte {
	t.Helper()
	f, ok, err := st.AppQueue.Pop(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return nil
	}
	return f
}

// Nothing is held for an app that has no access, and turning access off forgets what was waiting.
func TestAppQueue_OnlyWhileAccessIsOn(t *testing.T) {
	ctx := context.Background()
	st, id := appQueueStore(t, false)
	if err := st.AppQueue.Push(ctx, id, false, []byte{16, 1}, 256); err != nil {
		t.Fatal(err)
	}
	if f := pop(t, st, id); f != nil {
		t.Fatalf("queued with access off: % x", f)
	}

	c, _ := st.Companions.Get(ctx, id)
	c.AppEnabled = true
	if err := st.Companions.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	st.AppQueue.Push(ctx, id, false, []byte{16, 2}, 256)
	st.AppQueue.Push(ctx, id, true, []byte{17, 3}, 256)
	if f := pop(t, st, id); f[1] != 2 {
		t.Fatalf("first out = % x, want the first in", f)
	}
	c.AppEnabled = false
	if err := st.Companions.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.AppEnabled = true
	st.Companions.Update(ctx, c)
	if f := pop(t, st, id); f != nil {
		t.Fatalf("kept across access off: % x", f)
	}
}

// Full, the queue drops its oldest channel message for a new one, and drops the new one when it holds only direct messages (firmware addToOfflineQueue).
func TestAppQueue_Full(t *testing.T) {
	ctx := context.Background()
	st, id := appQueueStore(t, true)
	const size = 16
	st.AppQueue.Push(ctx, id, true, []byte{17, 0}, size)
	for i := 1; i < size; i++ {
		st.AppQueue.Push(ctx, id, false, []byte{16, byte(i)}, size)
	}
	st.AppQueue.Push(ctx, id, false, []byte{16, 0xee}, size)
	if f := pop(t, st, id); f[0] != 16 || f[1] != 1 {
		t.Fatalf("head = % x, want the channel message gone", f)
	}
	st.AppQueue.Push(ctx, id, false, []byte{16, 0xef}, size) // one free slot again
	st.AppQueue.Push(ctx, id, false, []byte{16, 0xff}, size) // full of direct messages: dropped
	var last []byte
	for f := pop(t, st, id); f != nil; f = pop(t, st, id) {
		last = f
	}
	if last[1] != 0xef {
		t.Fatalf("last = % x, want the message that fitted, not the one past full", last)
	}
}

func drain(t *testing.T, st *Store, id int64) [][]byte {
	t.Helper()
	var out [][]byte
	for f := pop(t, st, id); f != nil; f = pop(t, st, id) {
		out = append(out, f)
	}
	return out
}

// A lowered size trims the oldest channel messages on the next push; direct messages past it stay until the app collects them.
func TestAppQueue_SizeLowered(t *testing.T) {
	ctx := context.Background()
	st, id := appQueueStore(t, true)
	for i := range 10 {
		st.AppQueue.Push(ctx, id, false, []byte{16, byte(i)}, 256)
	}
	for i := range 20 {
		st.AppQueue.Push(ctx, id, true, []byte{17, byte(i)}, 256)
	}
	if err := st.AppQueue.Push(ctx, id, false, []byte{16, 0xee}, 16); err != nil {
		t.Fatal(err)
	}
	var want [][]byte
	for i := range 10 {
		want = append(want, []byte{16, byte(i)})
	}
	for i := 15; i < 20; i++ {
		want = append(want, []byte{17, byte(i)})
	}
	want = append(want, []byte{16, 0xee})
	if got := drain(t, st, id); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queue = %v, want the 15 oldest channel messages gone and the new one in: %v", got, want)
	}

	for i := range 18 {
		st.AppQueue.Push(ctx, id, false, []byte{16, byte(i)}, 256)
	}
	st.AppQueue.Push(ctx, id, true, []byte{17, 0}, 256)
	st.AppQueue.Push(ctx, id, true, []byte{17, 1}, 256)
	if err := st.AppQueue.Push(ctx, id, true, []byte{17, 0xee}, 16); err != nil {
		t.Fatal(err)
	}
	want = nil
	for i := range 18 {
		want = append(want, []byte{16, byte(i)})
	}
	if got := drain(t, st, id); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queue = %v, want the 18 direct messages kept and every channel message, the new one too, dropped: %v", got, want)
	}
}

// Companions from before the setting hold what the firmware's offline queue holds.
func TestMigration032_AppQueueSizeDefault(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := dbAt(t, path, 31)
	exec(t, db, `INSERT INTO companions (id, name, private_key, pubkey) VALUES (1, 'c', 'aa', 'k1')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := st.Companions.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.AppQueueSize != 256 {
		t.Errorf("app queue size %d, want 256", c.AppQueueSize)
	}
}
