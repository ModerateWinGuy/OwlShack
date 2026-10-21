package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OwlShack/OwlShack/internal/monitor"
	"github.com/OwlShack/OwlShack/internal/store"
)

type pushBackend struct {
	Backend
	stopped chan string
}

func (b pushBackend) Repeater(string) (*RepeaterOps, bool) {
	return &RepeaterOps{Unsubscribe: func(pk string) error { b.stopped <- pk; return nil }}, true
}

type pushPoller struct{ polled chan []byte }

func (pushPoller) PollNow(context.Context, []byte) error             { return nil }
func (p pushPoller) PollSoon(pubkey []byte)                          { p.polled <- pubkey }
func (pushPoller) Targets(context.Context) ([]monitor.Target, error) { return nil, nil }

// The form's rules for a sensor push hold at the server, and saving one puts it on air at once.
func TestContactPatch_SensorPush(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := store.Companion{Name: "home"}
	pub, rpt := bytes.Repeat([]byte{0xAB}, 32), bytes.Repeat([]byte{0xCD}, 32)
	st.WriteSync(func() {
		if err = st.Companions.Create(ctx, &c); err == nil {
			err = errors.Join(st.Contacts.Add(ctx, c.ID, pub, "sens", "SENSOR"), st.Contacts.Add(ctx, c.ID, rpt, "rpt", "REPEATER"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	stopped, polled := make(chan string, 4), make(chan []byte, 4)
	s.SetBackend(pushBackend{stopped: stopped})
	s.SetPoller(pushPoller{polled: polled})
	patchKey := func(key []byte, body string) (int, string) {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/companions/home/contacts/"+hex.EncodeToString(key), strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	patch := func(body string) (int, string) { return patchKey(pub, body) }
	wantPoll := func(why string) {
		t.Helper()
		select {
		case pk := <-polled:
			if !bytes.Equal(pk, pub) {
				t.Fatalf("%s: polled %x", why, pk)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the sensor was not polled", why)
		}
	}
	deltas := func(d string) string { return `{"monitorPushDeltas":[` + d + `]}` }

	for _, tc := range []struct{ body, wantErr string }{
		{deltas(`{"channel":2,"type":103}`), "channel, type and value"},
		{deltas(`{"channel":256,"type":103,"value":1}`), "0-255"},
		{deltas(`{"channel":2,"type":104,"value":2}`), "humidity"},
		{deltas(`{"channel":2,"type":136,"value":1}`), "single reading"},
		{deltas(`{"channel":2,"type":103,"value":0}`), "from 0.1 to 3276.7"},
		{deltas(`{"channel":2,"type":103,"value":0.01}`), "from 0.1 to 3276.7"},
		{deltas(`{"channel":2,"type":103,"value":3276.8}`), "from 0.1 to 3276.7"},
		{deltas(`{"channel":1,"type":116,"value":655.36}`), "from 0.01 to 655.35"},
		{deltas(`{"channel":1,"type":120,"value":0.5}`), "from 1 to 255"},
		{deltas(`{"channel":1,"type":120,"value":0.5,"name":"Battery level"}`), "Battery level: the change"},
		{deltas(`{"channel":1,"type":102,"value":1}`), "single reading"},
		{deltas(`{"channel":1,"type":100,"value":16777217}`), "from 1 to 16777216"},
		{deltas(`{"channel":1,"type":100,"value":1},{"channel":2,"type":130,"value":1},{"channel":3,"type":103,"value":1}`), "don't fit"},
		{deltas(`{"channel":1,"type":103,"value":1},{"channel":2,"type":103,"value":1},{"channel":3,"type":103,"value":1},{"channel":4,"type":103,"value":1}`), "at most three"},
		{deltas(`{"channel":1,"type":103,"value":1,"name":"a\u0007"}`), "control character"},
		{`{"monitor":true,"monitorPush":true}`, "at least one reading"},
		{`{"monitorPush":true,"monitorIntervalSecs":86400,` + deltas(`{"channel":2,"type":103,"value":0.5}`)[1:], "lapse"},
	} {
		if code, body := patch(tc.body); code != http.StatusBadRequest || !strings.Contains(body, tc.wantErr) {
			t.Errorf("%s: %d %s, want 400 mentioning %q", tc.body, code, body, tc.wantErr)
		}
	}
	got, err := st.Contacts.Get(ctx, c.ID, pub)
	if err != nil || got.Metadata.MonitorPush || len(got.Metadata.MonitorPushDeltas) != 0 || got.Metadata.Monitor {
		t.Fatalf("a refused PATCH changed the row: %+v %v", got, err)
	}

	on := `{"monitor":true,"monitorPush":true,"monitorIntervalSecs":64800,"monitorPushDeltas":[{"channel":2,"type":103,"value":0.5,"name":"Temperature","unit":"°C"},{"channel":1,"type":116,"value":655.35,"name":"Voltage","unit":"V"}]}`
	if code, body := patch(on); code != http.StatusNoContent {
		t.Fatalf("turning push on: %d %s", code, body)
	}
	wantPoll("turning push on")
	// The UI sends the whole list, so a new value in the same place must still reach the sensor.
	if code, body := patch(`{"monitorPushDeltas":[{"channel":2,"type":103,"value":2},{"channel":1,"type":116,"value":655.35,"name":"Voltage","unit":"V"}]}`); code != http.StatusNoContent {
		t.Fatalf("changing a threshold: %d %s", code, body)
	}
	wantPoll("changing a threshold")
	if got, _ := st.Contacts.Get(ctx, c.ID, pub); got.Metadata.MonitorPushDeltas[0] != (store.PushDelta{Channel: 2, Type: 103, Value: 2}) {
		t.Errorf("stored %+v: the old label leaked into the new reading", got.Metadata.MonitorPushDeltas[0])
	}
	if code, body := patchKey(rpt, on); code != http.StatusBadRequest || !strings.Contains(body, "sensors only") {
		t.Errorf("push on a repeater: %d %s, want 400", code, body)
	}
	// Push can't be left on with its readings gone.
	if code, _ := patch(`{"monitorPushDeltas":[]}`); code != http.StatusBadRequest {
		t.Errorf("clearing the readings with push on got %d, want 400", code)
	}

	if code, body := patch(`{"monitor":false}`); code != http.StatusNoContent {
		t.Fatalf("turning monitoring off: %d %s", code, body)
	}
	select {
	case pk := <-stopped:
		if pk != hex.EncodeToString(pub) {
			t.Fatalf("stopped %s", pk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turning monitoring off did not stop the push")
	}
	// Saving again with nothing to change sends nothing.
	if code, _ := patch(`{"monitor":false}`); code != http.StatusNoContent {
		t.Fatal(code)
	}
	select {
	case <-stopped:
		t.Fatal("an unchanged save stopped the push again")
	case <-polled:
		t.Fatal("an unchanged save polled")
	case <-time.After(200 * time.Millisecond):
	}
}
