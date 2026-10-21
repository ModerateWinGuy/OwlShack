package app

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"

	repeaterclient "github.com/OwlShack/OwlShack/internal/client/repeater"
	"github.com/OwlShack/OwlShack/internal/monitor"
	"github.com/OwlShack/OwlShack/internal/store"
	"github.com/OwlShack/OwlShack/internal/telemetry"
)

// pushingSensor is a 1.18 sensor we're logged into: it answers telemetry, and subscribe as subErr says.
type pushingSensor struct {
	offAirNode
	telemetryErr error
	subErr       error
	subs         []uint16
	deltas       []meshcore.LPPDataValue
}

func (n *pushingSensor) SendTelemetryReq(string, time.Duration) (*telemetry.Telemetry, error) {
	if n.telemetryErr != nil {
		return nil, n.telemetryErr
	}
	return telemetry.Parse([]byte{2, meshcore.LPPTemperature, 0x00, 0xD7})
}

func (n *pushingSensor) SendSubscribe(_ string, secs uint16, deltas []meshcore.LPPDataValue, _ time.Duration) (meshcore.SubscribeReply, error) {
	n.subs = append(n.subs, secs)
	n.deltas = deltas
	return meshcore.SubscribeReply{ExpirySecs: secs}, n.subErr
}

func newSensorCollector(t *testing.T) *sensorCollector {
	return &sensorCollector{admin: &repeaterCollector{db: newHealthBackend(t).db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

var watchTemp = []store.PushDelta{{Channel: 2, Type: int(meshcore.LPPTemperature), Value: 0.5, Name: "Temperature"}}

// Each poll of a pushing sensor reads its telemetry, then renews the watch to outlast the next poll.
func TestSensorCollect_RenewsTheWatch(t *testing.T) {
	t.Parallel()
	sc := newSensorCollector(t)
	node := &pushingSensor{offAirNode: offAirNode{session: &repeaterclient.Session{IsAdmin: true}}}
	res, err := sc.collect(t.Context(), node, monitor.Target{Pubkey: make([]byte, 32), IntervalSecs: 3600}, 1, "sens", func() []store.PushDelta { return watchTemp })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readings) != 1 || res.Readings[0].Value != 21.5 || res.RetryFailure {
		t.Errorf("result %+v, want the temperature and no retry", res)
	}
	want := []meshcore.LPPDataValue{{Channel: 2, Type: meshcore.LPPTemperature, Value: 0.5}}
	if len(node.subs) != 1 || node.subs[0] != 4200 || fmt.Sprint(node.deltas) != fmt.Sprint(want) {
		t.Errorf("subscribed %v with %+v, want once for 4200 s with %+v", node.subs, node.deltas, want)
	}
}

// A refusal is the sensor's set-up, not a failed poll: the readings stand and nothing retries early.
func TestSensorCollect_RefusedKeepsThePoll(t *testing.T) {
	t.Parallel()
	sc := newSensorCollector(t)
	node := &pushingSensor{offAirNode: offAirNode{session: &repeaterclient.Session{IsAdmin: true}}, subErr: fmt.Errorf("%w: %w", repeaterclient.ErrRejected, repeaterclient.ErrSubscribeRefused)}
	res, err := sc.collect(t.Context(), node, monitor.Target{Pubkey: make([]byte, 32)}, 1, "sens", func() []store.PushDelta { return watchTemp })
	if err != nil || len(res.Readings) != 1 || res.RetryFailure {
		t.Errorf("res %+v err %v, want the readings and no retry", res, err)
	}
}

// An unanswered subscribe leaves the watch to lapse, so the poll retries soon.
func TestSensorCollect_UnansweredSubscribeRetries(t *testing.T) {
	t.Parallel()
	sc := newSensorCollector(t)
	node := &pushingSensor{offAirNode: offAirNode{session: &repeaterclient.Session{IsAdmin: true}}, subErr: repeaterclient.ErrNoReply}
	res, err := sc.collect(t.Context(), node, monitor.Target{Pubkey: make([]byte, 32)}, 1, "sens", func() []store.PushDelta { return watchTemp })
	if err != nil || len(res.Readings) != 1 || !res.RetryFailure {
		t.Errorf("res %+v err %v, want the readings and a retry", res, err)
	}
}

// A request the encoder or sensor turns down would only be turned down again, so it costs no early retry.
func TestSensorCollect_RejectedSubscribeDoesNotRetry(t *testing.T) {
	t.Parallel()
	sc := newSensorCollector(t)
	node := &pushingSensor{offAirNode: offAirNode{session: &repeaterclient.Session{IsAdmin: true}}, subErr: fmt.Errorf("%w: too big", repeaterclient.ErrRejected)}
	res, err := sc.collect(t.Context(), node, monitor.Target{Pubkey: make([]byte, 32)}, 1, "sens", func() []store.PushDelta { return watchTemp })
	if err != nil || res.RetryFailure {
		t.Errorf("res %+v err %v, want no retry", res, err)
	}
}

// Push turned off while the poll was reading telemetry starts no watch after the stop.
func TestSensorCollect_PushOffMidPoll(t *testing.T) {
	t.Parallel()
	sc := newSensorCollector(t)
	node := &pushingSensor{offAirNode: offAirNode{session: &repeaterclient.Session{IsAdmin: true}}}
	if _, err := sc.collect(t.Context(), node, monitor.Target{Pubkey: make([]byte, 32)}, 1, "sens", func() []store.PushDelta { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(node.subs) != 0 {
		t.Errorf("subscribed %v after push was turned off", node.subs)
	}
}

// A sensor that doesn't answer its telemetry gets no subscribe either.
func TestSensorCollect_QuietSensorIsNotSubscribed(t *testing.T) {
	t.Parallel()
	sc := newSensorCollector(t)
	node := &pushingSensor{offAirNode: offAirNode{session: &repeaterclient.Session{IsAdmin: true}}, telemetryErr: repeaterclient.ErrNoReply}
	if _, err := sc.collect(t.Context(), node, monitor.Target{Pubkey: make([]byte, 32)}, 1, "sens", func() []store.PushDelta { return watchTemp }); !errors.Is(err, repeaterclient.ErrNoReply) {
		t.Errorf("err %v, want no reply", err)
	}
	if len(node.subs) != 0 {
		t.Errorf("subscribed %v, want nothing", node.subs)
	}
}

func TestMonitorInterval(t *testing.T) {
	t.Parallel()
	push := store.ContactMetadata{MonitorPush: true}
	for _, tc := range []struct {
		name    string
		kind    string
		m       store.ContactMetadata
		refused bool
		want    int64
	}{
		{"pushing gets the short default", "sensor", push, false, pushIntervalSecs},
		{"refused falls back to the normal default", "sensor", push, true, 0},
		{"the operator's interval wins", "sensor", store.ContactMetadata{MonitorPush: true, MonitorIntervalSecs: 900}, false, 900},
		{"push off", "sensor", store.ContactMetadata{}, false, 0},
		{"not a sensor", "repeater", push, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := monitorInterval(tc.kind, tc.m, tc.refused); got != tc.want {
				t.Errorf("interval %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPushWatchSecs(t *testing.T) {
	t.Parallel()
	for in, want := range map[int64]uint16{0: uint16(monitor.DefaultIntervalSecs + 600), 3600: 4200, 64800: 65400, 86400: 65535} {
		if got := pushWatchSecs(in); got != want {
			t.Errorf("pushWatchSecs(%d) = %d, want %d", in, got, want)
		}
	}
}

// A push is kept only while its contact still asks for pushes.
func TestPushWanted(t *testing.T) {
	t.Parallel()
	db := newHealthBackend(t).db
	ctx := t.Context()
	c := store.Companion{Name: "home"}
	pk := make([]byte, 32)
	pk[0] = 0x5e
	var err error
	db.WriteSync(func() {
		if err = db.Companions.Create(ctx, &c); err == nil {
			err = db.Contacts.Add(ctx, c.ID, pk, "sens", "SENSOR")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	set := func(m store.ContactMetadata) {
		db.WriteSync(func() { err = db.Contacts.UpdateMetadata(ctx, c.ID, pk, m) })
		if err != nil {
			t.Fatal(err)
		}
	}
	set(store.ContactMetadata{Monitor: true, MonitorPush: true, MonitorPushDeltas: watchTemp})
	if !pushWanted(ctx, db, c.ID, pk) {
		t.Error("a pushing sensor's push was turned away")
	}
	set(store.ContactMetadata{Monitor: false, MonitorPush: true, MonitorPushDeltas: watchTemp})
	if pushWanted(ctx, db, c.ID, pk) {
		t.Error("a push was kept with monitoring off")
	}
	db.WriteSync(func() { err = db.Contacts.Delete(ctx, c.ID, pk) })
	if err != nil || pushWanted(ctx, db, c.ID, pk) {
		t.Errorf("a push was kept for a deleted contact (%v)", err)
	}
}

func TestTakesTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                       string
		taken, takenPushes, pushes bool
		want                       bool
	}{
		{"first companion", false, false, false, true},
		{"a later plain one stays out", true, false, false, false},
		{"a later pushing one takes over, as only it subscribes", true, false, true, true},
		{"the first pushing one keeps it", true, true, true, false},
	} {
		if got := takesTarget(tc.taken, tc.takenPushes, tc.pushes); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
