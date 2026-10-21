package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"

	"github.com/OwlShack/OwlShack/internal/client/repeater"
	"github.com/OwlShack/OwlShack/internal/monitor"
	"github.com/OwlShack/OwlShack/internal/node/companion"
	"github.com/OwlShack/OwlShack/internal/store"
	"github.com/OwlShack/OwlShack/internal/telemetry"
)

// Per-request timeouts for a monitor poll; login is a flood round-trip, so it gets more.
const (
	monitorLoginTimeout = 15 * time.Second
	monitorReqTimeout   = 12 * time.Second
	// monitorRequestGap spaces round-trips within one poll; back-to-back requests collide on air.
	monitorRequestGap = 1500 * time.Millisecond
	// monitorProbeAttempts: RF round-trips drop silently, so one in-poll retry beats waiting a whole cycle.
	monitorProbeAttempts = 2
)

// gap pauses between sub-requests, returning early if the context is cancelled.
func gap(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(monitorRequestGap):
	}
}

// companionRegistry is a reload-surviving handle to the current companions, so collectors resolve by name instead of capturing a slice.
type companionRegistry struct {
	mu     sync.RWMutex
	byName map[string]*companion.Companion
	// onPush, set before the first set, gets every companion's sensor pushes.
	onPush func(c *companion.Companion, pubkeyHex string, t *telemetry.Telemetry)
}

func newCompanionRegistry() *companionRegistry {
	return &companionRegistry{byName: map[string]*companion.Companion{}}
}

func (r *companionRegistry) set(companions []*companion.Companion) {
	m := make(map[string]*companion.Companion, len(companions))
	for _, c := range companions {
		m[c.Name()] = c
		if r.onPush != nil {
			c.OnTelemetryPush(func(pubkeyHex string, t *telemetry.Telemetry) { r.onPush(c, pubkeyHex, t) })
		}
	}
	r.mu.Lock()
	r.byName = m
	r.mu.Unlock()
}

func (r *companionRegistry) find(name string) (*companion.Companion, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byName[name]
	return c, ok
}

// newContactLister scans contacts for the monitor flag, deduped by pubkey so a node shared by two companions isn't polled twice.
func newContactLister(reg *companionRegistry, db *store.Store) monitor.ListerFunc {
	return func(ctx context.Context) ([]monitor.Target, error) {
		reg.mu.RLock()
		comps := make(map[string]*companion.Companion, len(reg.byName))
		for n, c := range reg.byName {
			comps[n] = c
		}
		reg.mu.RUnlock()

		seen := make(map[string]int) // index into targets
		pushing := make(map[string]bool)
		var targets []monitor.Target
		// Sorted, so a node two companions monitor is always polled by the same one; one with push on wins, as only it subscribes.
		for _, name := range slices.Sorted(maps.Keys(comps)) {
			c := comps[name]
			contacts, err := db.Contacts.List(ctx, c.ID())
			if err != nil {
				// Fail the listing rather than drop a companion's nodes, which would read as "not monitored".
				return nil, fmt.Errorf("listing contacts for %q: %w", name, err)
			}
			for _, ct := range contacts {
				if !ct.Metadata.Monitor {
					continue
				}
				key := hex.EncodeToString(ct.PeerPubKey)
				at, dup := seen[key]
				if !takesTarget(dup, pushing[key], ct.Metadata.MonitorPush) {
					continue
				}
				kind := monitorKind(c, ct)
				if kind == "" {
					continue
				}
				if !dup {
					at = len(targets)
					targets = append(targets, monitor.Target{})
				}
				seen[key], pushing[key] = at, ct.Metadata.MonitorPush
				interval := monitorInterval(kind, ct.Metadata, c.Repeaters().SubscribeRefused(key))
				targets[at] = monitor.Target{
					Pubkey:       ct.PeerPubKey,
					CompanionID:  name,
					Kind:         kind,
					IntervalSecs: interval,
					RetrySecs:    ct.Metadata.MonitorRetrySecs,
					MaxRetries:   ct.Metadata.MonitorMaxRetries,
					Probes:       ct.Metadata.MonitorProbes,
				}
			}
		}
		return targets, nil
	}
}

