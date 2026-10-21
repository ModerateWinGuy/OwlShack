package repeater

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/store"
)

// A companion's CLI needs no login: TXT_TYPE_CLI_COMMAND out, the CLI_DATA reply echoing our tag back (BaseChatMesh.cpp:262).
func TestSendCompanionCLI(t *testing.T) {
	t.Parallel()
	self := meshcore.NewLocalIdentityFromSeed([32]byte{1})
	peer := meshcore.NewLocalIdentityFromSeed([32]byte{0x42})
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	radio := captureRadio{sent: make(chan []byte, 4)}
	n := node.New(self, radio)
	t.Cleanup(n.Stop)
	n.Peers().Insert(&node.Peer{Identity: peer.Identity, Name: "Pal"})
	rm := NewClient(n, st, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func() uint8 { return 1 }, func([]byte) *meshcore.Region { return nil })
	pk := hex.EncodeToString(peer.PublicKeyBytes())
	secret, err := peer.SharedSecret(self.Identity)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		pkt, err := meshcore.PacketFromBytes(<-radio.sent)
		if err != nil {
			return
		}
		plain, err := meshcore.MACThenDecrypt(secret, pkt.Payload[2:])
		if err != nil || plain[4]>>2 != meshcore.TxtTypeCLICommand {
			return // the test times out on a wrong text type
		}
		text := strings.TrimRight(string(plain[5:]), "\x00")
		rm.HandleCLIResponse([32]byte{}, text[:3]+"> v1.18.0 got "+text[3:])
	}()
	if got, err := rm.SendCompanionCLI(pk, "ver", 2*time.Second); err != nil || got != "> v1.18.0 got ver" {
		t.Fatalf("reply = %q, %v; want the tagged reply to ver", got, err)
	}

	if _, err := rm.SendCompanionCLI(pk, "ver", time.Millisecond); !errors.Is(err, ErrCompanionCLINoReply) || !errors.Is(err, ErrNoReply) {
		t.Errorf("silence = %v, want ErrCompanionCLINoReply wrapping ErrNoReply", err)
	}
}
