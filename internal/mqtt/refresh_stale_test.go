package mqtt

import (
	"context"
	"testing"
	"time"
)

// A failed token refresh must drop the stale client. Its token has minutes of validity left, so
// leaving it connected makes retryConnect's already-connected guard return immediately: the retry
// looks spawned, the broker looks healthy, and nothing reconnects until the next refresh tick —
// by which time the token has expired and paho is auto-reconnecting with dead credentials.
//
// The retry loop this spawns MUST be stopped before the test returns. Not calling shrinkBackoff here
// is not enough: the loop reads the package backoff vars, and any LATER test that shrinks them races
// with it across test boundaries.
func TestRefreshToken_FailureDropsStaleClientSoRetryCanDial(t *testing.T) {
	addr, port := freePort(t)
	fb := listenFakeBroker(t, addr)

	o := testObserver(t)
	bc := testBrokerClient("tok", "127.0.0.1", port, "token")
	bc.cfg.Audience = "test"

	client, err := o.connectBroker(bc)
	if err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	bc.swapClient(client)
	if !client.IsConnected() {
		t.Fatal("precondition: client should be connected")
	}

	// Take the broker away so the refresh dial fails while the old client is still healthy.
	fb.ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if o.refreshToken(ctx, bc) {
		t.Fatal("refreshToken reported success against a dead broker")
	}

	if c := bc.currentClient(); c != nil && c.IsConnected() {
		t.Error("stale client left connected after a failed refresh; retryConnect's already-connected guard will return without dialling")
	}

	// With the stale client gone the retry loop can do its job, so it stays alive rather than
	// returning on its first guard check.
	waitFor(t, "retry loop to take ownership", 3*time.Second, bc.retrying.Load)

	close(bc.stop) // wakes the loop out of its backoff sleep
	waitFor(t, "retry loop to exit", 3*time.Second, func() bool { return !bc.retrying.Load() })
}
