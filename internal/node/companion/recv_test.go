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
	"github.com/OwlShack/OwlShack/internal/telemetry"
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

// fakeStats stands in for the modem's last poll.
type fakeStats struct {
	device modem.DeviceStats
}

func (f fakeStats) CachedStats() modem.DeviceStats { return f.device }

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

// A sensor's push reaches the page as {pubkey, readings} on "telemetry", the shape SensorWatch reads.
func TestTelemetryPush_Broadcast(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	srv := api.NewServer(c.store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	for _, action := range []string{"subscribe", "ping"} {
		if err := ws.WriteJSON(map[string]string{"action": action, "topic": "telemetry"}); err != nil {
			t.Fatal(err)
		}
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pong struct{ Topic string }
	if err := ws.ReadJSON(&pong); err != nil || pong.Topic != "pong" {
		t.Fatalf("subscribing: %+v, %v", pong, err)
	}
	c.hub = srv.Hub()

	tl, err := telemetry.Parse([]byte{2, meshcore.LPPTemperature, 0x00, 0xD7})
	if err != nil {
		t.Fatal(err)
	}
	c.broadcastTelemetryPush("ab12", tl)
	var got struct {
		Topic string
		Data  struct {
			Pubkey   string
			Readings []struct {
				Channel int
				Type    int
				Value   float64
			}
		}
	}
	if err := ws.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.Topic != "telemetry" || got.Data.Pubkey != "ab12" || len(got.Data.Readings) != 1 || got.Data.Readings[0].Value != 21.5 || got.Data.Readings[0].Channel != 2 {
		t.Errorf("push broadcast = %+v", got)
	}
}
