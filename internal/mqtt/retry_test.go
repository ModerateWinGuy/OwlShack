package mqtt

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-go/OwlShack/internal/config"
	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/node"
)

// fakeBroker answers MQTT 3.1.1 CONNECT with CONNACK and ACKs what it can, which is all paho's Connect needs.
type fakeBroker struct {
	ln      net.Listener
	accepts atomic.Int32
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
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeBroker) serve(conn net.Conn) {
	defer conn.Close()
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

	first, err := o.connectBroker(bc.cfg, "TST")
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
