package companion

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/echo"
	"github.com/OwlShack/OwlShack/internal/store"
)

// westestModem fails every #westest post whose text says "fail", and keeps each post's frame by its text.
type westestModem struct {
	mu   sync.Mutex
	sent map[string][]byte
	rx   func([]byte, float32, int8, bool)
}

func (m *westestModem) SendData(b []byte) error {
	pkt, err := meshcore.PacketFromBytes(b)
	if err != nil || pkt.PayloadType() != meshcore.PayloadTypeGrpTxt {
		return nil
	}
	grp, err := meshcore.GroupTextFromBytes(pkt.Payload)
	if err != nil {
		return err
	}
	p, err := grp.DecryptStruct(meshcore.NewChannelFromHashtag("#westest").PSK)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.sent[p.Text] = append([]byte(nil), b...)
	m.mu.Unlock()
	if strings.Contains(p.Text, "fail") {
		return errors.New("kiss: tx failed")
	}
	return nil
}
func (m *westestModem) SetDataHandler(h func([]byte, float32, int8, bool)) {
	m.mu.Lock()
	m.rx = h
	m.mu.Unlock()
}
func (m *westestModem) AddOutboundHandler(func([]byte)) {}

// postGate holds the first n channel posts at the radio until all n have reached it, as posts sent at the same moment can arrive.
type postGate struct {
	mu  sync.Mutex
	n   int
	all chan struct{}
}

func (g *postGate) wait(d []byte) {
	if pkt, err := meshcore.PacketFromBytes(d); err != nil || pkt.PayloadType() != meshcore.PayloadTypeGrpTxt {
		return
	}
	g.mu.Lock()
	if g.n == 0 {
		g.mu.Unlock()
		return
	}
	if g.n--; g.n == 0 {
		close(g.all)
	}
	g.mu.Unlock()
	<-g.all
}

// gatedRadio is a companion's radio on the mux whose outbound handlers run only once the gate lets its post through.
type gatedRadio struct {
	node.MuxRadio
	gate     *postGate
	mu       sync.Mutex
	handlers []func([]byte)
}

func (g *gatedRadio) AddOutboundHandler(h func([]byte)) {
	g.mu.Lock()
	g.handlers = append(g.handlers, h)
	g.mu.Unlock()
}

func (g *gatedRadio) Enqueue(d []byte, p uint8, delay time.Duration) bool {
	g.gate.wait(d)
	g.mu.Lock()
	hs := g.handlers
	g.mu.Unlock()
	for _, h := range hs {
		h(d)
	}
	return g.MuxRadio.Enqueue(d, p, delay)
}

func gatedCompanion(t *testing.T, st *store.Store, tr *echo.Tracker, radio node.MuxRadio, name string, seed byte) *Companion {
	t.Helper()
	row := store.Companion{Name: name}
	var err error
	st.WriteSync(func() { err = st.Companions.Create(t.Context(), &row) })
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.CompanionConfig{ID: row.ID, Name: name}
	c := &Companion{runCtx: t.Context(), cfg: cfg, radio: &radio, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st, echoTracker: tr,
		chanScopes: map[string]config.FloodScope{}, floodScopeOf: func(*meshcore.Packet) string { return "everywhere" }}
	c.live.Store(&cfg)
	c.node = node.New(meshcore.NewLocalIdentityFromSeed([32]byte{seed}), radio)
	t.Cleanup(c.node.Stop)
	ch, err := channelFromRef(config.ChannelRef{Name: "#westest"})
	if err != nil {
		t.Fatal(err)
	}
	c.node.SetChannel(0, ch)
	c.registerPacketHandlers()
	return c
}

// Posts sent at once from two companions on one radio each settle from their own packet: one that failed is failed alone, and each echo counts for the post it repeats.
func TestConcurrentPostsSettleFromTheirOwnPacket(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "posts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	modem := &westestModem{sent: map[string][]byte{}}
	tr := echo.NewTracker(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := node.NewRadioMux(modem, node.WithMuxTxResult(tr.TxResult))
	t.Cleanup(mux.Stop)

	const each = 6
	gate := &postGate{n: 2 * each, all: make(chan struct{})}
	comps := map[string]*Companion{}
	for i, name := range []string{"home", "away"} {
		comps[name] = gatedCompanion(t, st, tr, &gatedRadio{MuxRadio: mux.NewRadio(), gate: gate}, name, byte(i+1))
	}

	var texts []string
	var wg sync.WaitGroup
	for name, c := range comps {
		for i := range each {
			text := fmt.Sprintf("%s echo %d", name, i)
			if i%2 == 1 {
				text = fmt.Sprintf("%s fail %d", name, i)
			}
			texts = append(texts, text)
			wg.Go(func() {
				if err := c.SendChannelMessage("#westest", text); err != nil {
					t.Errorf("%s: %v", text, err)
				}
			})
		}
	}
	wg.Wait()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		modem.mu.Lock()
		n := len(modem.sent)
		modem.mu.Unlock()
		if n == len(texts) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the radio sent %d of %d posts", n, len(texts))
		}
	}

	for _, text := range texts {
		if !strings.Contains(text, "echo") {
			continue
		}
		modem.mu.Lock()
		pkt, err := meshcore.PacketFromBytes(modem.sent[text])
		rx := modem.rx
		modem.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		pkt.PathLength, pkt.Path = meshcore.MakePathLen(1, 1), []byte{0x42}
		raw, _ := pkt.ToBytes()
		rx(raw, 5, -70, true)
	}

	time.Sleep(200 * time.Millisecond)
	st.WriteSync(func() {})
	for name, c := range comps {
		msgs, err := st.Messages.List(ctx, c.ID(), "#westest", 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, m := range msgs {
			if m.Direction != "tx" {
				continue
			}
			seen++
			failed := m.Status != nil && *m.Status == "failed"
			repeats := 0
			if m.RepeatCount != nil {
				repeats = *m.RepeatCount
			}
			if wantFail := strings.Contains(m.Text, "fail"); failed != wantFail {
				t.Errorf("%s: %q failed=%v, want %v", name, m.Text, failed, wantFail)
			}
			if wantRepeats := map[bool]int{true: 1}[strings.Contains(m.Text, "echo")]; repeats != wantRepeats {
				t.Errorf("%s: %q heard repeated %d times, want %d", name, m.Text, repeats, wantRepeats)
			}
		}
		if seen != each {
			t.Errorf("%s stored %d posts, want %d", name, seen, each)
		}
	}
}