// monitorKind trusts the advertised type over the isRepeater flag: companion firmware has no login handler, so that poll only times out.
func monitorKind(c *companion.Companion, ct store.Contact) string {
	var peerType string
	if c != nil {
		if id, err := meshcore.NewIdentityFromBytes(ct.PeerPubKey); err == nil {
			if p := c.Node().Peers().Lookup(id.PublicKey()); p != nil {
				peerType = strings.ToUpper(p.Type)
			}
		}
	}
	if peerType == "" {
		// Advert not heard yet: the contact row caches the type the operator or a past advert gave it.
		peerType = strings.ToUpper(ct.Type)
	}
	switch peerType {
	case "REPEATER":
		return "repeater"
	case "SENSOR":
		return "sensor"
	case "ROOM", "ROOM_SERVER":
		return "companion"
	case "CHAT", "COMPANION":
		return "companion"
	}
	if ct.Metadata.IsRepeater {
		return "repeater"
	}
	return ""
}

// repeaterCollector owns the session: it logs in when there is none, and re-logs in once when a stale session fails the status request.
type repeaterCollector struct {
	reg *companionRegistry
	db  *store.Store
	log *slog.Logger
	// unanswered counts the quiet polls of each companion's node since a login went unanswered; they send one request, alternating status on the held session and a fresh login, until one answers.
	unanswered sync.Map
}

func newRepeaterCollector(reg *companionRegistry, db *store.Store, log *slog.Logger) *repeaterCollector {
	if log == nil {
		log = slog.Default()
	}
	return &repeaterCollector{reg: reg, db: db, log: log.With("component", "repeater-collector")}
}

func (rc *repeaterCollector) Collect(ctx context.Context, t monitor.Target) (*monitor.CollectResult, error) {
	c, ok := rc.reg.find(t.CompanionID)
	if !ok {
		return nil, fmt.Errorf("companion %q is not running", t.CompanionID)
	}
	return rc.collect(ctx, c.Repeaters(), t, c.ID(), nodeName(c, t.Pubkey))
}

// pollClient is what a poll needs from the repeater client.
type pollClient interface {
	Session(pubkeyHex string) *repeater.Session
	SendLogin(pubkeyHex, password string, timeout time.Duration) (*repeater.LoginResult, error)
	SendStatusReq(pubkeyHex string, timeout time.Duration) (*repeater.Status, error)
	SendTelemetryReq(pubkeyHex string, timeout time.Duration) (*telemetry.Telemetry, error)
	SendNeighborsReq(pubkeyHex string, count uint8, offset uint16, timeout time.Duration) (*repeater.Neighbors, error)
}

func (rc *repeaterCollector) collect(ctx context.Context, client pollClient, t monitor.Target, companionID int64, name string) (*monitor.CollectResult, error) {
	pubkeyHex := hex.EncodeToString(t.Pubkey)

	doStatus := probeEnabled(t.Probes, "status")
	doTelemetry := probeEnabled(t.Probes, "telemetry")
	doNeighbors := probeEnabled(t.Probes, "neighbors")

	key := quietKey(companionID, pubkeyHex)
	hadSession := client.Session(pubkeyHex) != nil
	quietPolls, quiet := rc.unanswered.Load(key)
	if quiet {
		rc.unanswered.Store(key, quietPolls.(int)+1) // odd quiet polls log in, even ones use the held session
	}
	if !hadSession || quiet && quietPolls.(int)%2 == 1 {
		if err := rc.login(ctx, client, t, companionID); err != nil {
			return nil, fmt.Errorf("login: %w", err)
		}
		hadSession, quiet = false, false // just logged in, so a failed status isn't a forgotten session
	}

	res := &monitor.CollectResult{Name: name}
	answered := false // a probe on the held session got a reply
	first := true     // first radio probe drives stale-session recovery
	var lastErr error

	if doStatus {
		status, err := client.SendStatusReq(pubkeyHex, monitorReqTimeout)
		if err == nil && quiet {
			rc.unanswered.Delete(key)
		}
		if err != nil && hadSession && !quiet {
			// The node may have forgotten us (a reboot), so log in again once and retry; a login only replaces ours when it succeeds, so a node merely out of reach keeps it.
			if lerr := rc.login(ctx, client, t, companionID); lerr != nil {
				return nil, fmt.Errorf("re-login after stale session: %w", lerr)
			}
			status, err = client.SendStatusReq(pubkeyHex, monitorReqTimeout)
		}
		if err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
		res.Readings = append(res.Readings, statusReadings(status)...)
		first = false
	}

	// Telemetry and neighbours are best-effort: a failure must not drop the readings already collected.
	if doTelemetry {
		if !first {
			gap(ctx)
		}
		if tel, terr := retryProbe(ctx, rc.log, "telemetry", pubkeyHex, func() (*telemetry.Telemetry, error) {
			return client.SendTelemetryReq(pubkeyHex, monitorReqTimeout)
		}); terr == nil {
			answered = true
			res.Readings = append(res.Readings, telemetryReadings(tel)...)
		} else {
			lastErr = terr
			rc.log.Debug("telemetry poll failed", "pubkey", pubkeyHex, "error", terr)
		}
		first = false
	}

	if doNeighbors {
		if !first {
			gap(ctx)
		}
		if nb, nerr := retryProbe(ctx, rc.log, "neighbors", pubkeyHex, func() (*repeater.Neighbors, error) {
			return client.SendNeighborsReq(pubkeyHex, 32, 0, monitorReqTimeout)
		}); nerr == nil {
			answered = true
			res.Readings = append(res.Readings, monitor.Reading{Metric: "neighbor_count", Value: float64(nb.TotalCount)})
			res.Neighbors = neighborSamples(nb)
		} else {
			lastErr = nerr
			rc.log.Debug("neighbors poll failed", "pubkey", pubkeyHex, "error", nerr)
		}
	}

	if !doStatus && lastErr != nil && !answered {
		// Like an unanswered status: the node is quiet, and later polls alternate the held session and a fresh login.
		if errors.Is(lastErr, repeater.ErrNoReply) {
			rc.unanswered.LoadOrStore(key, 0)
		}
		return nil, fmt.Errorf("no probe answered: %w", lastErr)
	}
	if quiet && answered {
		rc.unanswered.Delete(key)
	}
	return res, nil
}

