package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-go/OwlShack/internal/api"
	"github.com/meshcore-go/OwlShack/internal/modem"
	"github.com/meshcore-go/OwlShack/internal/store"
)

func newHealthBackend(t *testing.T) *backend {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Nil stats and no nodes is the shape of a process whose radio never came up — the case the
	// endpoint exists to make visible.
	return newBackend(nil, nil, db, nil, nil, nil, nil, nil)
}

func problemSet(t *testing.T, b *backend, now time.Time, act *radioActivity) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, p := range b.health(now, act).Problems {
		set[p] = true
	}
	return set
}

func TestHealth_NoRadioIsReportedNotZeroed(t *testing.T) {
	t.Parallel()
	b := newHealthBackend(t)
	info := b.health(time.Now(), &radioActivity{})

	if info.Radio.Connected {
		t.Fatal("Radio.Connected true with no modem")
	}
	// The counters must not read as a healthy quiet radio, and the ages must say "cannot measure"
	// rather than "just now".
	if info.Radio.LastRxSecs != nil || info.Radio.LastTxSecs != nil || info.Radio.LastReplySecs != nil {
		t.Errorf("ages should be null before any traffic, got rx=%v tx=%v reply=%v",
			info.Radio.LastRxSecs, info.Radio.LastTxSecs, info.Radio.LastReplySecs)
	}
	if info.Radio.Transport != "" {
		t.Errorf("Transport = %q with no modem", info.Radio.Transport)
	}

	problems := problemSet(t, b, time.Now(), &radioActivity{})
	for _, want := range []string{"radio: modem not connected", "no companion or repeater node is running"} {
		if !problems[want] {
			t.Errorf("missing problem %q, got %v", want, problems)
		}
	}
}

func TestHealth_ReportsTrafficAges(t *testing.T) {
	t.Parallel()
	b := newHealthBackend(t)
	now := time.Now()

	var act radioActivity
	act.lastRx.Store(now.Add(-90 * time.Second).UnixNano())
	act.lastTx.Store(now.Add(-5 * time.Second).UnixNano())

	info := b.health(now, &act)
	if info.Radio.LastRxSecs == nil || *info.Radio.LastRxSecs != 90 {
		t.Errorf("LastRxSecs = %v, want 90", info.Radio.LastRxSecs)
	}
	if info.Radio.LastTxSecs == nil || *info.Radio.LastTxSecs != 5 {
		t.Errorf("LastTxSecs = %v, want 5", info.Radio.LastTxSecs)
	}
	// An age is a fact for the operator to threshold, never a verdict here: 90s of silence on a
	// quiet mesh is normal, and nothing in this endpoint may decide otherwise.
	if problemSet(t, b, now, &act)["radio: silent"] {
		t.Error("mesh silence was turned into a problem")
	}
}

func TestHealth_DroppedWritesAreReportedByAgeNotLatched(t *testing.T) {
	t.Parallel()
	b := newHealthBackend(t)

	if info := b.health(time.Now(), &radioActivity{}); info.Database.WritesDroppedLastSecs != nil {
		t.Fatalf("reported a drop age of %v before any write was dropped",
			*info.Database.WritesDroppedLastSecs)
	}

	// Fill the queue behind a blocked writer, then overflow it: a dropped write is invisible
	// everywhere else, which is the whole reason it is published here.
	release := make(chan struct{})
	b.db.WriteAsync(func() { <-release })
	for range 4096 {
		b.db.WriteAsync(func() {})
	}
	defer close(release)

	info := b.health(time.Now(), &radioActivity{})
	if info.Database.WritesDropped == 0 {
		t.Fatalf("no writes recorded as dropped; queue len %d of %d",
			info.Database.WriteQueueLen, info.Database.WriteQueueCap)
	}
	if info.Database.WritesDroppedLastSecs == nil {
		t.Fatal("writes were dropped but no age was reported, so a monitor cannot tell when")
	}

	// The count only ever rises. Flagging it as a problem would leave this endpoint degraded for
	// the life of the process after one transient overflow, so recency is reported instead and the
	// operator decides what window matters.
	for _, p := range info.Problems {
		if strings.HasPrefix(p, "database:") {
			t.Errorf("a past write loss latched into problems: %q", p)
		}
	}
	if info.Status != "" && len(info.Problems) > 0 {
		t.Logf("problems (should not mention the database): %v", info.Problems)
	}
}

// countingStats records how often something asks the board a question over the wire. Embedding the
// interface means any method this test does not define panics rather than quietly passing.
type countingStats struct {
	modem.StatsProvider
	polls atomic.Int64
}

