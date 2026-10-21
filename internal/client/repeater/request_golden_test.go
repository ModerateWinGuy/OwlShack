package repeater

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
)

// sentPlaintext decrypts the next packet the rig sends, a REQ or ANON_REQ, under secret.
func (r *appRig) sentPlaintext(t *testing.T, secret []byte) []byte {
	t.Helper()
	pkt := r.next(t)
	var plain []byte
	switch pkt.PayloadType() {
	case meshcore.PayloadTypeReq:
		req, err := meshcore.RequestFromBytes(pkt.Payload)
		if err != nil {
			t.Fatal(err)
		}
		plain = req.Decrypt(secret)
	case meshcore.PayloadTypeAnonReq:
		req, err := meshcore.AnonReqFromBytes(pkt.Payload)
		if err != nil {
			t.Fatal(err)
		}
		plain = req.Decrypt(secret)
	}
	if plain == nil {
		t.Fatalf("payload type %d did not decrypt", pkt.PayloadType())
	}
	return plain
}

// Request bodies as the firmware lays them out (BaseChatMesh.cpp:690-698, MyMesh.cpp:276-282, SensorMesh.cpp:174-179); the tail of every padded block is zero.
func TestRequests_FirmwareLayout(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)
	r.rm.sessions[r.contactH] = &Session{sharedSecret: r.secretC, localPubKey: r.rm.node.Identity().PublicKey()}
	app := AppSend{Sent: func(uint32, time.Duration, bool) {}}

	for _, tc := range []struct {
		name  string
		send  func()
		fixed []byte // the body before its 4 random bytes, or all of it when random is false
		rand  bool
	}{
		{"status", func() { r.rm.SendStatusReq(r.contactH, time.Millisecond) }, []byte{meshcore.ReqTypeGetStatus, 0, 0, 0, 0}, true},
		{"room status", func() { r.rm.SendRoomStatusReq(r.contactH, time.Millisecond) }, []byte{meshcore.ReqTypeGetStatus, 0, 0, 0, 0}, true},
		{"owner", func() { r.rm.SendOwnerInfoReq(r.contactH, time.Millisecond) }, []byte{meshcore.ReqTypeGetOwnerInfo, 0, 0, 0, 0}, true},
		{"access list", func() { r.rm.SendAccessListReq(r.contactH, time.Millisecond) }, []byte{meshcore.ReqTypeGetAccessList, 0, 0, 0, 0}, true},
		{"telemetry", func() { r.rm.SendTelemetryReq(r.contactH, time.Millisecond) }, []byte{meshcore.ReqTypeGetTelemetryData, 0, 0, 0, 0}, true},
		{"contact telemetry", func() { r.rm.SendContactTelemetryReq(r.contactH, time.Millisecond) }, []byte{meshcore.ReqTypeGetTelemetryData, 0, 0, 0, 0}, true},
		{"path discovery", func() { r.rm.AppPathDiscovery(r.contactH, time.Millisecond, app) }, []byte{meshcore.ReqTypeGetTelemetryData, 0xFE, 0, 0, 0}, true},
		{"neighbours", func() { r.rm.SendNeighborsReq(r.contactH, 20, 0x0102, time.Millisecond) }, []byte{meshcore.ReqTypeGetNeighbours, 0, 20, 0x02, 0x01, 2, 6}, true},
		{"series", func() { r.rm.SendSeriesReq(r.contactH, 0x01020304, 0x05060708, time.Millisecond) }, []byte{meshcore.ReqTypeGetAvgMinMax, 4, 3, 2, 1, 8, 7, 6, 5, 0, 0}, false},
	} {
		var tails [][]byte
		for range 2 {
			tc.send()
			body := r.sentPlaintext(t, r.secretC)[4:]
			n := len(tc.fixed)
			if tc.rand {
				n += 4
			}
			if !bytes.Equal(body[:len(tc.fixed)], tc.fixed) || !bytes.Equal(body[n:], make([]byte, len(body)-n)) {
				t.Errorf("%s body = %x, want %x then %d bytes", tc.name, body, tc.fixed, n-len(tc.fixed))
			}
			tails = append(tails, body[len(tc.fixed):n])
		}
		if tc.rand && bytes.Equal(tails[0], tails[1]) {
			t.Errorf("%s: two requests carried the same random tail %x", tc.name, tails[0])
		}
	}
}

// The firmware's sendLogin keeps 15 bytes of a password (BaseChatMesh.cpp:612,616); a room's sync_since comes first.
func TestLogin_FirmwareLayout(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)

	r.rm.SendLogin(r.contactH, "0123456789abcdefXYZ", time.Millisecond)
	if body := bytes.TrimRight(r.sentPlaintext(t, r.secretC)[4:], "\x00"); string(body) != "0123456789abcde" {
		t.Errorf("repeater login body = %q, want the first 15 bytes", body)
	}
	r.rm.SendRoomLogin(r.contactH, "pw", 0x11223344, time.Millisecond)
	body := r.sentPlaintext(t, r.secretC)[4:]
	if binary.LittleEndian.Uint32(body) != 0x11223344 || string(bytes.TrimRight(body[4:], "\x00")) != "pw" {
		t.Errorf("room login body = %x, want sync_since 11223344 then pw", body)
	}
}

// A password starting with a control byte is the repeater's anon sub-request marker (MyMesh.cpp:582), so it is refused rather than sent.
func TestLogin_ControlBytePasswordRefused(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)
	if _, err := r.rm.SendLogin(r.contactH, "\x01pw", time.Millisecond); err == nil || !bytes.Contains([]byte(err.Error()), []byte("control byte")) {
		t.Errorf("err = %v, want a control-byte refusal", err)
	}
	select {
	case <-r.radio.sent:
		t.Error("a refused login was still sent")
	default:
	}
}