// retryProbe spaces retries by the request gap, so a retry doesn't collide with the request that just timed out.
func retryProbe[T any](ctx context.Context, log *slog.Logger, label, pubkeyHex string, fn func() (T, error)) (T, error) {
	var (
		zero T
		last error
	)
	for attempt := 1; attempt <= monitorProbeAttempts; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		last = err
		if attempt < monitorProbeAttempts {
			log.Debug(label+" probe failed, retrying", "pubkey", pubkeyHex, "attempt", attempt, "error", err)
			gap(ctx)
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
		}
	}
	return zero, last
}

// probeEnabled treats an empty or nil probe set as "all".
func probeEnabled(probes []string, name string) bool {
	if len(probes) == 0 {
		return true
	}
	for _, p := range probes {
		if p == name {
			return true
		}
	}
	return false
}

// login uses the admin password stored on the contact, blank when the repeater has none.
func (rc *repeaterCollector) login(ctx context.Context, client pollClient, t monitor.Target, companionID int64) error {
	password := ""
	if contact, err := rc.db.Contacts.Get(ctx, companionID, t.Pubkey); err == nil && contact != nil {
		password = contact.Metadata.RepeaterPassword
	}
	pubkeyHex := hex.EncodeToString(t.Pubkey)
	key := quietKey(companionID, pubkeyHex)
	if _, err := client.SendLogin(pubkeyHex, password, monitorLoginTimeout); err != nil {
		if errors.Is(err, repeater.ErrNoReply) {
			rc.unanswered.LoadOrStore(key, 0)
		}
		return err
	}
	rc.unanswered.Delete(key)
	return nil
}

// quietKey is per companion, as each companion's client holds its own session.
func quietKey(companionID int64, pubkeyHex string) string {
	return fmt.Sprintf("%d/%s", companionID, pubkeyHex)
}

// nodeName returns "" when the name is unknown, in which case node_state keeps its previous one.
func nodeName(c *companion.Companion, pubkey []byte) string {
	id, err := meshcore.NewIdentityFromBytes(pubkey)
	if err != nil {
		return ""
	}
	if p := c.Node().Peers().Lookup(id.PublicKey()); p != nil {
		return p.Name
	}
	return ""
}

// statusReadings: everything lands on channel 0; SNR is already real dB, RSSI and noise are dBm.
func statusReadings(s *repeater.Status) []monitor.Reading {
	return []monitor.Reading{
		{Metric: "battery_mv", Value: float64(s.BatteryMV)},
		{Metric: "queue_len", Value: float64(s.QueueLen)},
		{Metric: "noise_floor", Value: float64(s.NoiseFloor)},
		{Metric: "last_rssi", Value: float64(s.LastRSSI)},
		{Metric: "last_snr", Value: s.LastSNR},
		{Metric: "packets_recv", Value: float64(s.PacketsRecv)},
		{Metric: "packets_sent", Value: float64(s.PacketsSent)},
		{Metric: "tx_air_secs", Value: float64(s.TxAirSecs)},
		{Metric: "rx_air_secs", Value: float64(s.RxAirSecs)},
		{Metric: "uptime", Value: float64(s.UptimeSecs)},
		{Metric: "flood_tx", Value: float64(s.FloodTx)},
		{Metric: "direct_tx", Value: float64(s.DirectTx)},
		{Metric: "flood_rx", Value: float64(s.FloodRx)},
		{Metric: "direct_rx", Value: float64(s.DirectRx)},
		{Metric: "err_events", Value: float64(s.ErrEvents)},
		{Metric: "recv_errors", Value: float64(s.RecvErrors)},
		{Metric: "direct_dups", Value: float64(s.DirectDups)},
		{Metric: "flood_dups", Value: float64(s.FloodDups)},
		{Metric: "chan_util", Value: s.ChanUtil},
	}
}

