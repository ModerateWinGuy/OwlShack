package repeater

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/OwlShack/OwlShack/internal/sensor"
	"github.com/OwlShack/OwlShack/internal/store"
	"github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
)

// The firmware runs a CLI line sent as plain, CLI data or CLI command (simple_repeater onPeerDataRecv), and nothing else.
func TestHandleCLI_AnswersTheFirmwaresTextTypes(t *testing.T) {
	self := meshcore.NewLocalIdentityFromSeed([32]byte{1})
	admin := meshcore.NewLocalIdentityFromSeed([32]byte{2})
	radio := &recordingRadio{}
	r := telemetryRepeater(t, func() ([]sensor.ChannelEntry, []sensor.Status) { return nil, nil })
	r.node = node.New(self, radio)
	t.Cleanup(r.node.Stop)
	r.routes.m = map[[32]byte]clientRoute{}
	key := hex.EncodeToString(admin.PublicKeyBytes())
	r.acl.m = map[string]*store.RepeaterACLEntry{key: {PubKey: key, Permissions: permAdmin}}
	secret, err := admin.SharedSecret(self.Identity)
	if err != nil {
		t.Fatal(err)
	}

	answered := func(txtType byte, ts uint32) bool {
		t.Helper()
		plain := binary.LittleEndian.AppendUint32(nil, ts)
		plain = append(append(plain, meshcore.TextFlags(txtType)), "ver"...)
		payload, err := encPacket(secret, func(mac [2]byte, enc []byte) ([]byte, error) {
			return (&meshcore.TextMessage{Destination: self.PublicKey()[0], Source: admin.PublicKey()[0], MAC: mac, EncryptedPayload: enc}).ToBytes()
		}, plain)
		if err != nil {
			t.Fatal(err)
		}
		radio.take()
		r.handleCLI(&meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTxtMsg, 0), Payload: payload})
		for _, raw := range radio.take() {
			if pkt, err := meshcore.PacketFromBytes(raw); err == nil && pkt.PayloadType() == meshcore.PayloadTypeTxtMsg {
				return true
			}
		}
		return false
	}

	for i, tc := range []struct {
		txtType byte
		want    bool
	}{
		{meshcore.TxtTypePlain, true},
		{meshcore.TxtTypeCLIData, true},
		{meshcore.TxtTypeCLICommand, true},
		{meshcore.TxtTypeSignedPlain, false},
	} {
		if got := answered(tc.txtType, uint32(1000+i)); got != tc.want {
			t.Errorf("text type %d answered = %v, want %v", tc.txtType, got, tc.want)
		}
	}
}
