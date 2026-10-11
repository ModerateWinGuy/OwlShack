package mqtt

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OwlShack/OwlShack/internal/config"
	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
	paho "github.com/eclipse/paho.mqtt.golang"
)

// fakeBroker answers MQTT 3.1.1 CONNECT with CONNACK and ACKs what it can, which is all paho's Connect needs.
type fakeBroker struct {
	ln      net.Listener
	accepts atomic.Int32
	// takeovers counts CONNECTs that found another session still open, which a real broker kicks and publishes the will of.
	takeovers atomic.Int32
	// connackDelay holds each CONNACK back, as a slow broker or link does.
	connackDelay time.Duration
	mu           sync.Mutex
	conns        []net.Conn
	live         map[net.Conn]bool
}

// drop stops the broker, closing every session, as a broker restart or a lost network does.
func (f *fakeBroker) drop() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
}

func listenFakeBroker(t *testing.T, addr string) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	f := &fakeBroker{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepts.Add(1)
			f.mu.Lock()
			f.conns = append(f.conns, conn)
			f.mu.Unlock()
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeBroker) serve(conn net.Conn) {
	defer func() {
		f.mu.Lock()
		delete(f.live, conn)
		f.mu.Unlock()
		conn.Close()
	}()
	for {
		hdr := make([]byte, 1)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		// Remaining Length is a varint of up to 4 bytes.
		var rem, mult int = 0, 1
		for i := 0; i < 4; i++ {
			b := make([]byte, 1)
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			rem += int(b[0]&0x7F) * mult
			if b[0]&0x80 == 0 {
				break
			}
			mult *= 128
		}
		body := make([]byte, rem)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		switch hdr[0] & 0xF0 {
		case 0x10: // CONNECT -> CONNACK, session not present, accepted
			f.takeOver(conn)
			time.Sleep(f.connackDelay)
			conn.Write([]byte{0x20, 0x02, 0x00, 0x00})
		case 0x30: // PUBLISH; QoS 1 carries a packet id we must PUBACK
			if (hdr[0]>>1)&0x03 == 1 && rem >= 2 {
				tlen := int(body[0])<<8 | int(body[1])
				if rem >= 2+tlen+2 {
					id := body[2+tlen : 2+tlen+2]
					conn.Write([]byte{0x40, 0x02, id[0], id[1]})
				}
			}
		case 0xC0: // PINGREQ -> PINGRESP
			conn.Write([]byte{0xD0, 0x00})
		case 0xE0: // DISCONNECT
			return
		}
	}
}

