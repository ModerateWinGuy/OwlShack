package companion

import (
	"time"

	"github.com/OwlShack/OwlShack/internal/client/repeater"
	"github.com/OwlShack/OwlShack/internal/telemetry"
)

// broadcastTelemetryPush shows a subscribed sensor's pushed readings live, on the "telemetry" topic.
func (c *Companion) broadcastTelemetryPush(pubkeyHex string, t *telemetry.Telemetry) {
	if c.hub == nil {
		return
	}
	cfg := c.conf()
	c.hub.Broadcast("telemetry", map[string]any{"companion": cfg.Name, "companionId": cfg.ID, "pubkey": pubkeyHex, "readings": t.Readings, "receivedAt": time.Now()})
}

// OnTelemetryPush hands each sensor push to f as well as broadcasting it.
func (c *Companion) OnTelemetryPush(f repeater.TelemetryPushHandler) {
	c.repeaters.SetTelemetryPushHandler(func(pubkeyHex string, t *telemetry.Telemetry) {
		c.broadcastTelemetryPush(pubkeyHex, t)
		f(pubkeyHex, t)
	})
}
