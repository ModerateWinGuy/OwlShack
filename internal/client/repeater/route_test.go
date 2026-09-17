package repeater

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/node"

	"github.com/meshcore-go/OwlShack/internal/store"
)

func routeTestClient(t *testing.T) (*Client, int64, []byte) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	comp := &store.Companion{Name: "test"}
	if err := st.Companions.Create(t.Context(), comp); err != nil {
		t.Fatalf("Companions.Create: %v", err)
	}

	pubkey := bytes.Repeat([]byte{0x8d}, 32)
	if err := st.Contacts.Add(t.Context(), comp.ID, pubkey, "JKSparrOwl", "REPEATER"); err != nil {
		t.Fatalf("Contacts.Add: %v", err)
	}

	return &Client{store: st, companionID: comp.ID}, comp.ID, pubkey
}

func pubkeyArray(b []byte) [meshcore.PubKeySize]byte {
	var k [meshcore.PubKeySize]byte
	copy(k[:], b)
	return k
}

// The peer table is emptied of routes on every restart (hydratePeerTables leaves OutPath nil), so a
// route that only lives there is lost and every command floods until an inbound PATH arrives.
func TestLearnedRoute_FallsBackToTheContactRow(t *testing.T) {
	t.Parallel()

	rm, companionID, pubkey := routeTestClient(t)
	key := pubkeyArray(pubkey)

	// Nothing stored anywhere yet: flood is correct.
	if path, _ := rm.learnedRoute(key, nil); path != nil {
		t.Errorf("with no route stored, got %x, want nil (flood)", path)
	}

	if err := rm.store.Contacts.UpdateOutPath(t.Context(), companionID, pubkey, []byte{0xe6, 0x07, 0x1a}, 1); err != nil {
		t.Fatalf("UpdateOutPath: %v", err)
	}

	// A nil peer table entry must not mean "flood" while the row holds a route.
	path, hashSize := rm.learnedRoute(key, nil)
	if !bytes.Equal(path, []byte{0xe6, 0x07, 0x1a}) {
		t.Errorf("path = %x, want e6071a from the contact row", path)
	}
	if hashSize != 1 {
		t.Errorf("hashSize = %d, want 1", hashSize)
	}
	if rt, _ := routeForPeer(path, hashSize); rt != meshcore.RouteTypeDirect {
		t.Errorf("routeType = 0x%02x, want direct", rt)
	}
}

// persistOutPath writes asynchronously, so straight after a PATH the row still holds the old route:
// preferring it would send down a path we already know is superseded.
func TestLearnedRoute_LivePeerTableWinsOverAStaleRow(t *testing.T) {
	t.Parallel()

	rm, companionID, pubkey := routeTestClient(t)
	if err := rm.store.Contacts.UpdateOutPath(t.Context(), companionID, pubkey, []byte{0xaa, 0xbb}, 1); err != nil {
		t.Fatalf("UpdateOutPath: %v", err)
	}

	id, err := meshcore.NewIdentityFromBytes(pubkey)
	if err != nil {
		t.Fatalf("NewIdentityFromBytes: %v", err)
	}
	fresh := &node.Peer{Identity: id, OutPath: []byte{0xe6}, OutPathHashSize: 1}

	if path, _ := rm.learnedRoute(pubkeyArray(pubkey), fresh); !bytes.Equal(path, []byte{0xe6}) {
		t.Errorf("path = %x, want e6 from the live peer table", path)
	}
}

// A direct neighbour is an empty-but-not-nil path. Collapsing the two would flood at the one peer
// that needs no path at all.
func TestLearnedRoute_ZeroHopIsARouteNotAnAbsence(t *testing.T) {
	t.Parallel()

	rm, companionID, pubkey := routeTestClient(t)
	if err := rm.store.Contacts.UpdateOutPath(t.Context(), companionID, pubkey, []byte{}, 1); err != nil {
		t.Fatalf("UpdateOutPath: %v", err)
	}

	path, _ := rm.learnedRoute(pubkeyArray(pubkey), nil)
	if path == nil {
		t.Fatal("a 0-hop route read back as nil, which floods at a direct neighbour")
	}
	if len(path) != 0 {
		t.Errorf("path = %x, want empty", path)
	}
	if rt, pathLen := routeForPeer(path, 1); rt != meshcore.RouteTypeDirect || pathLen != 0 {
		t.Errorf("routeType/pathLen = 0x%02x/%d, want direct/0", rt, pathLen)
	}
}