// takeOver kicks any other open session, as every client here shares one ClientID; a DISCONNECT still in flight gets a moment to land first.
func (f *fakeBroker) takeOver(conn net.Conn) {
	for range 2 {
		f.mu.Lock()
		others := 0
		for c := range f.live {
			if c != conn {
				others++
			}
		}
		f.mu.Unlock()
		if others == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.live {
		if c != conn {
			f.takeovers.Add(1)
			c.Close()
			delete(f.live, c)
		}
	}
	if f.live == nil {
		f.live = map[net.Conn]bool{}
	}
	f.live[conn] = true
}

// freePort binds and releases a port, so connecting to it refuses until listenFakeBroker claims it.
func freePort(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()
	return addr.String(), addr.Port
}

func testObserver(t *testing.T) *Observer {
	t.Helper()
	var seed [ed25519.SeedSize]byte
	seed[0] = 1 // deterministic; the identity only has to exist and be stable
	id := meshcore.NewLocalIdentityFromSeed(seed)
	return &Observer{
		log:         slog.New(slog.DiscardHandler),
		id:          id,
		originName:  "test",
		pubKeyHx:    publicKeyHex(id),
		health:      map[string]*brokerHealth{},
		parseErrors: &atomic.Uint64{},
	}
}

func testBrokerClient(name, host string, port int, authType string) *brokerClient {
	return &brokerClient{
		cfg: config.BrokerConfig{
			Name: name, Host: host, Port: port,
			Transport: "tcp", AuthType: authType,
		},
		statusTopicStr: "meshcore/test/status",
		publishCh:      make(chan publishJob, publishQueueDepth),
		stop:           make(chan struct{}),
		workerDone:     make(chan struct{}),
	}
}

func shrinkBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := connectRetryMin, connectRetryMax
	connectRetryMin, connectRetryMax = 50*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { connectRetryMin, connectRetryMax = oldMin, oldMax })
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A broker that fails its FIRST connect must still be registered and still be retrying: Start appends
// before it dials for exactly this reason, and an append moved below the dial would drop the broker
// from o.brokers entirely, leaving nothing holding a reference and no path back but a SIGHUP.
func TestStart_RegistersABrokerThatFailsItsFirstConnect(t *testing.T) {
	shrinkBackoff(t)
	_, port := freePort(t) // nothing listening: the first connect cannot succeed

	o := testObserver(t)
	o.radio = (&node.RadioMux{}).NewRadio()
	o.cfg = config.MqttConfig{Brokers: []config.BrokerConfig{{
		Name: "down", Host: "127.0.0.1", Port: port,
		Transport: "tcp", AuthType: "basic", Enabled: true,
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	if err := o.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	brokers := o.brokerList()
	if len(brokers) != 1 {
		t.Fatalf("unreachable broker not registered: got %d brokers, want 1", len(brokers))
	}
	bc := brokers[0]

	// The retry goroutine reads the backoff vars that shrinkBackoff restores on cleanup, so it has to be gone first.
	t.Cleanup(func() {
		o.Stop()
		cancel()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && bc.retrying.Load(); {
			time.Sleep(5 * time.Millisecond)
		}
	})

	if bc.currentClient() != nil {
		t.Error("a broker that never connected must hold no client")
	}
	waitFor(t, "the retry loop to take ownership", time.Second, bc.retrying.Load)
}

// A broker down at startup must keep retrying: paho's SetAutoReconnect does not cover a client that never connected.
func TestRetryConnect_RecoversWhenBrokerAppears(t *testing.T) {
	shrinkBackoff(t)
	addr, port := freePort(t)

	o := testObserver(t)
	bc := testBrokerClient("late", "127.0.0.1", port, "none")
	o.brokers = append(o.brokers, bc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); o.retryConnect(ctx, bc) }()

	// Nothing is listening: the loop must stay alive and record the failure.
	waitFor(t, "first connect failure recorded", 3*time.Second, func() bool {
		o.healthMu.Lock()
		defer o.healthMu.Unlock()
		h := o.health["late"]
		return h != nil && h.lastErr != ""
	})
	if c := bc.currentClient(); c != nil {
		t.Fatal("client set while the broker was down")
	}
	if !bc.retrying.Load() {
		t.Fatal("retrying flag not held while looping")
	}

	listenFakeBroker(t, addr)

	waitFor(t, "reconnect", 5*time.Second, func() bool {
		c := bc.currentClient()
		return c != nil && c.IsConnected()
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retryConnect did not exit after connecting")
	}
	if bc.retrying.Load() {
		t.Error("retrying flag still held after exit")
	}
	o.healthMu.Lock()
	connectedAt := o.health["late"].connectedAt
	o.healthMu.Unlock()
	if connectedAt.IsZero() {
		t.Error("connectedAt not stamped on success")
	}

	statuses := o.BrokerStatuses()
	if len(statuses) != 0 {
		t.Fatalf("BrokerStatuses reads o.cfg.Brokers, expected none: %+v", statuses)
	}
	if c := bc.currentClient(); c != nil {
		c.Disconnect(0)
	}
}

// The loop must exit when the observer shuts down rather than retrying forever.
func TestRetryConnect_ExitsOnContextAndStop(t *testing.T) {
	shrinkBackoff(t)
	_, port := freePort(t)

	for _, tc := range []string{"ctx", "stop"} {
		t.Run(tc, func(t *testing.T) {
			o := testObserver(t)
			bc := testBrokerClient("down", "127.0.0.1", port, "none")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() { defer close(done); o.retryConnect(ctx, bc) }()
			waitFor(t, "loop running", 3*time.Second, bc.retrying.Load)

			if tc == "ctx" {
				cancel()
			} else {
				close(bc.stop)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("retryConnect did not exit on %s", tc)
			}
			if bc.retrying.Load() {
				t.Error("retrying flag still held after exit")
			}
		})
	}
}

// Only one loop may run per broker: Start and a failed token refresh both spawn one, and two would race on the client pointer.
func TestRetryConnect_SecondCallIsNoOp(t *testing.T) {
	shrinkBackoff(t)
	_, port := freePort(t)

	o := testObserver(t)
	bc := testBrokerClient("down", "127.0.0.1", port, "none")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := make(chan struct{})
	go func() { defer close(first); o.retryConnect(ctx, bc) }()
	waitFor(t, "first loop running", 3*time.Second, bc.retrying.Load)

	// The loop reads the backoff vars shrinkBackoff restores on cleanup, so it must be gone first.
	t.Cleanup(func() {
		cancel()
		<-first
	})

	returned := make(chan struct{})
	go func() { defer close(returned); o.retryConnect(ctx, bc) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("second retryConnect blocked instead of returning immediately")
	}
}

// refreshToken runs on every token broker, and Disconnect on the nil client of one that never connected kills the process.
func TestRefreshToken_SkipsUnconnectedAndRetryingBrokers(t *testing.T) {
	o := testObserver(t)
	ctx := context.Background()

	nilClient := testBrokerClient("token-down", "127.0.0.1", 1, "token")
	if o.refreshToken(ctx, nilClient) {
		t.Error("refreshed a broker with no client")
	}

	retrying := testBrokerClient("token-retrying", "127.0.0.1", 1, "token")
	retrying.retrying.Store(true)
	if o.refreshToken(ctx, retrying) {
		t.Error("refreshed a broker whose retryConnect owns the client")
	}

	basic := testBrokerClient("basic", "127.0.0.1", 1, "basic")
	if o.refreshToken(ctx, basic) {
		t.Error("refreshed a non-token broker")
	}
}

// A token broker that is up gets a new client, and the old one is disconnected rather than orphaned.
func TestRefreshToken_SwapsClient(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)

	o := testObserver(t)
	bc := testBrokerClient("tok", "127.0.0.1", port, "token")
	bc.cfg.Audience = "test"

	first, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("initial connect to fake broker: %v", err)
	}
	bc.swapClient(first)

	if !o.refreshToken(context.Background(), bc) {
		t.Fatal("refreshToken reported no reconnect")
	}
	second := bc.currentClient()
	if second == nil {
		t.Fatal("no client after refresh")
	}
	if second == first {
		t.Error("client pointer unchanged; no new connection was made")
	}
	if first.IsConnected() {
		t.Error("old client left connected (orphaned)")
	}
	waitFor(t, "second connection accepted", 3*time.Second, func() bool {
		return fb.accepts.Load() >= 2
	})
	second.Disconnect(0)
}

// brokerError is the error BrokerStatuses reports for the observer's one broker.
func brokerError(o *Observer) string {
	return o.BrokerStatuses()[0].LastError
}

func registered(o *Observer, bc *brokerClient) {
	bc.cfg.Enabled = true
	o.cfg.Brokers = []config.BrokerConfig{bc.cfg}
	o.brokers = []*brokerClient{bc}
}

// The status is the current connection's: an error from before it is gone, one since it stays.
func TestBrokerStatus_ErrorIsTheCurrentConnections(t *testing.T) {
	o := testObserver(t)
	bc := testBrokerClient("b", "127.0.0.1", 1, "none")
	registered(o, bc)

	o.recordBrokerErr("b", nil, errors.New("pingresp not received, disconnecting"))
	o.recordConnected("b")
	if got := brokerError(o); got != "" {
		t.Errorf("after reconnecting the error is %q, want none", got)
	}
	o.recordBrokerErr("b", nil, errors.New("publish to x timed out"))
	if got := brokerError(o); got != "publish to x timed out" {
		t.Errorf("an error on this connection reads %q, want it kept", got)
	}
}

// A refresh with the old session still open is a takeover: the broker publishes our offline will, and paho's immediate reconnect starts a fight.
func TestRefreshToken_NoTakeover(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)

	o := testObserver(t)
	bc := testBrokerClient("tok", "127.0.0.1", port, "token")
	bc.cfg.Audience = "test"
	registered(o, bc)
	first, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("initial connect to fake broker: %v", err)
	}
	bc.swapClient(first)
	defer func() { bc.currentClient().Disconnect(0) }()

	const refreshes = 5
	for i := range refreshes {
		if !o.refreshToken(context.Background(), bc) {
			t.Fatalf("refresh %d reported no reconnect", i)
		}
	}
	time.Sleep(1500 * time.Millisecond) // past paho's first reconnect, were a kicked client to make one
	if n := fb.takeovers.Load(); n != 0 {
		t.Errorf("%d takeovers in %d refreshes, want none", n, refreshes)
	}
	if n := fb.accepts.Load(); n != refreshes+1 {
		t.Errorf("%d connects for %d refreshes, want %d", n, refreshes, refreshes+1)
	}
	if st := o.BrokerStatuses()[0]; !st.Connected || st.LastError != "" {
		t.Errorf("connected=%v error %q, want connected with no error", st.Connected, st.LastError)
	}
}