// telemetryReadings only maps an already-decoded payload; LPP decode, naming and keying live in internal/telemetry.
func telemetryReadings(t *telemetry.Telemetry) []monitor.Reading {
	metrics := t.Metrics()
	out := make([]monitor.Reading, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, monitor.Reading{Metric: m.Key, Channel: m.Channel, Value: m.Value})
	}
	return out
}

// neighborSamples converts the firmware's x4 quarter-dB SNR to the real dB we store.
func neighborSamples(nb *repeater.Neighbors) []monitor.NeighborSample {
	out := make([]monitor.NeighborSample, 0, len(nb.Neighbors))
	for _, n := range nb.Neighbors {
		prefix, err := hex.DecodeString(n.PubkeyPrefix)
		if err != nil {
			continue
		}
		snr := n.SNR
		out = append(out, monitor.NeighborSample{Pubkey: prefix, SNR: &snr})
	}
	return out
}

// companionCollector gathers telemetry only: companions answer the sessionless telemetry request but no admin requests.
type companionCollector struct {
	reg *companionRegistry
	log *slog.Logger
}

func newCompanionCollector(reg *companionRegistry, log *slog.Logger) *companionCollector {
	if log == nil {
		log = slog.Default()
	}
	return &companionCollector{reg: reg, log: log.With("component", "companion-collector")}
}

