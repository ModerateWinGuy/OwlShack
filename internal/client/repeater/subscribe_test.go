package repeater

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/store"
	"github.com/OwlShack/OwlShack/internal/telemetry"
)

// A fake 1.18 sensor that answers as SensorMesh.cpp:213-241 does and pushes as :1042-1068.
type fakeSensor struct {
	t      *testing.T
	self   meshcore.LocalIdentity
	client meshcore.LocalIdentity
	secret []byte
	rm     *Client
}

func (s fakeSensor) respond(plain []byte) {
	s.t.Helper()
	enc, err := meshcore.EncryptThenMAC(s.secret, plain)
	if err != nil {
		s.t.Error(err)
		return
	}
	resp, err := (&meshcore.Response{Destination: s.client.PublicKey()[0], Source: s.self.PublicKey()[0], MAC: [2]byte{enc[0], enc[1]}, EncryptedPayload: enc[2:]}).ToBytes()
	if err != nil {
		s.t.Error(err)
		return
	}
	s.rm.HandleResponsePacket(&meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeResponse, 0), Payload: resp})
}

// read takes the next request off the radio and returns its plaintext, tag first; answer runs off the test goroutine, so it never calls Fatal.
func (s fakeSensor) read(radio captureRadio) []byte {
	pkt, err := meshcore.PacketFromBytes(<-radio.sent)
	if err != nil {
		s.t.Error(err)
		return nil
	}
	plain, err := meshcore.MACThenDecrypt(s.secret, pkt.Payload[2:])
	if err != nil {
		s.t.Error(err)
		return nil
	}
	return plain
}

// answer reads the request off the radio and replies with the request's tag, then body.
func (s fakeSensor) answer(radio captureRadio, body func(req []byte) []byte) []byte {
	plain := s.read(radio)
	if len(plain) < 4 {
		return nil
	}
	s.respond(append(append([]byte(nil), plain[:4]...), body(plain[4:])...))
	return plain[4:]
}

func newSubscribeRig(t *testing.T) (*Client, fakeSensor, captureRadio, string, chan *telemetry.Telemetry) {
	self := meshcore.NewLocalIdentityFromSeed([32]byte{1})
	sensorID := meshcore.NewLocalIdentityFromSeed([32]byte{0x5e})
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	radio := captureRadio{sent: make(chan []byte, 4)}
	n := node.New(self, radio)
	t.Cleanup(n.Stop)
	n.Peers().Insert(&node.Peer{Identity: sensorID.Identity, Name: "Weather"})
	rm := NewClient(n, st, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func() uint8 { return 1 }, func([]byte) *meshcore.Region { return nil })
	pk := hex.EncodeToString(sensorID.PublicKeyBytes())
	secret, err := sensorID.SharedSecret(self.Identity)
	if err != nil {
		t.Fatal(err)
	}
	rm.sessions[pk] = &Session{PubKeyHex: pk, sharedSecret: secret, localPubKey: self.PublicKey()}
	pushes := make(chan *telemetry.Telemetry, 4)
	rm.SetTelemetryPushHandler(func(from string, tl *telemetry.Telemetry) {
		if from == pk {
			pushes <- tl
		}
	})
	return rm, fakeSensor{t: t, self: sensorID, client: self, secret: secret, rm: rm}, radio, pk, pushes
}

