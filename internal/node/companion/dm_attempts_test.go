package companion

import (
	"encoding/binary"
	"encoding/hex"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
)

// timedRadio records when each packet was queued and the delay it asked for, and hands received packets to the node.
type timedRadio struct {
	recordingRadio
	mu     sync.Mutex
	at     []time.Time
	delays []time.Duration
	rx     func(*meshcore.Packet)
}

func (r *timedRadio) Enqueue(d []byte, p uint8, delay time.Duration) bool {
	r.mu.Lock()
	r.at, r.delays = append(r.at, time.Now()), append(r.delays, delay)
	r.mu.Unlock()
	return r.recordingRadio.Enqueue(d, p, delay)
}

func (r *timedRadio) SetDataHandler(h func(*meshcore.Packet)) { r.rx = h }

// timedCompanion is hashSizeCompanion on a timedRadio whose airtime estimate is 100 ms, with friend a 0-hop neighbour.
func timedCompanion(t *testing.T) (*Companion, *timedRadio, meshcore.LocalIdentity, meshcore.LocalIdentity) {
	t.Helper()
	c, _, self, friend := hashSizeCompanion(t)
	radio := &timedRadio{}
	c.node = node.New(self, radio, node.WithAirtimeEstimator(func(int) uint32 { return 100 }))
	t.Cleanup(c.node.Stop)
	c.node.Peers().Insert(&node.Peer{Identity: friend.Identity, Name: "friend", OutPath: []byte{}, OutPathHashSize: 2})
	return c, radio, self, friend
}

// decryptDM opens a DM the companion sent friend.
func decryptDM(t *testing.T, raw []byte, friend, self meshcore.LocalIdentity) (*meshcore.Packet, meshcore.TextPlaintext) {
	t.Helper()
	pkt, err := meshcore.PacketFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := friend.SharedSecret(self.Identity)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := meshcore.TextMessageFromBytes(pkt.Payload)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := meshcore.ParseTextPlaintext(msg.Decrypt(secret))
	if err != nil {
		t.Fatal(err)
	}
	return pkt, pt
}