func (c *countingStats) Stats(context.Context) modem.DeviceStats {
	c.polls.Add(1)
	return modem.DeviceStats{BatteryMV: 1111, HaveBattery: true}
}

func (c *countingStats) CachedStats() modem.DeviceStats {
	return modem.DeviceStats{BatteryMV: 4200, HaveBattery: true}
}
func (c *countingStats) Transport() string            { return "kiss" }
func (c *countingStats) LinkStats() modem.LinkStats   { return modem.LinkStats{} }
func (c *countingStats) RadioConfig() modem.RadioInfo { return modem.RadioInfo{} }

// A monitor scrapes this endpoint on a schedule. Asking the radio a question per scrape puts real
// traffic on a half-duplex link to answer "are you well", and costs the KISS path a 500ms wait, so
// health must read the cached readings and only the diagnostics endpoint may poll.
func TestHealth_DoesNotPollTheBoard(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "poll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stats := &countingStats{}
	b := newBackend(nil, nil, db, stats, nil, nil, nil, nil)

	info := b.health(time.Now(), &radioActivity{})
	if got := stats.polls.Load(); got != 0 {
		t.Errorf("health polled the board %d times, want 0", got)
	}
	if info.Radio.BatteryMV == nil || *info.Radio.BatteryMV != 4200 {
		t.Errorf("battery = %v, want the cached 4200", info.Radio.BatteryMV)
	}

	// The diagnostics endpoint still polls: a person looking at the radio page wants it fresh.
	if _, ok := b.RadioStats(); !ok {
		t.Fatal("RadioStats reported no radio")
	}
	if got := stats.polls.Load(); got != 1 {
		t.Errorf("RadioStats polled %d times, want 1", got)
	}
}

func TestBrokerHealth_AgesNotFlags(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	t.Run("a recovered broker does not stay failing", func(t *testing.T) {
		// The observer records the last error it ever saw and never clears it on reconnect, so a
		// boolean built from it would read as broken for the life of the process.
		got := brokerHealth(api.MqttBrokerStatus{
			Name: "letsmesh", Enabled: true, Connected: true,
			ConnectedTs: now.Add(-2 * time.Hour).Unix(),
			LastError:   "dial tcp: connection refused",
			LastErrorTs: now.Add(-3 * time.Hour).Unix(),
		}, now)

		if got.LastErrorSecs == nil || *got.LastErrorSecs != 10800 {
			t.Errorf("LastErrorSecs = %v, want 10800", got.LastErrorSecs)
		}
		if got.ConnectedSecs == nil || *got.ConnectedSecs != 7200 {
			t.Errorf("ConnectedSecs = %v, want 7200", got.ConnectedSecs)
		}
	})

	t.Run("flapping is visible as a resetting age", func(t *testing.T) {
		// Connected is sampled, so a broker reconnecting every thirty seconds reads true on nearly
		// every scrape. Only the age gives it away.
		got := brokerHealth(api.MqttBrokerStatus{
			Name: "flappy", Enabled: true, Connected: true,
			ConnectedTs: now.Add(-4 * time.Second).Unix(),
			LastErrorTs: now.Add(-5 * time.Second).Unix(),
		}, now)

		if !got.Connected {
			t.Fatal("Connected false")
		}
		if got.ConnectedSecs == nil || *got.ConnectedSecs != 4 {
			t.Errorf("ConnectedSecs = %v, want 4 — a boolean cannot show this", got.ConnectedSecs)
		}
	})

	t.Run("a disconnected broker reports no connection age", func(t *testing.T) {
		// ConnectedTs survives the disconnect, so using it here would claim the broker had been
		// connected for hours while it was down.
		got := brokerHealth(api.MqttBrokerStatus{
			Name: "down", Enabled: true, Connected: false,
			ConnectedTs: now.Add(-6 * time.Hour).Unix(),
			LastErrorTs: now.Add(-30 * time.Second).Unix(),
		}, now)

		if got.ConnectedSecs != nil {
			t.Errorf("ConnectedSecs = %v while disconnected, want null", *got.ConnectedSecs)
		}
		if got.LastErrorSecs == nil || *got.LastErrorSecs != 30 {
			t.Errorf("LastErrorSecs = %v, want 30", got.LastErrorSecs)
		}
	})

	t.Run("a broker that never erred reports no error age", func(t *testing.T) {
		got := brokerHealth(api.MqttBrokerStatus{
			Name: "clean", Enabled: true, Connected: true, ConnectedTs: now.Add(-time.Minute).Unix(),
		}, now)
		if got.LastErrorSecs != nil {
			t.Errorf("LastErrorSecs = %v, want null", *got.LastErrorSecs)
		}
	})
}