func (cc *companionCollector) Collect(ctx context.Context, t monitor.Target) (*monitor.CollectResult, error) {
	c, ok := cc.reg.find(t.CompanionID)
	if !ok {
		return nil, fmt.Errorf("companion %q is not running", t.CompanionID)
	}
	res := &monitor.CollectResult{Name: nodeName(c, t.Pubkey)}

	// Telemetry is all a companion answers.
	if !probeEnabled(t.Probes, "telemetry") {
		return res, nil
	}

	pubkeyHex := hex.EncodeToString(t.Pubkey)
	tel, err := retryProbe(ctx, cc.log, "telemetry", pubkeyHex, func() (*telemetry.Telemetry, error) {
		return c.Repeaters().SendContactTelemetryReq(pubkeyHex, monitorReqTimeout)
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	res.Readings = telemetryReadings(tel)
	return res, nil
}

// pushIntervalSecs is a pushing sensor's default poll, which renews its watch.
const pushIntervalSecs = 3600

// takesTarget is whether a companion's contact polls a node: the first in name order does, unless a later one pushes and it doesn't.
func takesTarget(taken, takenPushes, pushes bool) bool {
	return !taken || !takenPushes && pushes
}

// monitorInterval gives a pushing sensor a short default poll, so a rebooted sensor's lost watch comes back soon; refused, it is polled as usual.
func monitorInterval(kind string, m store.ContactMetadata, refused bool) int64 {
	if kind == "sensor" && m.MonitorPush && m.MonitorIntervalSecs == 0 && !refused {
		return pushIntervalSecs
	}
	return m.MonitorIntervalSecs
}

// pushWatchSecs outlasts the next poll by ten minutes, within the 16 bits the sensor keeps.
func pushWatchSecs(intervalSecs int64) uint16 {
	if intervalSecs <= 0 {
		intervalSecs = monitor.DefaultIntervalSecs
	}
	return uint16(min(intervalSecs+600, 65535))
}

// sensorCollector polls a sensor sessionless, as a companion; with push on it logs in, reads telemetry and renews the watch.
type sensorCollector struct {
	plain *companionCollector
	admin *repeaterCollector
}

// pushClient is what a pushing sensor's poll needs.
type pushClient interface {
	pollClient
	SendSubscribe(pubkeyHex string, timeoutSecs uint16, deltas []meshcore.LPPDataValue, timeout time.Duration) (meshcore.SubscribeReply, error)
}

func (sc *sensorCollector) Collect(ctx context.Context, t monitor.Target) (*monitor.CollectResult, error) {
	c, ok := sc.admin.reg.find(t.CompanionID)
	if !ok {
		return nil, fmt.Errorf("companion %q is not running", t.CompanionID)
	}
	// Read at the start and again before subscribing, so a poll in flight when push is turned off doesn't start a watch after the stop.
	watch := func() []store.PushDelta {
		ct, err := sc.admin.db.Contacts.Get(ctx, c.ID(), t.Pubkey)
		if err != nil || !ct.Metadata.Monitor || !ct.Metadata.MonitorPush {
			return nil
		}
		return ct.Metadata.MonitorPushDeltas
	}
	if watch() == nil {
		rm, pubkeyHex := c.Repeaters(), hex.EncodeToString(t.Pubkey)
		if rm.Watching(pubkeyHex) {
			if err := rm.SendUnsubscribe(pubkeyHex, monitorReqTimeout); err != nil {
				sc.admin.log.Warn("sensor push not stopped", "pubkey", pubkeyHex, "error", err)
			}
			gap(ctx)
		}
		return sc.plain.Collect(ctx, t)
	}
	return sc.collect(ctx, c.Repeaters(), t, c.ID(), nodeName(c, t.Pubkey), watch)
}

func (sc *sensorCollector) collect(ctx context.Context, client pushClient, t monitor.Target, companionID int64, name string, watch func() []store.PushDelta) (*monitor.CollectResult, error) {
	t.Probes = []string{"telemetry"}
	res, err := sc.admin.collect(ctx, client, t, companionID, name)
	if err != nil {
		return nil, err
	}
	gap(ctx)
	w := watch()
	if len(w) == 0 {
		return res, nil
	}
	deltas := make([]meshcore.LPPDataValue, len(w))
	for i, d := range w {
		deltas[i] = meshcore.LPPDataValue{Channel: byte(d.Channel), Type: byte(d.Type), Value: d.Value}
	}
	pubkeyHex := hex.EncodeToString(t.Pubkey)
	_, err = client.SendSubscribe(pubkeyHex, pushWatchSecs(t.IntervalSecs), deltas, monitorReqTimeout)
	switch {
	case err == nil:
	case errors.Is(err, repeater.ErrSubscribeRefused):
		sc.admin.log.Info("sensor refused to push; polling it instead", "pubkey", pubkeyHex)
	case errors.Is(err, repeater.ErrRejected):
		// Retrying a request the sensor or the encoder turns down only costs airtime.
		sc.admin.log.Warn("sensor push rejected", "pubkey", pubkeyHex, "error", err)
	default:
		sc.admin.log.Warn("sensor push not renewed", "pubkey", pubkeyHex, "error", err)
		res.RetryFailure = true
	}
	return res, nil
}

// pollSensorsSoon re-polls the pushing sensors after their companions' clients are rebuilt, which forgets every watch.
func pollSensorsSoon(mon *monitor.Service, db *store.Store, companions []*companion.Companion) {
	for _, c := range companions {
		contacts, err := db.Contacts.List(context.Background(), c.ID())
		if err != nil {
			continue
		}
		for _, ct := range contacts {
			if ct.Metadata.Monitor && ct.Metadata.MonitorPush {
				mon.PollSoon(ct.PeerPubKey)
			}
		}
	}
}

// pushWanted is whether the contact still asks for pushes: a watch can outlive its settings (a deleted contact, a stop the sensor
// never heard), and its pushes are then not ours to keep.
func pushWanted(ctx context.Context, db *store.Store, companionID int64, pubkey []byte) bool {
	ct, err := db.Contacts.Get(ctx, companionID, pubkey)
	return err == nil && ct.Metadata.Monitor && ct.Metadata.MonitorPush
}

// pushRecorder stores a sensor's push in Monitoring as it arrives.
func pushRecorder(mon *monitor.Service, db *store.Store) func(c *companion.Companion, pubkeyHex string, t *telemetry.Telemetry) {
	return func(c *companion.Companion, pubkeyHex string, t *telemetry.Telemetry) {
		pk, err := hex.DecodeString(pubkeyHex)
		if err != nil {
			return
		}
		if !pushWanted(context.Background(), db, c.ID(), pk) {
			return
		}
		mon.RecordPush(monitor.Target{Pubkey: pk, CompanionID: c.Name(), Kind: "sensor"}, nodeName(c, pk), telemetryReadings(t))
	}
}

var (
	_ monitor.Collector = (*repeaterCollector)(nil)
	_ monitor.Collector = (*companionCollector)(nil)
	_ monitor.Collector = (*sensorCollector)(nil)
)
