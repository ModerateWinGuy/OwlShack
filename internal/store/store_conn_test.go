package store

import (
	"context"
	"sync/atomic"
	"testing"
)

// A connection the pool opens must write nothing: a write outside the writer goroutine fails a read-then-write transaction with "database is locked", and the write is lost.
func TestOpen_ANewConnectionWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, id := appQueueStore(t, true)
	st.db.SetMaxIdleConns(0)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			if _, err := st.Companions.Get(ctx, id); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var failed int
	for i := range 1000 {
		st.WriteAsync(func() {
			if err := st.AppQueue.Push(ctx, id, true, []byte{17, byte(i)}, 256); err != nil {
				failed++
			}
		})
	}
	st.WriteSync(func() {})
	stop.Store(true)
	<-done
	if failed != 0 {
		t.Errorf("%d of 1000 writes were lost to a new connection's own write", failed)
	}
}