// The sensor takes a tag before answering and pushes with the static secret, so a lost reply, a logout and an unanswered stop must each be handled.
func TestSubscribe_LostReplyLogoutAndStop(t *testing.T) {
	t.Parallel()
	rm, sensor, radio, pk, pushes := newSubscribeRig(t)
	deltas := []meshcore.LPPDataValue{{Channel: 2, Type: meshcore.LPPTemperature, Value: 0.5}}
	lpp := []byte{2, meshcore.LPPTemperature, 0x00, 0xD7}
	push := func(tag uint32) bool {
		sensor.respond(append(binary.LittleEndian.AppendUint32(nil, tag), meshcore.BuildTelemetryPush(meshcore.TelemetryPush{LPP: lpp})...))
		select {
		case <-pushes:
			return true
		case <-time.After(200 * time.Millisecond):
			return false
		}
	}

	// A first subscribe whose reply is lost isn't a watch to report, though its tag is listened for.
	go sensor.read(radio)
	if _, err := rm.SendSubscribe(pk, 600, deltas, 300*time.Millisecond); !errors.Is(err, ErrNoReply) {
		t.Fatalf("first subscribe = %v, want no reply", err)
	}
	if sub, ok := rm.ActiveSubscription(pk); ok {
		t.Errorf("an unanswered subscribe is listed as active: %+v", sub)
	}
	if !rm.Watching(pk) {
		t.Error("an unanswered subscribe the sensor may hold is not counted as watching, so nothing would stop it")
	}

	go sensor.answer(radio, func([]byte) []byte {
		return meshcore.BuildSubscribeReply(meshcore.SubscribeReply{ExpirySecs: 600, Scope: "nz"})
	})
	if _, err := rm.SendSubscribe(pk, 600, deltas, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	// The renewal reaches the sensor, which takes its tag, but the reply is lost.
	lost := make(chan uint32, 1)
	go func() {
		if plain := sensor.read(radio); len(plain) >= 4 {
			sub, _ := meshcore.ParseSubscribeRequest(plain[4:])
			lost <- sub.PushTag
		}
	}()
	if _, err := rm.SendSubscribe(pk, 600, deltas, 300*time.Millisecond); !errors.Is(err, ErrNoReply) {
		t.Fatalf("renewal = %v, want no reply", err)
	}
	tag := <-lost
	if sub, ok := rm.ActiveSubscription(pk); !ok || sub.Scope != "nz" {
		t.Errorf("after a lost renewal, active = %+v %v, want the answered watch", sub, ok)
	}
	if !push(tag) {
		t.Error("a push under the unanswered tag was dropped")
	}

	rm.Logout(pk)
	if !push(tag) {
		t.Error("a push after logging out was dropped")
	}

	// Stopping with no session can't reach the sensor, but its pushes stop counting at once.
	if err := rm.SendUnsubscribe(pk, 300*time.Millisecond); err == nil {
		t.Error("unsubscribe with no session succeeded")
	}
	if push(tag) {
		t.Error("a push was taken after stopping")
	}
	if _, ok := rm.ActiveSubscription(pk); ok {
		t.Error("a watch is still listed after stopping")
	}
}

func TestSubscribe_PushesReachTheHandler(t *testing.T) {
	t.Parallel()
	rm, sensor, radio, pk, pushes := newSubscribeRig(t)

	deltas := []meshcore.LPPDataValue{{Channel: 2, Type: meshcore.LPPTemperature, Value: 0.5}}
	var pushTag uint32
	go sensor.answer(radio, func(req []byte) []byte {
		sub, err := meshcore.ParseSubscribeRequest(req)
		if err != nil || sub.TimeoutSecs != 600 || len(sub.MinDeltas) != 1 {
			t.Errorf("subscribe = %+v, %v", sub, err)
		}
		pushTag = sub.PushTag
		return meshcore.BuildSubscribeReply(meshcore.SubscribeReply{ExpirySecs: 600, Scope: "nz"})
	})
	reply, err := rm.SendSubscribe(pk, 600, deltas, 2*time.Second)
	if err != nil || reply.ExpirySecs != 600 || reply.Scope != "nz" {
		t.Fatalf("subscribe = %+v, %v", reply, err)
	}

	lpp := []byte{1, meshcore.LPPVoltage, 0x01, 0x9E, 2, meshcore.LPPTemperature, 0x00, 0xD7} // 4.14 V, 21.5 C
	sensor.respond(append(binary.LittleEndian.AppendUint32(nil, pushTag), meshcore.BuildTelemetryPush(meshcore.TelemetryPush{Timestamp: 1792000000, LPP: lpp})...))
	select {
	case tl := <-pushes:
		if len(tl.Readings) != 2 {
			t.Errorf("push readings = %+v", tl.Readings)
		}
	case <-time.After(time.Second):
		t.Fatal("the push never reached the handler")
	}

	sensor.respond(append(binary.LittleEndian.AppendUint32(nil, pushTag+1), meshcore.BuildTelemetryPush(meshcore.TelemetryPush{LPP: lpp})...))
	select {
	case <-pushes:
		t.Error("a response with a tag nobody subscribed under was taken as a push")
	case <-time.After(100 * time.Millisecond):
	}

	if sub, ok := rm.ActiveSubscription(pk); !ok || sub.Scope != "nz" || time.Until(sub.Expires) < 590*time.Second || rm.SubscribeRefused(pk) {
		t.Errorf("active subscription = %+v, %v, refused %v", sub, ok, rm.SubscribeRefused(pk))
	}

	// The firmware stores a refused request's tag first (SensorMesh.cpp:214), so the running watch pushes under it.
	var refusedTag uint32
	go sensor.answer(radio, func(req []byte) []byte {
		sub, _ := meshcore.ParseSubscribeRequest(req)
		refusedTag = sub.PushTag
		return meshcore.BuildSubscribeReply(meshcore.SubscribeReply{})
	})
	if _, err := rm.SendSubscribe(pk, 600, deltas, 2*time.Second); !errors.Is(err, ErrSubscribeRefused) || !errors.Is(err, ErrRejected) {
		t.Errorf("expiry 0 = %v, want ErrSubscribeRefused", err)
	}
	if !rm.SubscribeRefused(pk) {
		t.Error("the refusal was not kept")
	}
	sensor.respond(append(binary.LittleEndian.AppendUint32(nil, refusedTag), meshcore.BuildTelemetryPush(meshcore.TelemetryPush{LPP: lpp})...))
	select {
	case <-pushes:
	case <-time.After(time.Second):
		t.Error("after a refused resubscribe, the running watch's push under the new tag was dropped")
	}
	pushTag = refusedTag

	go sensor.answer(radio, func([]byte) []byte { return meshcore.BuildUnsubscribeReply([2]byte{7, 9}) })
	if err := rm.SendUnsubscribe(pk, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := rm.ActiveSubscription(pk); ok || rm.SubscribeRefused(pk) {
		t.Error("a subscription or refusal is still listed after unsubscribing")
	}
	sensor.respond(append(binary.LittleEndian.AppendUint32(nil, pushTag), meshcore.BuildTelemetryPush(meshcore.TelemetryPush{LPP: lpp})...))
	select {
	case <-pushes:
		t.Error("a push still arrived after unsubscribing")
	case <-time.After(100 * time.Millisecond):
	}
}