// The old client is gone before the new one connects, so a publish in that gap waits for the new one instead of dropping.
func TestRefreshToken_PublishesWaitForTheNewClient(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)

	o := testObserver(t)
	bc := testBrokerClient("tok", "127.0.0.1", port, "token")
	bc.cfg.Audience = "test"
	registered(o, bc)
	first, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("initial connect to fake broker: %v", err)
	}
	bc.swapClient(first)
	go o.publishWorker(bc)
	defer func() {
		close(bc.stop)
		<-bc.workerDone
		bc.currentClient().Disconnect(0)
	}()

	fb.connackDelay = 300 * time.Millisecond
	done := make(chan bool)
	go func() { done <- o.refreshToken(context.Background(), bc) }()
	time.Sleep(100 * time.Millisecond)
	const jobs = 5
	for range jobs {
		o.enqueuePublish(bc, publishJob{topic: "t", payload: []byte("x"), qos: 1})
	}
	if !<-done {
		t.Fatal("refresh reported no reconnect")
	}
	waitFor(t, "the queued publishes", 3*time.Second, func() bool { return bc.published.Load()+bc.dropped.Load() >= jobs })
	if d := bc.dropped.Load(); d != 0 {
		t.Errorf("%d of %d publishes made during the refresh were dropped", d, jobs)
	}
}

