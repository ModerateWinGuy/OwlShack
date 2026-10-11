package echo

import (
	"context"
	"sync"
	"time"

	"log/slog"

	"github.com/OwlShack/OwlShack/internal/api"
	"github.com/OwlShack/OwlShack/internal/store"
	meshcore "github.com/OwlShack/meshcore-go"
)

// Sent identifies the message a tracked packet belongs to. A struct rather than four arguments:
// messageID and companionID are both int64 and adjacent, so a swapped pair would compile and
// attach every echo to the wrong message.
type Sent struct {
	MessageID   int64
	CompanionID int64
	Companion   string
	Channel     string
}

type entry struct {
	sent         Sent
	registeredAt time.Time
}

// pendingKey scopes a tracked packet to the companion that is waiting on it. Keying on the hash
// alone collided whenever two companions shared a channel: one sends, the other decodes the very
// same packet as a received message, and its Track overwrote the sender's entry — so the sender
// kept the one echo that arrived before the overwrite and lost every repeat after it.
type pendingKey struct {
	hash      [meshcore.PacketHashSize]byte
	companion string
}

type Tracker struct {
	mu      sync.Mutex
	pending map[pendingKey]*entry
	// queued is each post in the TX queue, not yet written to the modem.
	queued map[[meshcore.PacketHashSize]byte]Sent
	store  *store.Store
	hub    *api.Hub
	log    *slog.Logger
	ttl    time.Duration
}

func NewTracker(st *store.Store, hub *api.Hub, log *slog.Logger) *Tracker {
	return &Tracker{
		pending: make(map[pendingKey]*entry),
		queued:  make(map[[meshcore.PacketHashSize]byte]Sent),
		store:   st,
		hub:     hub,
		log:     log,
		ttl:     30 * time.Second,
	}
}

func (t *Tracker) Track(hash [meshcore.PacketHashSize]byte, sent Sent) {
	t.mu.Lock()
	t.pending[pendingKey{hash: hash, companion: sent.Companion}] = &entry{
		sent:         sent,
		registeredAt: time.Now(),
	}
	t.mu.Unlock()
	t.log.Debug("echo tracked", "messageID", sent.MessageID, "hash", hash, "companion", sent.Companion, "channel", sent.Channel)
}

// TrackQueued tracks an outgoing post from when it joins the TX queue, so a reset that drops the queue can fail it.
func (t *Tracker) TrackQueued(hash [meshcore.PacketHashSize]byte, sent Sent) {
	t.mu.Lock()
	t.queued[hash] = sent
	t.mu.Unlock()
	t.Track(hash, sent)
}

// OnAir is every frame written to the modem: a post that got that far has left the queue.
func (t *Tracker) OnAir(data []byte) {
	pkt, err := meshcore.PacketFromBytes(data)
	if err != nil {
		return
	}
	t.mu.Lock()
	delete(t.queued, pkt.PacketHash())
	t.mu.Unlock()
}

// FailQueued fails every post the modem never got, once a radio reset has thrown the TX queue away.
func (t *Tracker) FailQueued() {
	t.mu.Lock()
	lost := t.queued
	t.queued = map[[meshcore.PacketHashSize]byte]Sent{}
	t.mu.Unlock()
	for _, s := range lost {
		t.store.WriteAsync(func() {
			if err := t.store.Messages.UpdateStatus(context.Background(), s.MessageID, "failed"); err != nil {
				t.log.Error("failed to mark a dropped post failed", "error", err, "messageID", s.MessageID)
			}
		})
		if t.hub != nil {
			t.hub.Broadcast("messages", map[string]any{
				"action":      "status",
				"companion":   s.Companion,
				"companionId": s.CompanionID,
				"channel":     s.Channel,
				"id":          s.MessageID,
				"status":      "failed",
			})
		}
	}
}

// OnRawPacket is called by each companion for every frame it hears, so companion scopes the lookup
// to that companion's own tracked message.
func (t *Tracker) OnRawPacket(companion string, data []byte, snr float32, rssi int8, hasSignalInfo bool) {
	pkt, err := meshcore.PacketFromBytes(data)
	if err != nil {
		return
	}

	key := pendingKey{hash: pkt.PacketHash(), companion: companion}

	t.mu.Lock()
	entry, ok := t.pending[key]
	if !ok {
		t.mu.Unlock()
		return
	}

	if time.Since(entry.registeredAt) > t.ttl {
		delete(t.pending, key)
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	t.log.Debug("echo matched", "messageID", entry.sent.MessageID, "companion", companion, "hash", key.hash, "hops", pkt.PathHashCount())

	var snrPtr *float64
	var rssiPtr *int8
	if hasSignalInfo {
		s := float64(snr)
		snrPtr = &s
		rssiPtr = &rssi
	}

	echo := &store.MessageEcho{
		MessageID:    entry.sent.MessageID,
		ReceivedAt:   time.Now(),
		PathHashes:   pkt.Path,
		PathHashSize: int(pkt.PathHashSize()),
		Hops:         int(pkt.PathHashCount()),
		SNR:          snrPtr,
		RSSI:         rssiPtr,
	}

	t.store.WriteAsync(func() {
		if err := t.store.Echoes.Insert(context.Background(), echo); err != nil {
			t.log.Error("failed to insert echo", "error", err, "messageID", entry.sent.MessageID)
			return
		}

		if echo.ID == 0 {
			return
		}

		count, err := t.store.Messages.IncrementRepeatCount(context.Background(), entry.sent.MessageID)
		if err != nil {
			t.log.Error("failed to increment repeat count", "error", err, "messageID", entry.sent.MessageID)
			return
		}

		if t.hub != nil {
			t.hub.Broadcast("messages", map[string]any{
				"action":      "repeatCount",
				"companion":   entry.sent.Companion,
				"companionId": entry.sent.CompanionID,
				"channel":     entry.sent.Channel,
				"id":          entry.sent.MessageID,
				"repeatCount": count,
			})
		}
	})
}

// PruneLoop drops expired entries every ttl; without it, never-echoed messages stay in the map forever.
func (t *Tracker) PruneLoop(ctx context.Context) {
	tick := time.NewTicker(t.ttl)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.Prune()
		}
	}
}

func (t *Tracker) Prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for key, entry := range t.pending {
		if now.Sub(entry.registeredAt) > t.ttl {
			delete(t.pending, key)
		}
	}
}
