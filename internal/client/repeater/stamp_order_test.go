package repeater

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/store"
)

// Firmware drops a CLI or request whose timestamp is below the last it saw, so concurrent sends must reach the radio in stamp order.
func TestConcurrentSends_ReachTheRadioInStampOrder(t *testing.T) {
	t.Parallel()

	self := meshcore.NewLocalIdentityFromSeed([32]byte{1})
	repeater := meshcore.NewLocalIdentityFromSeed([32]byte{0x8d})
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const sends = 64
	radio := captureRadio{sent: make(chan []byte, 2*sends)}
	n := node.New(self, radio)
	t.Cleanup(n.Stop)
	n.Peers().Insert(&node.Peer{Identity: repeater.Identity, Name: "Owly"})
	rm := NewClient(n, st, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func() uint8 { return 1 }, func([]byte) *meshcore.Region { return nil })
	pk := hex.EncodeToString(repeater.PublicKeyBytes())
	secret, err := repeater.SharedSecret(self.Identity)
	if err != nil {
		t.Fatal(err)
	}
	rm.sessions[pk] = &Session{PubKeyHex: pk, IsAdmin: true, sharedSecret: secret, localPubKey: self.PublicKey()}

	var wg sync.WaitGroup
	for i := range sends {
		wg.Go(func() {
			if i%2 == 0 {
				_, _ = rm.SendCLI(pk, "ver", time.Millisecond)
			} else {
				_, _ = rm.SendContactTelemetryReq(pk, time.Millisecond)
			}
		})
	}
	wg.Wait()

	var last uint32
	for range sends {
		pkt, err := meshcore.PacketFromBytes(<-radio.sent)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := meshcore.MACThenDecrypt(secret, pkt.Payload[2:])
		if err != nil {
			t.Fatal(err)
		}
		ts := binary.LittleEndian.Uint32(plain[:4])
		if ts <= last {
			t.Fatalf("timestamp %d reached the radio after %d: the node would drop it", ts, last)
		}
		last = ts
	}
}
