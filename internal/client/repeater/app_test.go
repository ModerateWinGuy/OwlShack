package repeater

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/store"
)

// appRig is a client whose node has heard a stranger and holds a contact with no route, sending into a capture.
type appRig struct {
	rm                  *Client
	radio               captureRadio
	stranger, contact   meshcore.LocalIdentity
	secretS, secretC    []byte
	strangerH, contactH string
}

func newAppRig(t *testing.T) *appRig {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	comp := &store.Companion{Name: "test"}
	if err := st.Companions.Create(t.Context(), comp); err != nil {
		t.Fatal(err)
	}
	r := &appRig{radio: captureRadio{sent: make(chan []byte, 8)}, stranger: meshcore.NewLocalIdentityFromSeed([32]byte{0x51}), contact: meshcore.NewLocalIdentityFromSeed([32]byte{0x8d})}
	if err := st.Contacts.Add(t.Context(), comp.ID, r.contact.PublicKeyBytes(), "Rpt", "REPEATER"); err != nil {
		t.Fatal(err)
	}
	self := meshcore.NewLocalIdentityFromSeed([32]byte{1})
	n := node.New(self, r.radio)
	t.Cleanup(n.Stop)
	n.Peers().Insert(&node.Peer{Identity: r.stranger.Identity, Name: "Heard"})
	n.Peers().Insert(&node.Peer{Identity: r.contact.Identity, Name: "Rpt"})
	// OwlShack's own floods to this contact go unscoped, so a scoped one can only be the app's choice.
	r.rm = NewClient(n, st, comp.ID, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func() uint8 { return 1 }, func([]byte) *meshcore.Region { return nil })
	r.secretS, _ = r.stranger.SharedSecret(self.Identity)
	r.secretC, _ = r.contact.SharedSecret(self.Identity)
	r.strangerH, r.contactH = hex.EncodeToString(r.stranger.PublicKeyBytes()), hex.EncodeToString(r.contact.PublicKeyBytes())
	return r
}

func (r *appRig) next(t *testing.T) *meshcore.Packet {
	t.Helper()
	select {
	case raw := <-r.radio.sent:
		pkt, err := meshcore.PacketFromBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		return pkt
	case <-time.After(2 * time.Second):
		t.Fatal("nothing sent")
		return nil
	}
}

// sentFlood records whether the app was told its request flooded.
func sentFlood(flood *bool) func(uint32, time.Duration, bool) {
	return func(_ uint32, _ time.Duration, f bool) { *flood = f }
}

// An anonymous request to a node only heard goes zero-hop, as the firmware sends it; one to a contact with no route floods in the app's scope.
func TestAppAnonRequest_RoutedAsTheFirmware(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)
	wlg := meshcore.NewRegion("wlg")

	var flood bool
	r.rm.AppAnonRequest(r.strangerH, []byte{3}, time.Millisecond, AppSend{Scope: wlg, Sent: sentFlood(&flood)})
	if pkt := r.next(t); !pkt.IsRouteDirect() || pkt.PathLength != 0 || flood {
		t.Errorf("to a heard node: route %d, path length %#x, told flood %v; want zero-hop direct", pkt.RouteType(), pkt.PathLength, flood)
	}

	r.rm.AppAnonRequest(r.contactH, []byte{3}, time.Millisecond, AppSend{Scope: wlg, Sent: sentFlood(&flood)})
	if pkt := r.next(t); !pkt.IsRouteFlood() || !wlg.MatchesPacket(pkt) || !flood {
		t.Errorf("to an unrouted contact: route %d, in wlg %v, told flood %v; want a flood in wlg", pkt.RouteType(), wlg.MatchesPacket(pkt), flood)
	}
}

// Every flood the app sends to a contact goes in the app's scope, as the firmware's sendFloodScoped; OwlShack's own keep the contact's.
func TestAppFloodsTakeTheAppScope(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)
	wlg := meshcore.NewRegion("wlg")
	app := AppSend{Scope: wlg, Sent: func(uint32, time.Duration, bool) {}}

	for name, send := range map[string]func(){
		"login": func() { r.rm.AppLogin(r.contactH, "pw", nil, time.Millisecond, app) },
		"request": func() {
			r.rm.AppRequest(r.contactH, []byte{meshcore.ReqTypeGetStatus, 0, 0, 0, 0}, time.Millisecond, app)
		},
		"path discovery": func() { r.rm.AppPathDiscovery(r.contactH, time.Millisecond, app) },
		"CLI":            func() { r.rm.AppCLI(r.contactH, "ver", meshcore.TxtTypeCLIData, 0, app) },
	} {
		send()
		if pkt := r.next(t); !pkt.IsRouteFlood() || !wlg.MatchesPacket(pkt) {
			t.Errorf("app %s: route %d, in wlg %v; want a flood in wlg", name, pkt.RouteType(), wlg.MatchesPacket(pkt))
		}
	}
	r.rm.SendLogin(r.contactH, "pw", time.Millisecond)
	if pkt := r.next(t); pkt.IsTransport() {
		t.Error("OwlShack's own login took a scope the contact does not have")
	}
}

// The app's CLI goes out as the firmware's sendCommandData: no session, its own text type, no prefix of ours.
func TestAppCLINeedsNoSession(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)
	var flood bool
	var wait time.Duration
	if err := r.rm.AppCLI(r.contactH, "get name", meshcore.TxtTypeCLICommand, 2, AppSend{Sent: func(_ uint32, w time.Duration, f bool) { wait, flood = w, f }}); err != nil {
		t.Fatalf("AppCLI without a session: %v", err)
	}
	pkt := r.next(t)
	plain, err := meshcore.MACThenDecrypt(r.secretC, pkt.Payload[2:])
	if err != nil {
		t.Fatal(err)
	}
	if pkt.PayloadType() != meshcore.PayloadTypeTxtMsg || plain[4] != meshcore.TxtTypeCLICommand<<2|2 || string(bytes.TrimRight(plain[5:], "\x00")) != "get name" || !flood {
		t.Errorf("sent type %d flags %#x text %q flood %v", pkt.PayloadType(), plain[4], plain[5:], flood)
	}
	// The app is told the firmware's est_timeout for the command it sent (MyMesh.cpp:1176,1195), as sendCommandData reports it.
	if want := node.CalcFloodTimeout(0); wait != want {
		t.Errorf("app told to wait %v, want est_timeout %v", wait, want)
	}
}

// A password goes out cut to the firmware's 15 bytes, as its sendLogin cuts it.
func TestLoginPasswordCutTo15Bytes(t *testing.T) {
	t.Parallel()
	r := newAppRig(t)
	r.rm.SendLogin(r.contactH, "0123456789abcdefghij", time.Millisecond)
	req, err := meshcore.AnonReqFromBytes(r.next(t).Payload)
	if err != nil {
		t.Fatal(err)
	}
	plain := req.Decrypt(r.secretC)
	if got := string(bytes.TrimRight(plain[4:], "\x00")); got != "0123456789abcde" {
		t.Errorf("password sent as %q", got)
	}
}