// An error from a client the broker has since replaced is not the current one's, whichever order the loss and the swap came in.
func TestBrokerStatus_ReplacedClientsErrorIsHidden(t *testing.T) {
	o := testObserver(t)
	bc := testBrokerClient("b", "127.0.0.1", 1, "token")
	registered(o, bc)
	old, next := paho.NewClient(paho.NewClientOptions()), paho.NewClient(paho.NewClientOptions())

	bc.swapClient(old)
	o.recordBrokerErr("b", old, errors.New("EOF"))
	if got := brokerError(o); got != "EOF" {
		t.Fatalf("the current client's error reads %q, want EOF", got)
	}
	bc.swapClient(next)
	if got := brokerError(o); got != "" {
		t.Errorf("after the swap the old client's error still reads %q", got)
	}
	o.recordBrokerErr("b", old, errors.New("late EOF"))
	if got := brokerError(o); got != "" {
		t.Errorf("a loss reported after the swap reads %q", got)
	}
	o.recordBrokerErr("b", next, errors.New("publish to x timed out"))
	if got := brokerError(o); got != "publish to x timed out" {
		t.Errorf("the new client's error reads %q, want it shown", got)
	}
}

// While paho is reconnecting its IsConnected is still true, so the status must read the open connection instead.
func TestBrokerStatus_DownWhileReconnecting(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)

	o := testObserver(t)
	bc := testBrokerClient("b", "127.0.0.1", port, "none")
	registered(o, bc)
	c, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("connect to fake broker: %v", err)
	}
	bc.swapClient(c)
	defer c.Disconnect(0)

	fb.drop()
	waitFor(t, "the loss to be seen", 3*time.Second, func() bool { return brokerError(o) != "" })
	if st := o.BrokerStatuses()[0]; st.Connected {
		t.Errorf("with the broker gone the status reads connected (error %q)", st.LastError)
	}

	listenFakeBroker(t, addr)
	waitFor(t, "paho to reconnect", 10*time.Second, func() bool { return o.BrokerStatuses()[0].Connected })
	// paho runs OnConnect, which clears the error, on its own goroutine after the session opens.
	waitFor(t, "the error to clear once reconnected", time.Second, func() bool { return brokerError(o) == "" })
}

