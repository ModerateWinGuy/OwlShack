package advert

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
)

type stubRadio struct{}

func (stubRadio) SendData([]byte) error                               { return nil }
func (stubRadio) SetDataHandler(func(*meshcore.Packet))               {}
func (stubRadio) SetRawDataHandler(func([]byte, float32, int8, bool)) {}
func (stubRadio) AddOutboundHandler(func([]byte))                     {}
func (stubRadio) Close() error                                        { return nil }
func (stubRadio) Enqueue([]byte, uint8, time.Duration) bool           { return true }
func (stubRadio) TxQueueLen() int                                     { return 0 }

// A self-advert is laid out as the firmware's createAdvert (Mesh.cpp:404-440): key, plain clock, signature over both and the app data, then [flags][lat][lon][name].
func TestBuildSelf_FirmwareLayout(t *testing.T) {
	seed := [32]byte{9}
	n := node.New(meshcore.NewLocalIdentityFromSeed(seed), stubRadio{})
	t.Cleanup(n.Stop)
	lat, lon := -36.848461, 174.763336
	before := uint32(time.Now().Unix())

	pkt, err := BuildSelf(n, "CHAT", "Owly", &lat, &lon, true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !pkt.IsRouteFlood() || pkt.PathLength != 0x40 || pkt.PayloadType() != meshcore.PayloadTypeAdvert {
		t.Fatalf("flood advert route %d path_len %#x type %d, want flood 0x40 ADVERT", pkt.RouteType(), pkt.PathLength, pkt.PayloadType())
	}
	ts := binary.LittleEndian.Uint32(pkt.Payload[32:36])
	if ts < before || ts > uint32(time.Now().Unix()) {
		t.Fatalf("timestamp %d is not the clock now", ts)
	}
	app := []byte{0x01 | 0x10 | 0x80, 0xB3, 0xBC, 0xCD, 0xFD, 0x48, 0xAD, 0x6A, 0x0A}
	app = append(app, "Owly"...)
	pub := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
	signed := append(binary.LittleEndian.AppendUint32(bytes.Clone(pub), ts), app...)
	want := append(append(binary.LittleEndian.AppendUint32(bytes.Clone(pub), ts), ed25519.Sign(ed25519.NewKeyFromSeed(seed[:]), signed)...), app...)
	if !bytes.Equal(pkt.Payload, want) {
		t.Errorf("advert = %x\nwant     %x", pkt.Payload, want)
	}

	pkt, err = BuildSelf(n, "REPEATER", "Owly", nil, nil, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	adv, err := meshcore.AdvertFromBytes(pkt.Payload)
	if err != nil || !adv.Verify() {
		t.Fatalf("zero-hop advert does not verify: %v", err)
	}
	if !pkt.IsRouteDirect() || pkt.PathLength != 0 || !bytes.Equal(adv.RawAppData, append([]byte{0x02 | 0x80}, "Owly"...)) {
		t.Errorf("zero-hop advert route %d path_len %#x app %x, want direct, 0, 82 Owly", pkt.RouteType(), pkt.PathLength, adv.RawAppData)
	}
}
