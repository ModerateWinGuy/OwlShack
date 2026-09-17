package app

import (
	"sync/atomic"
	"time"

	"github.com/meshcore-go/OwlShack/internal/api"
)

// radioActivity is process-scoped, not a backend field: a reload swaps the backend but the radio's history carries on. 0 = none since start.
type radioActivity struct{ lastRx, lastTx atomic.Int64 }

func (a *radioActivity) rx() { a.lastRx.Store(time.Now().UnixNano()) }
func (a *radioActivity) tx() { a.lastTx.Store(time.Now().UnixNano()) }

// radioSeen is written by the packet logger, which sees every frame in both directions.
var radioSeen radioActivity

// secsSinceTime tests IsZero, not UnixNano: a zero time.Time's nanos are a large negative number, not 0.
func secsSinceTime(t, now time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	secs := int64(now.Sub(t).Seconds())
	return &secs
}

// secsSince is nil where there is nothing to measure from, so "silent" and "cannot be asked" stay distinct.
func secsSince(nanos int64, now time.Time) *int64 {
	if nanos == 0 {
		return nil
	}
	secs := int64(now.Sub(time.Unix(0, nanos)).Seconds())
	return &secs
}

func (b *backend) Health() api.HealthInfo {
	return b.health(time.Now(), &radioSeen)
}

// health takes now and act so a test can drive both without a clock or a radio.
func (b *backend) health(now time.Time, act *radioActivity) api.HealthInfo {
	info := api.HealthInfo{
		Problems: []string{},
		Radio:    b.radioHealth(now, act),
		Brokers:  []api.BrokerHealth{},
	}

	if b.db != nil {
		queued, capacity, dropped, lastDrop := b.db.WriterStats()
		info.Database = api.DatabaseHealth{
			WriteQueueLen: queued, WriteQueueCap: capacity, WritesDropped: dropped,
			WritesDroppedLastSecs: secsSinceTime(lastDrop, now),
		}
		// Not a problem entry: the count never resets, so one transient overflow would pin "degraded" until restart.
	}

	if brokers, ok := b.MqttStatus(); ok {
		for _, br := range brokers {
			info.Brokers = append(info.Brokers, brokerHealth(br, now))
			// Only a broker that is meant to be up counts: a disabled one is not a fault.
			if br.Enabled && !br.Connected {
				info.Problems = append(info.Problems, "mqtt: broker "+br.Name+" is not connected")
			}
		}
	}

	if !info.Radio.Connected {
		info.Problems = append(info.Problems, "radio: modem not connected")
	}
	if len(b.companions) == 0 && b.repeater == nil {
		info.Problems = append(info.Problems, "no companion or repeater node is running")
	}

	return info
}

// brokerHealth maps one broker's status onto the wire shape, turning both timestamps into ages.
func brokerHealth(st api.MqttBrokerStatus, now time.Time) api.BrokerHealth {
	h := api.BrokerHealth{
		Name: st.Name, Enabled: st.Enabled, Connected: st.Connected,
		LastErrorSecs: secsSinceUnix(st.LastErrorTs, now),
		Published:     st.Published, Dropped: st.Dropped,
	}
	// The timestamp survives a disconnect; an age from it would claim hours of uptime while down.
	if st.Connected {
		h.ConnectedSecs = secsSinceUnix(st.ConnectedTs, now)
	}
	return h
}

// secsSinceUnix is secsSince for the unix-second timestamps the MQTT status carries; 0 means unset.
func secsSinceUnix(ts int64, now time.Time) *int64 {
	if ts == 0 {
		return nil
	}
	secs := int64(now.Sub(time.Unix(ts, 0)).Seconds())
	return &secs
}

func (b *backend) radioHealth(now time.Time, act *radioActivity) api.RadioHealth {
	h := api.RadioHealth{
		LastRxSecs: secsSince(act.lastRx.Load(), now),
		LastTxSecs: secsSince(act.lastTx.Load(), now),
	}

	// The liveness probe's own signal; null on a transport that cannot be probed, rather than claiming silence.
	if lr, ok := b.stats.(interface{ LastReply() time.Time }); ok {
		if t := lr.LastReply(); !t.IsZero() {
			secs := int64(now.Sub(t).Seconds())
			h.LastReplySecs = &secs
		}
	}

	// false: a monitor scrapes on a schedule, and polling the board per scrape puts traffic on the link.
	stats, ok := b.radioStats(false)
	if !ok {
		return h // no modem: everything below would be a zero that reads as healthy
	}
	// A modem exists. On a transport that reconnects itself the object outlives the link, so ask it
	// whether the link is actually up; the others are torn down and rebuilt, where existing is up.
	h.Connected = true
	if c, ok := b.stats.(interface{ Connected() bool }); ok {
		h.Connected = c.Connected()
	}
	h.Transport = stats.Transport
	h.TxQueueLen = stats.TxQueueLen
	h.TxSent = stats.TxSent
	h.TxFailed = stats.TxFailed
	h.TxDroppedBusy = stats.TxDroppedBusy
	h.TxDroppedQueue = stats.TxDroppedQueue
	h.InboundDroppedNew = stats.InboundDroppedNew
	h.HandlerSlow = stats.HandlerSlow
	h.CRCErrors = stats.CRCErrors
	h.RecvErrors = stats.RecvErrors
	h.DriverErrors = stats.DriverErrors
	h.HwErrors = stats.HwErrors
	h.NoiseFloor = stats.NoiseFloor
	h.BatteryMV = stats.BatteryMV
	h.MCUTempC = stats.MCUTempC
	return h
}
