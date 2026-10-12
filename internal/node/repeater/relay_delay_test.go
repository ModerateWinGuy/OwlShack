package repeater

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/store"
	"github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/hardware"
	"github.com/OwlShack/meshcore-go/node"
)

// relayModem hands the mux received packets, notes when each transmit leaves, and knows its airtime as every real modem does.
type relayModem struct {
	node.AirtimeSource
	mu      sync.Mutex
	handler func([]byte, float32, int8, bool)
	sent    []time.Time
}

func (m *relayModem) SendData([]byte) error {
	m.mu.Lock()
	m.sent = append(m.sent, time.Now())
	m.mu.Unlock()
	return nil
}
func (m *relayModem) SetDataHandler(h func([]byte, float32, int8, bool)) {
	m.mu.Lock()
	m.handler = h
	m.mu.Unlock()
}
func (m *relayModem) AddOutboundHandler(func([]byte)) {}

type estAirtime func(int) uint32

func (e estAirtime) EstAirtime(n int) uint32 { return e(n) }

// Firmware waits rand(0..4) slots of the packet's airtime before relaying a flood; with no airtime every relay went out at once.
func TestRelay_WaitsItsAirtimeSlots(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	freq, bw, sf, cr := 917.375, 62.5, 7, 5
	if err := st.Settings.Set(ctx, &store.Settings{ConnectionType: "kiss", PacketRetentionDays: 7, Freq: &freq, BW: &bw, SF: &sf, CR: &cr}); err != nil {
		t.Fatal(err)
	}
	m := &relayModem{AirtimeSource: estAirtime(hardware.LoRaAirtimeEstimator(&hardware.RadioConfig{FreqHz: 917_375_000, BwHz: 62_500, SF: 7, CR: 5}))}
	mux := node.NewRadioMux(m)
	t.Cleanup(mux.Stop)
	r, err := NewRepeater(config.RepeaterConfig{Name: "rp", PrivateKey: strings.Repeat("22", 32), AdminPassword: "pw"}, mux, st, nil, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop() })

	const floods = 12
	var heard []time.Time
	for range floods {
		payload := make([]byte, 40)
		rand.Read(payload)
		raw, err := (&meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeGrpTxt, 0), PathLength: meshcore.MakePathLen(1, 0), Payload: payload}).ToBytes()
		if err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		h := m.handler
		m.mu.Unlock()
		heard = append(heard, time.Now())
		h(raw, 10, -60, true)
		time.Sleep(600 * time.Millisecond)
	}
	m.mu.Lock()
	sent := append([]time.Time(nil), m.sent...)
	m.mu.Unlock()
	if len(sent) < floods {
		t.Fatalf("relayed %d of %d floods", len(sent), floods)
	}
	// The engine adds a fixed latency to every send, so only the spread shows the random slots.
	lo, hi := time.Hour, time.Duration(0)
	for i := range floods {
		d := sent[i].Sub(heard[i])
		lo, hi = min(lo, d), max(hi, d)
	}
	if hi-lo < 30*time.Millisecond {
		t.Errorf("relays waited %v to %v: no relay waited its random airtime slots", lo, hi)
	}
}
