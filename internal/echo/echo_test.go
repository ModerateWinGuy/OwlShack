package echo

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"

	"github.com/OwlShack/OwlShack/internal/store"
)

// A radio reset throws the TX queue away, so a post still in it never went out and must not read as sent.
func TestFailQueued_FailsOnlyWhatNeverReachedTheModem(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "echo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := store.Companion{Name: "home"}
	db.WriteSync(func() { err = db.Companions.Create(ctx, &c) })
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTracker(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	frame := func(b byte) ([]byte, [meshcore.PacketHashSize]byte) {
		pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeGrpTxt, 0), Payload: []byte{b, 1, 2, 3}}
		data, err := pkt.ToBytes()
		if err != nil {
			t.Fatal(err)
		}
		return data, pkt.PacketHash()
	}
	ids := map[string]int64{}
	for _, name := range []string{"went out", "still queued", "received"} {
		m := store.Message{CompanionID: c.ID, Channel: "Public", Sender: "home", Text: name, Direction: "tx", Timestamp: time.Now(), ReceivedAt: time.Now(), FloodScope: "everywhere"}
		db.WriteSync(func() { err = db.Messages.Insert(ctx, &m) })
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = m.ID
	}
	sentData, sentHash := frame(1)
	_, queuedHash := frame(2)
	_, heardHash := frame(3)
	tr.TrackQueued(sentHash, Sent{MessageID: ids["went out"], CompanionID: c.ID, Companion: "home", Channel: "Public"})
	tr.TrackQueued(queuedHash, Sent{MessageID: ids["still queued"], CompanionID: c.ID, Companion: "home", Channel: "Public"})
	tr.Track(heardHash, Sent{MessageID: ids["received"], CompanionID: c.ID, Companion: "home", Channel: "Public"})
	tr.OnAir(sentData)
	tr.FailQueued()
	db.WriteSync(func() {})

	for name, want := range map[string]string{"went out": "", "still queued": "failed", "received": ""} {
		m, err := db.Messages.GetByID(ctx, ids[name])
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if m.Status != nil {
			got = *m.Status
		}
		if got != want {
			t.Errorf("%s: status %q, want %q", name, got, want)
		}
	}
}

// TestPrune: entries past ttl are dropped, fresh ones kept.
func TestPrune(t *testing.T) {
	tr := NewTracker(nil, nil, nil)
	tr.ttl = 10 * time.Millisecond
	var old, fresh [meshcore.PacketHashSize]byte
	old[0], fresh[0] = 1, 2

	oldKey := pendingKey{hash: old, companion: "a"}
	freshKey := pendingKey{hash: fresh, companion: "a"}
	tr.pending[oldKey] = &entry{registeredAt: time.Now().Add(-time.Second)}
	tr.pending[freshKey] = &entry{registeredAt: time.Now()}
	tr.Prune()

	if _, ok := tr.pending[oldKey]; ok {
		t.Error("expired entry survived Prune")
	}
	if _, ok := tr.pending[freshKey]; !ok {
		t.Error("fresh entry was pruned")
	}
}

// Two companions on the same channel see the same packet: one sends it, the other decodes it as a
// received message. Keyed on the hash alone the second Track replaced the first, so the sender kept
// only the echoes that arrived before the overwrite — one, in practice — and lost the rest.
func TestTrack_TwoCompanionsOnOnePacketDoNotCollide(t *testing.T) {
	tr := NewTracker(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var hash [meshcore.PacketHashSize]byte
	hash[0] = 9

	tr.Track(hash, Sent{MessageID: 101, CompanionID: 1, Companion: "Wes", Channel: "#westest"})     // the sender
	tr.Track(hash, Sent{MessageID: 202, CompanionID: 2, Companion: "Wes Bot", Channel: "#westest"}) // the other companion receiving the same packet

	if len(tr.pending) != 2 {
		t.Fatalf("pending holds %d entries, want 2 — the second companion overwrote the first", len(tr.pending))
	}
	if e := tr.pending[pendingKey{hash: hash, companion: "Wes"}]; e == nil || e.sent.MessageID != 101 {
		t.Errorf("sender's entry = %+v, want messageID 101 still tracked", e)
	}
	if e := tr.pending[pendingKey{hash: hash, companion: "Wes Bot"}]; e == nil || e.sent.MessageID != 202 {
		t.Errorf("other companion's entry = %+v, want messageID 202", e)
	}
}