// While paho reconnects it takes a QoS 0 publish and discards it, and holds a QoS 1 one until the link is back, so both must count as dropped at once.
func TestDoPublish_DropsWhileReconnecting(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)
	o := testObserver(t)
	bc := testBrokerClient("b", "127.0.0.1", port, "none")
	registered(o, bc)
	c, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("connect to fake broker: %v", err)
	}
	bc.swapClient(c)
	defer c.Disconnect(0)

	fb.drop()
	waitFor(t, "the loss to be seen", 3*time.Second, func() bool { return brokerError(o) != "" })
	start := time.Now()
	o.doPublish(bc, publishJob{topic: "packets", payload: []byte("x"), qos: 0})
	o.doPublish(bc, publishJob{topic: "status", payload: []byte("x"), qos: 1})
	if took := time.Since(start); took > time.Second {
		t.Errorf("publishing while reconnecting took %v, want no wait", took.Round(time.Millisecond))
	}
	if p, d := bc.published.Load(), bc.dropped.Load(); p != 0 || d != 2 {
		t.Errorf("published %d dropped %d, want 0 and 2", p, d)
	}
}

// open is how many sessions the broker holds.
func (f *fakeBroker) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

// kick closes every session but keeps listening, as a broker that drops clients does.
func (f *fakeBroker) kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.live {
		c.Close()
		delete(f.live, c)
	}
}

// A refresh while paho is reconnecting would cancel that reconnect and leave nothing dialling once the broker is back.
func TestRefreshToken_LeavesAReconnectingSessionToPaho(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)
	o := testObserver(t)
	bc := testBrokerClient("tok", "127.0.0.1", port, "token")
	bc.cfg.Audience = "test"
	registered(o, bc)
	c, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("connect to fake broker: %v", err)
	}
	bc.swapClient(c)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		close(bc.stop)
		waitFor(t, "any retry loop to exit", 3*time.Second, func() bool { return !bc.retrying.Load() })
		if c := bc.currentClient(); c != nil {
			c.Disconnect(0)
		}
	})

	fb.drop()
	waitFor(t, "the loss to be seen", 3*time.Second, func() bool { return brokerError(o) != "" })
	if o.refreshToken(ctx, bc) {
		t.Error("refreshed a session paho is reconnecting")
	}
	back := listenFakeBroker(t, addr)
	waitFor(t, "the broker to be reconnected", 10*time.Second, func() bool { return o.BrokerStatuses()[0].Connected })
	if n, k := back.accepts.Load(), back.takeovers.Load(); n != 1 || k != 0 {
		t.Errorf("%d connects and %d takeovers once the broker was back, want 1 and 0", n, k)
	}
}

// lockedLog is a log the paho goroutines can write while the test reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lockedLog) count(s string) int { return strings.Count(l.String(), s) }

// paho's own reconnect must publish online, or the retained offline will stands until the next heartbeat; the log says it is back after saying it was lost.
func TestReconnect_PublishesOnline(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)
	o := testObserver(t)
	logs := &lockedLog{}
	o.log = slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.runCtx = ctx
	bc := testBrokerClient("b", "127.0.0.1", port, "none")
	registered(o, bc)
	c, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("connect to fake broker: %v", err)
	}
	defer c.Disconnect(0)
	bc.swapClient(c)
	time.Sleep(200 * time.Millisecond)
	if n := len(bc.publishCh); n != 0 {
		t.Fatalf("the first connect queued %d publishes; whoever adopts the client announces it", n)
	}
	if n := logs.count("broker reconnected"); n != 0 {
		t.Fatalf("the first connect logged %d reconnects", n)
	}

	fb.drop()
	waitFor(t, "the loss to be seen", 3*time.Second, func() bool { return brokerError(o) != "" })
	listenFakeBroker(t, addr)
	waitFor(t, "paho to reconnect", 10*time.Second, func() bool { return o.BrokerStatuses()[0].Connected })
	waitFor(t, "a status after the reconnect", 2*time.Second, func() bool { return len(bc.publishCh) > 0 })
	job := <-bc.publishCh
	if job.topic != bc.statusTopic() || !strings.Contains(string(job.payload), `"status":"online"`) {
		t.Errorf("after the reconnect published %s on %q, want online on %q", job.payload, job.topic, bc.statusTopic())
	}
	if n := logs.count("level=INFO msg=\"broker reconnected\" broker=b"); n != 1 {
		t.Errorf("logged %d reconnects at INFO, want 1:\n%s", n, logs)
	}
}

