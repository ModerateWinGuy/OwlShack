package repeater

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/buildinfo"
	"github.com/OwlShack/OwlShack/internal/config"
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

// anonRepeater answers anon sub-requests from client over a recording radio.
func anonRepeater(t *testing.T, regions ...config.RepeaterRegion) (*Repeater, *recordingRadio, meshcore.LocalIdentity) {
	t.Helper()
	radio := &recordingRadio{}
	r := telemetryRepeater(t, nil)
	r.cfg.Name, r.cfg.OwnerInfo, r.cfg.Regions = "Owly", "Wes\nAkl", regions
	r.node = node.New(meshcore.NewLocalIdentityFromSeed([32]byte{1}), radio)
	t.Cleanup(r.node.Stop)
	r.anonLimiter = newRateLimiter(100, time.Minute)
	return r, radio, meshcore.NewLocalIdentityFromSeed([32]byte{2})
}

// anonReply sends a direct anon sub-request asking for a reply along path, and gives the reply after its tag and clock.
func anonReply(t *testing.T, r *Repeater, radio *recordingRadio, client meshcore.LocalIdentity, subType byte, pathLen byte, path []byte) []byte {
	t.Helper()
	secret, err := client.SharedSecret(r.node.Identity().Identity)
	if err != nil {
		t.Fatal(err)
	}
	plain := append(binary.LittleEndian.AppendUint32(nil, 4242), subType, pathLen)
	payload, err := encPacket(secret, func(mac [2]byte, enc []byte) ([]byte, error) {
		return (&meshcore.AnonReq{Destination: r.node.Identity().PublicKey()[0], EphemeralPubKey: client.PublicKey(), MAC: mac, EncryptedPayload: enc}).ToBytes()
	}, append(plain, path...))
	if err != nil {
		t.Fatal(err)
	}
	radio.take()
	before := uint32(time.Now().Unix())
	r.handleAnonReq(&meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeAnonReq, 0), Payload: payload})
	sent := radio.take()
	if len(sent) != 1 {
		t.Fatalf("sent %d packets for anon request %d, want 1", len(sent), subType)
	}
	pkt, err := meshcore.PacketFromBytes(sent[0])
	if err != nil {
		t.Fatal(err)
	}
	if !pkt.IsRouteDirect() || pkt.PathLength != pathLen || !bytes.Equal(pkt.Path, path) || pkt.PayloadType() != meshcore.PayloadTypeResponse {
		t.Fatalf("reply route %d path_len %#x path %x type %d, want direct %#x %x RESPONSE", pkt.RouteType(), pkt.PathLength, pkt.Path, pkt.PayloadType(), pathLen, path)
	}
	resp, err := meshcore.ResponseFromBytes(pkt.Payload)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.TrimRight(resp.Decrypt(secret), "\x00")
	if len(body) < 8 || binary.LittleEndian.Uint32(body) != 4242 || binary.LittleEndian.Uint32(body[4:]) < before {
		t.Fatalf("reply %x, want [4242][our clock][body]", body)
	}
	return body[8:]
}

// The anon REGIONS, OWNER and BASIC replies are laid out as simple_repeater's handleAnon*Req (MyMesh.cpp:146-205) and go back along the path the request carried.
func TestAnonSubRequests_FirmwareLayout(t *testing.T) {
	r, radio, client := anonRepeater(t, config.RepeaterRegion{Name: "#alpha"}, config.RepeaterRegion{Name: "bravo", DenyFlood: true}, config.RepeaterRegion{Name: "*"})
	path := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	if got := anonReply(t, r, radio, client, meshcore.AnonReqTypeRegions, meshcore.MakePathLen(3, 2), path); string(got) != "*,alpha" {
		t.Errorf("regions = %q, want *,alpha", got)
	}
	if got := anonReply(t, r, radio, client, meshcore.AnonReqTypeOwner, meshcore.MakePathLen(1, 1), path[:1]); string(got) != "Owly\nWes\nAkl" {
		t.Errorf("owner = %q, want Owly\\nWes\\nAkl", got)
	}
	off := true
	r.cfg.DisableFwd = &off
	if got := anonReply(t, r, radio, client, meshcore.AnonReqTypeBasic, 0, nil); !bytes.Equal(got, []byte{0x80}) {
		t.Errorf("basic with forwarding off = %x, want 80", got)
	}
}

// A discover request and our answers are laid out as simple_repeater's (MyMesh.cpp:806-813, 846-859): 0x90|type, SNR quarter-dB, the tag, then the key or its 8-byte prefix.
func TestDiscover_FirmwareLayout(t *testing.T) {
	r, radio, _ := anonRepeater(t)
	r.discoverLimiter = newRateLimiter(100, time.Minute)
	if err := r.sendDiscover(); err != nil {
		t.Fatal(err)
	}
	sent := radio.take()
	if len(sent) != 1 {
		t.Fatalf("sent %d packets, want 1", len(sent))
	}
	// Zero-hop: a direct header and an empty path, then [0x80][filter][tag:4][since:4].
	want := append([]byte{meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeControl, 0), 0, 0x80, 1 << meshcore.AdvertTypeRepeater}, binary.LittleEndian.AppendUint32(nil, r.discover.tag)...)
	if want = append(want, 0, 0, 0, 0); !bytes.Equal(sent[0], want) {
		t.Errorf("request = %x, want %x", sent[0], want)
	}

	pub := r.node.Identity().PublicKeyBytes()
	for _, prefixOnly := range []bool{false, true} {
		flags := byte(0x80)
		if prefixOnly {
			flags |= 1
		}
		r.handleControl(&meshcore.Packet{
			Header:  meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeControl, 0),
			Payload: []byte{flags, 1 << meshcore.AdvertTypeRepeater, 0x78, 0x56, 0x34, 0x12, 0, 0, 0, 0},
			SNR:     -6.25, HasSignalInfo: true,
		})
		sent = radio.take()
		if len(sent) != 1 {
			t.Fatalf("prefixOnly %v: sent %d answers, want 1", prefixOnly, len(sent))
		}
		key := pub[:]
		if prefixOnly {
			key = pub[:8]
		}
		want = append([]byte{0x90 | meshcore.AdvertTypeRepeater, 0xE7, 0x78, 0x56, 0x34, 0x12}, key...)
		if got := sent[0][2:]; !bytes.Equal(got, want) {
			t.Errorf("prefixOnly %v: answer = %x (%d bytes), want %x", prefixOnly, got, len(got), want)
		}
	}
}

// A repeater with both advert intervals off is hidden and ignores discover requests (simple_repeater MyMesh.cpp:793).
func TestDiscover_HiddenNodeStaysSilent(t *testing.T) {
	zero, two := 0, 2*60*60
	for _, tc := range []struct {
		name         string
		local, flood *int
		wantAnswered bool
	}{
		{"defaults", nil, nil, true},
		{"flood advert only", &zero, &two, true},
		{"both off", &zero, &zero, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, radio, _ := anonRepeater(t)
			r.discoverLimiter = newRateLimiter(100, time.Minute)
			r.cfg.AdvertInterval, r.cfg.FloodAdvertInterval = tc.local, tc.flood
			r.handleControl(&meshcore.Packet{
				Header:  meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeControl, 0),
				Payload: []byte{0x80, 1 << meshcore.AdvertTypeRepeater, 1, 2, 3, 4, 0, 0, 0, 0},
			})
			if got := len(radio.take()) == 1; got != tc.wantAnswered {
				t.Errorf("answered = %v, want %v", got, tc.wantAnswered)
			}
		})
	}
}
