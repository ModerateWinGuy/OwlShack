package repeater

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/buildinfo"
	"github.com/OwlShack/OwlShack/internal/sensor"
	"github.com/OwlShack/OwlShack/internal/store"
)

// Flood requests are answered in a PATH return laid out as the firmware's createPathReturn (Mesh.cpp:448-486) around its replies (MyMesh.cpp:90-145, 262-360).
func TestFloodReplies_FirmwareLayout(t *testing.T) {
	self := meshcore.NewLocalIdentityFromSeed([32]byte{1})
	admin := meshcore.NewLocalIdentityFromSeed([32]byte{2})
	radio := &recordingRadio{}
	r := telemetryRepeater(t, func() ([]sensor.ChannelEntry, []sensor.Status) { return nil, nil })
	r.cfg.AdminPassword, r.cfg.Name, r.cfg.OwnerInfo = "pw", "Owly", "Wes\nAkl"
	r.node = node.New(self, radio)
	t.Cleanup(r.node.Stop)
	r.routes.m = map[[32]byte]clientRoute{}
	r.acl.m = map[string]*store.RepeaterACLEntry{}
	var nb [32]byte
	nb[0], nb[1], nb[2], nb[3] = 0xA1, 0xA2, 0xA3, 0xA4
	r.neighbors.m = map[[32]byte]*neighbor{nb: {pubkey: nb, snr: -3.5, heard: time.Now().Add(-10500 * time.Millisecond)}}
	secret, err := admin.SharedSecret(self.Identity)
	if err != nil {
		t.Fatal(err)
	}
	pathLen, path := meshcore.MakePathLen(2, 2), []byte{0xAA, 0xBB, 0xCC, 0xDD}

	flood := func(payloadType byte, payload []byte, p []byte, pl byte) *meshcore.Packet {
		return &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, payloadType, 0), PathLength: pl, Path: p, Payload: payload}
	}
	request := func(plain []byte, p []byte, pl byte) []byte {
		t.Helper()
		radio.take()
		payload, err := encPacket(secret, func(mac [2]byte, enc []byte) ([]byte, error) {
			return (&meshcore.Request{Destination: self.PublicKey()[0], Source: admin.PublicKey()[0], MAC: mac, EncryptedPayload: enc}).ToBytes()
		}, plain)
		if err != nil {
			t.Fatal(err)
		}
		r.handleReq(flood(meshcore.PayloadTypeReq, payload, p, pl))
		return returned(t, radio, secret, p, pl)
	}

	plain := append(binary.LittleEndian.AppendUint32(nil, 1000), "pw"...)
	payload, err := encPacket(secret, func(mac [2]byte, enc []byte) ([]byte, error) {
		return (&meshcore.AnonReq{Destination: self.PublicKey()[0], EphemeralPubKey: admin.PublicKey(), MAC: mac, EncryptedPayload: enc}).ToBytes()
	}, plain)
	if err != nil {
		t.Fatal(err)
	}
	r.handleAnonReq(flood(meshcore.PayloadTypeAnonReq, payload, path, pathLen))
	// [server clock:4][OK][keep-alive 0][is admin][permissions][random:4][FIRMWARE_VER_LEVEL]
	if login := returned(t, radio, secret, path, pathLen); len(login) < 13 || !bytes.Equal(login[4:8], []byte{0, 0, 1, meshcore.PermACLAdmin}) || login[12] != firmwareVerLevel {
		t.Errorf("login reply = %x, want [clock][00 00 01 03][random][02]", login)
	}

	tagged := func(tag uint32, body ...byte) []byte {
		return append(binary.LittleEndian.AppendUint32(nil, tag), body...)
	}
	want := tagged(2000, []byte(buildinfo.Version+"\nOwly\nWes\nAkl")...)
	if got := request(tagged(2000, meshcore.ReqTypeGetOwnerInfo, 0, 0, 0, 0, 1, 2, 3, 4), path, pathLen); !bytes.Equal(bytes.TrimRight(got, "\x00"), want) {
		t.Errorf("owner info reply = %q, want %q", got, want)
	}
	pub := admin.PublicKeyBytes()
	want = tagged(2001, append(pub[:6:6], meshcore.PermACLAdmin)...)
	if got := request(tagged(2001, meshcore.ReqTypeGetAccessList, 0, 0, 0, 0, 1, 2, 3, 4), path, pathLen); !bytes.Equal(got[:len(want)], want) || !bytes.Equal(got[len(want):], make([]byte, len(got)-len(want))) {
		t.Errorf("access list reply = %x, want %x", got, want)
	}
	// [total:2][results:2] then [prefix][heard secs ago:4][snr quarter-dB]
	want = tagged(2002, 1, 0, 1, 0, 0xA1, 0xA2, 0xA3, 0xA4, 10, 0, 0, 0, 0xF2)
	if got := request(tagged(2002, meshcore.ReqTypeGetNeighbours, 0, 10, 0, 0, 0, 4, 1, 2, 3, 4), path, pathLen); !bytes.Equal(got[:len(want)], want) {
		t.Errorf("neighbours reply = %x, want %x", got, want)
	}

	// Nine 6-byte entries and a 64-byte path fit one packet but pass MAX_COMBINED_PATH, where createPathReturn gives up (Mesh.cpp:452).
	for i := range 11 {
		var k [32]byte
		k[0] = byte(i + 1)
		r.neighbors.m[k] = &neighbor{pubkey: k, snr: float64(i), heard: time.Now()}
	}
	radio.take()
	long := bytes.Repeat([]byte{0xEE}, 64)
	payload, err = encPacket(secret, func(mac [2]byte, enc []byte) ([]byte, error) {
		return (&meshcore.Request{Destination: self.PublicKey()[0], Source: admin.PublicKey()[0], MAC: mac, EncryptedPayload: enc}).ToBytes()
	}, tagged(2003, meshcore.ReqTypeGetNeighbours, 0, 9, 0, 0, 0, 6, 1, 2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	r.handleReq(flood(meshcore.PayloadTypeReq, payload, long, meshcore.MakePathLen(2, 32)))
	if sent := radio.take(); len(sent) != 0 {
		t.Errorf("sent %d packets for a reply too long to return with a 64-byte path, want none", len(sent))
	}
}

// returned opens the PATH return sent for a flood request, checks it carries the request's path, and gives its RESPONSE plaintext.
func returned(t *testing.T, radio *recordingRadio, secret, path []byte, pathLen byte) []byte {
	t.Helper()
	for _, raw := range radio.take() {
		pkt, err := meshcore.PacketFromBytes(raw)
		if err != nil || pkt.PayloadType() != meshcore.PayloadTypePath {
			continue
		}
		p, err := meshcore.PathFromBytes(pkt.Payload)
		if err != nil {
			t.Fatal(err)
		}
		pp, err := p.DecryptStruct(secret)
		if err != nil {
			t.Fatal(err)
		}
		if pp.PathLength != pathLen || !bytes.Equal(pp.Path, path) || pp.ExtraType != meshcore.PayloadTypeResponse {
			t.Fatalf("path return carries path_len %#x path %x type %d, want %#x %x RESPONSE", pp.PathLength, pp.Path, pp.ExtraType, pathLen, path)
		}
		return pp.Extra
	}
	t.Fatal("no path return sent")
	return nil
}