// Stop while a refresh is dialling: the client it gets afterwards must be closed, not adopted with nothing left to stop it.
func TestStop_DuringRefreshDialLeavesNoSession(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)
	o := testObserver(t)
	bc := testBrokerClient("tok", "127.0.0.1", port, "token")
	bc.cfg.Audience = "test"
	registered(o, bc)
	first, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("connect to fake broker: %v", err)
	}
	bc.swapClient(first)
	close(bc.workerDone) // no worker, so Stop returns while the refresh still dials

	fb.connackDelay = time.Second
	done := make(chan bool)
	go func() { done <- o.refreshToken(context.Background(), bc) }()
	waitFor(t, "the refresh to dial", 3*time.Second, func() bool { return fb.accepts.Load() == 2 })
	o.Stop()
	if <-done {
		t.Error("the refresh reported success after Stop")
	}
	waitFor(t, "every session to close", 3*time.Second, func() bool { return fb.open() == 0 })
}

// Stop while paho sleeps before a reconnect: the stopped client must never dial again, or it takes over the next observer's session.
func TestStop_InPahoBackoffNeverDialsAgain(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)
	o := testObserver(t)
	bc := testBrokerClient("b", "127.0.0.1", port, "none")
	registered(o, bc)
	c, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("connect to fake broker: %v", err)
	}
	bc.swapClient(c)
	go o.publishWorker(bc)
	fb.kick()
	waitFor(t, "the first reconnect", 3*time.Second, func() bool { return fb.accepts.Load() == 2 && c.IsConnectionOpen() })
	fb.kick() // lost again within 3 s, so paho sleeps before its next attempt
	time.Sleep(200 * time.Millisecond)
	o.Stop()
	before := fb.accepts.Load()
	time.Sleep(2500 * time.Millisecond)
	if n := fb.accepts.Load() - before; n != 0 {
		t.Errorf("the stopped observer's client connected %d more time(s)", n)
	}
}

// Once Stop has run no transport may open a connection.
func TestConnectBroker_StoppedNeverDials(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)
	o := testObserver(t)
	for _, tc := range []struct {
		transport string
		tls       bool
	}{{"tcp", false}, {"tcp", true}, {"websockets", false}, {"websockets", true}} {
		bc := testBrokerClient("b", "127.0.0.1", port, "none")
		bc.cfg.Transport, bc.cfg.TlsEnabled = tc.transport, tc.tls
		close(bc.stop)
		if c, err := o.connectBroker(bc); !errors.Is(err, errStopped) {
			if c != nil {
				c.Disconnect(0)
			}
			t.Errorf("%s tls=%v after Stop: %v, want %v", tc.transport, tc.tls, err, errStopped)
		}
	}
	if n := fb.accepts.Load(); n != 0 {
		t.Errorf("%d connections opened after Stop", n)
	}
}

// Two brokers on one host need their own ClientIDs, or each takes the other over; a lone broker keeps the ID it always had.
func TestClientID_UniquePerBroker(t *testing.T) {
	o := testObserver(t)
	o.cfg.Brokers = []config.BrokerConfig{
		{Name: "tcp", Host: "mqtt.example", Port: 1883, Enabled: true},
		{Name: "ws", Host: "mqtt.example", Port: 443, Enabled: true},
		{Name: "off", Host: "solo.example", Port: 443},
		{Name: "solo", Host: "solo.example", Port: 1883, Enabled: true},
	}
	ids := map[string]string{}
	for _, b := range o.cfg.Brokers {
		ids[b.Name] = o.clientID(b)
	}
	if ids["tcp"] == ids["ws"] {
		t.Errorf("two brokers on one host share ClientID %q", ids["tcp"])
	}
	if want := "meshcore_" + o.pubKeyHx[:16] + "_solo.example"; ids["solo"] != want {
		t.Errorf("a broker alone on its host has ClientID %q, want %q", ids["solo"], want)
	}
}
