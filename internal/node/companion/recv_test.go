package companion

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/gorilla/websocket"

	"github.com/OwlShack/OwlShack/internal/api"
	"github.com/OwlShack/OwlShack/internal/modem"
)

func TestRecentDM_CollapsesRetriesOfOneMessage(t *testing.T) {
	t.Parallel()

	c := &Companion{}
	sender := []byte{0x26, 0x82, 0x5a, 0x3c, 0xe0, 0xed, 0xf2, 0xd1, 0x66}

	if c.recentDM(sender, 1000, "!ping") {
		t.Fatal("the first attempt must not look like a retry")
	}
	// Attempts 1..5 of the same message: same timestamp, only the attempt byte differs.
	for attempt := 1; attempt <= 5; attempt++ {
		if !c.recentDM(sender, 1000, "!ping") {
			t.Errorf("attempt %d was treated as a new message", attempt)
		}
	}
	// A genuinely new message gets its own timestamp from getCurrentTimeUnique.
	if c.recentDM(sender, 1001, "!ping") {
		t.Error("a new timestamp must be a new message, not a retry")
	}
	// Another sender's message that happens to share a timestamp is not a retry of ours.
	other := []byte{0xc0, 0xc5, 0x41, 0x4e, 0xb6, 0x01, 0xa7, 0xe1, 0x0c}
	if c.recentDM(other, 1000, "!ping") {
		t.Error("a different sender must not collide with ours")
	}
}

// An entry older than the window is forgotten, so a person resending the same text much later is
// a real message rather than a silently dropped one.
func TestRecentDM_ForgetsPastTheWindow(t *testing.T) {
	t.Parallel()

	c := &Companion{}
	sender := []byte{0x26, 0x82, 0x5a, 0x3c}

	c.recentDM(sender, 2000, "hi")
	c.dmSeen.Lock()
	for k := range c.dmSeen.at {
		c.dmSeen.at[k] = time.Now().Add(-dmRetryWindow - time.Minute)
	}
	c.dmSeen.Unlock()

	if c.recentDM(sender, 2000, "hi") {
		t.Error("an entry past the retry window must be forgotten")
	}
}

// Two different messages inside one second are NOT retries of each other: a plain DM's timestamp
// is the sending app's clock at second resolution, not getCurrentTimeUnique, so a bot or a fast
// typist can legitimately produce both. Dropping the second would ack it and lose it silently.
func TestRecentDM_DistinctTextsInOneSecondBothSurvive(t *testing.T) {
	t.Parallel()

	c := &Companion{}
	sender := []byte{0x26, 0x82, 0x5a, 0x3c, 0xe0, 0xed}

	if c.recentDM(sender, 5000, "first") {
		t.Fatal("the first message must not look like a retry")
	}
	if c.recentDM(sender, 5000, "second") {
		t.Error("a different text in the same second was dropped as a duplicate")
	}
	// Retries of each still collapse.
	if !c.recentDM(sender, 5000, "first") {
		t.Error("a retry of the first message was treated as new")
	}
	if !c.recentDM(sender, 5000, "second") {
		t.Error("a retry of the second message was treated as new")
	}
}

// fakeStats reports a fixed airtime, standing in for the modem's radio params.
type fakeStats struct {
	airtime uint32
	device  modem.DeviceStats
}

func (f fakeStats) EstAirtimeMs(int) uint32 { return f.airtime }

func (f fakeStats) CachedStats() modem.DeviceStats { return f.device }

func TestDMAckTimeout(t *testing.T) {
	t.Parallel()

	const floor = 5 * time.Second

	// No stats and unknown radio params both fall back to the caller's floor rather than guessing.
	if got := (&Companion{}).dmAckTimeout(20, nil, 1, floor); got != floor {
		t.Errorf("no stats: got %v, want the floor %v", got, floor)
	}
	c0 := &Companion{stats: fakeStats{airtime: 0}}
	if got := c0.dmAckTimeout(20, nil, 1, floor); got != floor {
		t.Errorf("zero airtime: got %v, want the floor %v", got, floor)
	}

	// 460ms is roughly a 100-byte packet at SF7/62.5kHz, the bench preset.
	c := &Companion{stats: fakeStats{airtime: 460}}

	flood := c.dmAckTimeout(20, nil, 1, floor)
	if flood <= floor {
		t.Errorf("flood timeout %v did not exceed the 5s floor, which is the bug this fixes", flood)
	}

	// A direct route waits longer the more hops it has to traverse.
	direct0 := c.dmAckTimeout(20, []byte{}, 1, floor)
	direct2 := c.dmAckTimeout(20, []byte{0xaa, 0xbb}, 1, floor)
	if direct2 <= direct0 {
		t.Errorf("2-hop %v must exceed 0-hop %v", direct2, direct0)
	}

	// A generous operator setting still wins.
	if got := c.dmAckTimeout(20, nil, 1, 60*time.Second); got != 60*time.Second {
		t.Errorf("configured floor ignored: got %v", got)
	}
}

// A room server floods its posts in a region, which the live post shows as a DM or channel message does.
func TestRoomPost_LivePostCarriesItsRegion(t *testing.T) {
	c, _, _, room := scopeCompanion(t)
	srv := api.NewServer(c.store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	for _, action := range []string{"subscribe", "ping"} {
		if err := ws.WriteJSON(map[string]string{"action": action, "topic": "messages"}); err != nil {
			t.Fatal(err)
		}
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pong struct{ Topic string }
	if err := ws.ReadJSON(&pong); err != nil || pong.Topic != "pong" {
		t.Fatalf("subscribing: %+v, %v", pong, err)
	}
	c.hub = srv.Hub()
	c.floodScopeOf = func(*meshcore.Packet) string { return "region:nz" }

	body := meshcore.BuildSignedTextPlaintext(time.Now(), [4]byte{1, 2, 3, 4}, []byte("hello room"), 0)
	post, err := meshcore.ParseTextPlaintext(body)
	if err != nil {
		t.Fatal(err)
	}
	pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTxtMsg, 0)}
	c.handleRoomPush(pkt, room.PublicKeyBytes(), hex.EncodeToString(room.PublicKeyBytes()), make([]byte, 32), body, post)

	var got struct {
		Data map[string]any `json:"data"`
	}
	if err := ws.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.Data["text"] != "hello room" || got.Data["floodScope"] != "region:nz" {
		t.Errorf("live room post = %v, want its region region:nz", got.Data)
	}
}