// The write-through ResetPeerPath relies on: a nil path must clear the row, or learnedRoute reads
// the old route straight back and the Reset Path button does nothing. ResetPeerPath itself needs a
// live node, so this covers the store half only.
func TestPersistOutPath_NilClearsTheRow(t *testing.T) {
	t.Parallel()

	rm, companionID, pubkey := routeTestClient(t)
	if err := rm.store.Contacts.UpdateOutPath(t.Context(), companionID, pubkey, []byte{0xe6, 0x07, 0x1a}, 1); err != nil {
		t.Fatalf("UpdateOutPath: %v", err)
	}

	rm.persistOutPath(pubkey, nil, 0)
	rm.store.WriteSync(func() {}) // drain the async writer

	if path, _ := rm.learnedRoute(pubkeyArray(pubkey), nil); path != nil {
		t.Errorf("after a reset the route came back as %x, want nil (flood to rediscover)", path)
	}
}

// routedPacket exists because the length byte and the hops must come from the same place:
// meshcore-go writes PathLength from the field and the hops from Path, so a mismatch makes the
// receiver read that many bytes of PAYLOAD as path — a send that logs as well-formed and is
// garbage on air. SendRoomKeepAlive shipped exactly that, taking its header from learnedRoute and
// its bytes from the peer table, which diverge after a restart when only the contact row has the
// route. This pins the invariant on the constructor every send now uses.
func TestRoutedPacket_LengthAlwaysDescribesTheBytesItCarries(t *testing.T) {
	t.Parallel()

	rm, companionID, pubkey := routeTestClient(t)
	key := pubkeyArray(pubkey)

	for _, tt := range []struct {
		name  string
		row   []byte
		hs    uint8
		table *node.Peer
	}{
		{"no route anywhere floods", nil, 0, nil},
		{"contact row only, as after a restart", []byte{0xe6, 0x07, 0x1a}, 1, nil},
		{"contact row, two-byte hashes", []byte{0xe6, 0x07, 0x1a, 0x45}, 2, nil},
		{"zero-hop row", []byte{}, 1, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := rm.store.Contacts.UpdateOutPath(t.Context(), companionID, pubkey, tt.row, tt.hs); err != nil {
				t.Fatalf("UpdateOutPath: %v", err)
			}
			pkt, outPath, _ := rm.routedPacket(key, tt.table, meshcore.PayloadTypeReq, []byte{1, 2, 3, 4})

			hs := int((pkt.PathLength>>6)&3) + 1
			if got, want := int(pkt.PathLength&63)*hs, len(pkt.Path); got != want {
				t.Errorf("PathLength describes %d bytes, Path carries %d", got, want)
			}
			if !bytes.Equal(pkt.Path, outPath) {
				t.Errorf("Path %x is not the route that sized it (%x)", pkt.Path, outPath)
			}
		})
	}
}

// A learned route that has gone stale used to fail every login identically and forever: the row
// held a path, so every attempt went direct down it, and nothing in this package ever cleared it.
// Before the contact-row fallback existed a restart floods and masked it; now the route survives
// the restart, so a failed login has to be able to abandon it or the peer is unreachable for good.
func TestLoginTimeout_ClearsTheRouteItFailedOn(t *testing.T) {
	t.Parallel()

	rm, companionID, pubkey := routeTestClient(t)
	key := pubkeyArray(pubkey)

	if err := rm.store.Contacts.UpdateOutPath(t.Context(), companionID, pubkey, []byte{0xbc, 0x8d}, 1); err != nil {
		t.Fatalf("UpdateOutPath: %v", err)
	}
	if path, _ := rm.learnedRoute(key, nil); path == nil {
		t.Fatal("precondition: the stale route should be resolvable before the failure")
	}

	// What the timeout branch does once it has established the send went out on a learned route.
	rm.persistOutPath(pubkey, nil, 0)
	rm.store.WriteSync(func() {}) // drain the async writer

	path, _ := rm.learnedRoute(key, nil)
	if path != nil {
		t.Errorf("route still resolves to %x after a failed login; the retry would repeat it", path)
	}
	if rt, _ := routeForPeer(path, 0); rt != meshcore.RouteTypeFlood {
		t.Errorf("next attempt routes as 0x%02x, want flood so it can rediscover", rt)
	}
}