// Firmware's sendMessage per attempt: three along the route then floods, numbered 0 on with one timestamp, each waiting its own route's estimate.
func TestSendDM_AttemptsAlongTheRouteThenFloodEachOnItsOwnWait(t *testing.T) {
	t.Parallel()
	c, radio, self, friend := timedCompanion(t)
	if err := c.sendDMReply(hex.EncodeToString(friend.PublicKeyBytes()), "hi", 2, time.Millisecond, c.contactScopeHex(hex.EncodeToString(friend.PublicKeyBytes()))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		radio.mu.Lock()
		n := len(radio.at)
		radio.mu.Unlock()
		if n >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := radio.take()
	if len(sent) != 5 {
		t.Fatalf("sent %d attempts, want 5", len(sent))
	}
	var ts uint32
	for i, raw := range sent {
		pkt, pt := decryptDM(t, raw, friend, self)
		if i == 0 {
			ts = pt.Timestamp
		}
		if pt.Attempt != i || pt.Timestamp != ts || pkt.IsRouteDirect() != (i < 3) {
			t.Errorf("send %d: attempt %d, timestamp %d, direct %v; want attempt %d, timestamp %d, direct %v", i, pt.Attempt, pt.Timestamp, pkt.IsRouteDirect(), i, ts, i < 3)
		}
	}
	// Direct 0-hop waits 500+(6*100+250) ms and a flood 500+16*100 ms; the ACK sweep adds up to 500 ms.
	radio.mu.Lock()
	at := radio.at
	radio.mu.Unlock()
	for i := 1; i < 5; i++ {
		gap, want := at[i].Sub(at[i-1]), node.CalcDirectTimeout(100, 0)
		if i == 4 {
			want = node.CalcFloodTimeout(100)
		}
		if gap < want || gap > want+700*time.Millisecond {
			t.Errorf("attempt %d went %v after the last, want its wait %v", i, gap, want)
		}
	}
}

// A flood ACK to our DM from a node we route to direct means it never learned our route, so it is resent after 3 s (BaseChatMesh::onAckRecv).
func TestFloodACK_ResendsOurRouteAfter3s(t *testing.T) {
	t.Parallel()
	c, radio, self, friend := timedCompanion(t)
	c.node.OnPacket(meshcore.PayloadTypeAck, c.handleAck)
	friendHex := hex.EncodeToString(friend.PublicKeyBytes())
	if err := c.SendContactMessage(friendHex, "hi"); err != nil {
		t.Fatal(err)
	}
	_, pt := decryptDM(t, radio.take()[0], friend, self)
	crc := meshcore.TextAckHash(meshcore.BuildTextPlaintextWithAttempt(time.Unix(int64(pt.Timestamp), 0), 0, []byte("hi"), 0), self.PublicKeyBytes())
	ack := func(route byte, hop byte) {
		radio.rx(&meshcore.Packet{
			Header:     meshcore.MakeHeader(route, meshcore.PayloadTypeAck, 0),
			PathLength: meshcore.MakePathLen(1, 1), Path: []byte{hop},
			Payload: append(binary.LittleEndian.AppendUint32(nil, crc), 0, hop), // firmware's 6-byte ACK: CRC, attempt, random
		})
	}
	radio.mu.Lock()
	radio.delays = nil
	radio.mu.Unlock()
	ack(meshcore.RouteTypeFlood, 0x42)
	ack(meshcore.RouteTypeFlood, 0x43) // the table entry is spent, as processAck clears it

	if got := lastStatus(t, c, "dm:"+friendHex); got != "delivered" {
		t.Fatalf("DM status %q, want delivered", got)
	}
	sent := radio.take()
	radio.mu.Lock()
	delays := radio.delays
	radio.mu.Unlock()
	if len(sent) != 1 || len(delays) != 1 || delays[0] != 3*time.Second {
		t.Fatalf("sent %d packets with delays %v, want one path return after 3s", len(sent), delays)
	}
	pkt, err := meshcore.PacketFromBytes(sent[0])
	if err != nil {
		t.Fatal(err)
	}
	path, err := meshcore.PathFromBytes(pkt.Payload)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := friend.SharedSecret(self.Identity)
	pp, err := meshcore.ParsePathPayload(path.Decrypt(secret))
	if err != nil {
		t.Fatal(err)
	}
	if pkt.PayloadType() != meshcore.PayloadTypePath || !pkt.IsRouteDirect() || len(pkt.Path) != 0 || len(pp.Path) != 1 || pp.Path[0] != 0x42 {
		t.Errorf("sent type %d direct %v along %x, teaching %x; want a direct PATH along our 0-hop route teaching 42", pkt.PayloadType(), pkt.IsRouteDirect(), pkt.Path, pp.Path)
	}
}

// Stopping the node ends a DM's ACK wait unreported, so the DM fails then, once, rather than reading "sending" for good.
func TestStop_FailsADMStillWaiting(t *testing.T) {
	t.Parallel()
	c, _, _, friend := timedCompanion(t)
	friendHex := hex.EncodeToString(friend.PublicKeyBytes())
	var results atomic.Int32
	if _, err := c.sendDM(friendHex, "hi", 2, time.Minute, scopeOf(c.contactScopeHex(friendHex)), func(node.DMSendResult) { results.Add(1) }); err != nil {
		t.Fatal(err)
	}
	c.dmsInFlight.Lock()
	abort := slices.Collect(maps.Values(c.dmsInFlight.abort))
	c.dmsInFlight.Unlock()
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := lastStatus(t, c, "dm:"+friendHex); got != "failed" {
		t.Errorf("DM status %q after stop, want failed", got)
	}
	for _, f := range abort { // as an ACK settling it while Stop fails it would
		f()
	}
	if n := results.Load(); n != 1 {
		t.Errorf("%d results, want 1", n)
	}
}
