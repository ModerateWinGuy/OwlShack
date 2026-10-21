package monitor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/OwlShack/OwlShack/internal/store"
)

type captureBC struct{ got []map[string]any }

func (c *captureBC) Broadcast(topic string, data any) {
	if topic == WSTopicMetrics {
		c.got = append(c.got, data.(map[string]any))
	}
}

// A sensor's push lands in its history and live view as a poll would, at the time it arrived.
func TestRecordPush(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bc := &captureBC{}
	s := New(st, bc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pk := bytes.Repeat([]byte{0xAB}, 32)

	// A failed renewal poll stays on show however many pushes follow it.
	st.WriteSync(func() {
		err = st.Metrics.MarkPollFailure(ctx, &store.NodeState{Pubkey: pk, CompanionID: "home", Kind: "sensor", LastPollTS: 100, LastError: "login: no reply"})
	})
	if err != nil {
		t.Fatal(err)
	}
	s.RecordPush(Target{Pubkey: pk, CompanionID: "home", Kind: "sensor"}, "sens", []Reading{{Metric: "temperature_ch2", Channel: 2, Value: 21.5}})
	st.WriteSync(func() {}) // waits out the async write queued before it

	pts, err := st.Metrics.QueryHistory(ctx, pk, "temperature_ch2", 0, 1<<40, 0)
	if err != nil || len(pts) != 1 || pts[0].Value != 21.5 {
		t.Fatalf("history %+v %v, want the pushed 21.5", pts, err)
	}
	states, err := st.Metrics.ListNodeStates(ctx)
	if err != nil || len(states) != 1 || states[0].Kind != "sensor" || states[0].Name != "sens" || states[0].LastOkTS != pts[0].TS || states[0].LastPollTS != 100 || states[0].LastError != "login: no reply" {
		t.Fatalf("node state %+v %v", states, err)
	}
	if len(bc.got) != 1 || bc.got[0]["kind"] != "sensor" || bc.got[0]["pushed"] != true || bc.got[0]["companionId"] != "home" || bc.got[0]["metrics"].(map[string]float64)["temperature_ch2"] != 21.5 {
		t.Fatalf("broadcast %+v", bc.got)
	}
}

func TestPollSoon(t *testing.T) {
	s := newReschedTestService()
	pk := []byte{0xAB}
	s.rescheduleAfter("ab", Target{Pubkey: pk}, true)
	if s.dueIn("ab") < time.Hour {
		t.Fatal("not scheduled")
	}
	s.PollSoon(pk)
	s.mu.RLock()
	_, scheduled := s.nextDue["ab"]
	s.mu.RUnlock()
	if scheduled {
		t.Error("still scheduled after PollSoon")
	}

	// Its poll then runs and reschedules as usual: one request, one poll.
	s.setInFlight("ab")
	s.rescheduleAfter("ab", Target{Pubkey: pk}, true)
	if s.dueIn("ab") < time.Hour {
		t.Error("a PollSoon made before the poll polled twice")
	}

	// Asked again while that node's poll is in flight, which read the settings before the change.
	s.setInFlight("ab")
	s.PollSoon(pk)
	s.rescheduleAfter("ab", Target{Pubkey: pk}, true)
	s.mu.RLock()
	_, scheduled = s.nextDue["ab"]
	s.mu.RUnlock()
	if scheduled {
		t.Error("a PollSoon made mid-poll was lost when that poll rescheduled")
	}
}
