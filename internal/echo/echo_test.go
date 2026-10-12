package echo

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/store"
)

type failingModem struct{ sent chan []byte }

func (m failingModem) SendData(data []byte) error {
	m.sent <- data
	if data[len(data)-4] == 2 {
		return errors.New("kiss: tx failed")
	}
	return nil
}
func (failingModem) SetDataHandler(func([]byte, float32, int8, bool)) {}
func (failingModem) AddOutboundHandler(func([]byte))                  {}

// A post the radio took but never transmitted, or still held when it stopped, must not read as sent; each settles on its first result.
func TestTxResult_FailsWhatNeverWentOut(t *testing.T) {
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
	modem := failingModem{sent: make(chan []byte, 8)}
	mux := node.NewRadioMux(modem, node.WithMuxTxResult(tr.TxResult))
	radio := mux.NewRadio()

	names := []string{"went out", "tx failed", "still queued", "received"}
	ids := map[string]int64{}
	frames := map[string][]byte{}
	for i, name := range names {
		m := store.Message{CompanionID: c.ID, Channel: "Public", Sender: "home", Text: name, Direction: "tx", Timestamp: time.Now(), ReceivedAt: time.Now(), FloodScope: "everywhere"}
		db.WriteSync(func() { err = db.Messages.Insert(ctx, &m) })
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = m.ID
		pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeGrpTxt, 0), Payload: []byte{byte(i + 1), 1, 2, 3}}
		frames[name], _ = pkt.ToBytes()
		sent := Sent{MessageID: m.ID, CompanionID: c.ID, Companion: "home", Channel: "Public"}
		if name == "received" {
			tr.Track(pkt.PacketHash(), sent)
			continue
		}
		tr.TrackQueued(pkt.PacketHash(), sent)
		delay := time.Duration(0)
		if name == "still queued" {
			delay = time.Hour
		}
		radio.Enqueue(frames[name], node.PrioritySend, delay)
	}
	for range 2 {
		select {
		case <-modem.sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the radio never took the posts")
		}
	}
	time.Sleep(100 * time.Millisecond)
	mux.Stop()
	time.Sleep(200 * time.Millisecond)
	tr.TxResult(frames["went out"], errors.New("a later resend failed"))
	db.WriteSync(func() {})

	for name, want := range map[string]string{"went out": "", "tx failed": "failed", "still queued": "failed", "received": ""} {
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
