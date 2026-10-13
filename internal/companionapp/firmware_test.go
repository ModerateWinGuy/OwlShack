package companionapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	mc "github.com/OwlShack/meshcore-go/companion"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/node/companion"
	"github.com/OwlShack/OwlShack/internal/store"
)

// contactOf makes a node the companion has heard and keeps as a contact, returning its identity.
func (r *rig) contactOf(t *testing.T, name, typ string) meshcore.LocalIdentity {
	t.Helper()
	id, err := meshcore.GenerateLocalIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}
	r.st.WriteSync(func() { err = r.st.Contacts.Add(context.Background(), r.id, id.PublicKeyBytes(), name, typ) })
	if err != nil {
		t.Fatal(err)
	}
	r.comp.Node().Peers().Insert(&node.Peer{Identity: id.Identity, Name: name, Type: typ})
	return id
}

func notType(payloadType byte) func(*meshcore.Packet) bool {
	return func(p *meshcore.Packet) bool { return p.PayloadType() != payloadType }
}

// opened is what a peer reads of a text message the companion sent it.
func (r *rig) opened(t *testing.T, peer meshcore.LocalIdentity, pkt *meshcore.Packet) []byte {
	t.Helper()
	secret, err := peer.SharedSecret(r.comp.Node().Identity().Identity)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := meshcore.MACThenDecrypt(secret, pkt.Payload[2:])
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

// An OK to a settings write means the change is running: the app reads it straight back, and the advert it exports carries it.
func TestSettingsOKOnceLive(t *testing.T) {
	r := newRig(t)
	r.saved.lag = 300 * time.Millisecond
	a := r.dial(t)
	a.ask([]byte{mc.RespOk}, append([]byte{mc.CmdSetAdvertName}, "away"...)...)
	a.send(append([]byte{mc.CmdAppStart}, make([]byte, 7)...)...)
	got, err := mc.ParseResponse(a.next())
	if err != nil {
		t.Fatal(err)
	}
	if si, ok := got.Data.(mc.SelfInfoResponse); !ok || si.Name != "away" {
		t.Errorf("self info just after the OK = %+v, want the new name", got.Data)
	}
	a.ask([]byte{mc.RespOk}, mc.CmdSetOtherParams, 1, 0, 0) // stop sharing the position
	a.send(mc.CmdExportContact)
	pkt, err := meshcore.PacketFromBytes(a.next()[1:])
	if err != nil {
		t.Fatal(err)
	}
	adv, err := meshcore.AdvertFromBytes(pkt.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if adv.AppData().HasLocation || adv.AppData().Name != "away" {
		t.Errorf("advert just after the OK: %q, position %v", adv.AppData().Name, adv.AppData().HasLocation)
	}
}

// Every change to a contact reaches an app syncing since its last sync, as the firmware bumps lastmod on each; the web UI's edits included.
func TestIncrementalContactsSeeEdits(t *testing.T) {
	r := newRig(t)
	bg := context.Background()
	pk := bytes.Repeat([]byte{7}, 32)
	cl := r.client(t)
	since := uint32(time.Now().Unix()) - 60
	for name, edit := range map[string]func() error{
		"star": func() error {
			return r.st.Contacts.UpdateMetadata(bg, r.id, pk, store.ContactMetadata{Favourite: true})
		},
		"rename":   func() error { return r.st.Contacts.SetName(bg, r.id, pk, "Owl") },
		"reset":    func() error { return r.st.Contacts.UpdateOutPath(bg, r.id, pk, nil, 0) },
		"route":    func() error { return r.st.Contacts.SetRoute(bg, r.id, pk, []byte{0xaa}, 1) },
		"region":   func() error { return r.st.Contacts.SetFloodScope(bg, r.id, pk, "region:nz") },
		"position": func() error { return r.st.Contacts.SetLocation(bg, r.id, pk, -41000000, 174000000) },
		"add": func() error {
			if err := r.st.Contacts.Delete(bg, r.id, pk); err != nil {
				return err
			}
			if err := r.st.Contacts.Add(bg, r.id, pk, "Owly", "REPEATER"); err != nil {
				return err
			}
			return r.st.Contacts.RefreshFromAdvert(bg, pk, "Owly", "REPEATER", 0, 0, 0, 0, time.Now().Add(-time.Hour), 0, false)
		},
	} {
		var err error
		r.st.WriteSync(func() {
			err = r.st.Contacts.Restore(bg, &store.Contact{CompanionID: r.id, PeerPubKey: pk, Name: "Owly", Type: "REPEATER", OutPath: []byte{0xbb}, OutPathHashSize: 1, PathHashSize: 1, AddedAt: time.Now().Add(-time.Hour)})
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, _, err := cl.GetContactsSince(ctx(t), since, true); err != nil || len(got) != 0 {
			t.Fatalf("%s: before the edit, %d contacts since a minute ago (%v); want none", name, len(got), err)
		}
		r.st.WriteSync(func() { err = edit() })
		if err != nil {
			t.Fatal(err)
		}
		if got, _, err := cl.GetContactsSince(ctx(t), since, true); err != nil || len(got) != 1 {
			t.Errorf("%s: %d contacts since a minute ago (%v); want the one edited", name, len(got), err)
		}
	}
}

func addContactFrame(pub []byte, name string, lastAdvert uint32) []byte {
	b := append([]byte{mc.CmdAddUpdateContact}, pub...)
	b = append(b, meshcore.AdvertTypeChat, 0, 0xFF)
	b = append(b, make([]byte, 64)...)
	return le32(fixed(b, name, 32), lastAdvert)
}

// A heard node added from the app keeps the advert heard, as the REST add does; one never heard takes the frame's, as the firmware does. Only a heard one has an advert path.
func TestAddContactLastAdvertAndAdvertPath(t *testing.T) {
	r := newRig(t)
	heard, _ := meshcore.GenerateLocalIdentity(nil)
	far, _ := meshcore.GenerateLocalIdentity(nil)
	r.st.WriteSync(func() {
		r.st.Peers.Upsert(context.Background(), &store.Peer{PubKey: heard.PublicKeyBytes(), Name: "Heard", Type: "CHAT", OutPath: []byte{0x12}, OutPathHashSize: 1, LastAdvertTS: 1700000000, LastSeen: time.Now()})
	})
	a := r.dial(t)
	a.ask([]byte{mc.RespOk}, addContactFrame(heard.PublicKeyBytes(), "Heard", 0)...)
	a.ask([]byte{mc.RespOk}, addContactFrame(far.PublicKeyBytes(), "Far", 1700000123)...)
	for name, want := range map[string]struct {
		pub []byte
		ts  uint32
	}{"heard": {heard.PublicKeyBytes(), 1700000000}, "far": {far.PublicKeyBytes(), 1700000123}} {
		if k, _ := r.srv.contact(want.pub); k == nil || k.LastAdvertTS != want.ts {
			t.Errorf("%s: contact %+v, want last advert %d", name, k, want.ts)
		}
	}
	a.ask([]byte{mc.RespErr, mc.ErrCodeNotFound}, append([]byte{mc.CmdGetAdvertPath, 0}, far.PublicKeyBytes()...)...)
	a.send(append([]byte{mc.CmdGetAdvertPath, 0}, heard.PublicKeyBytes()...)...)
	if f := a.next(); !bytes.Equal(f[5:], []byte{1, 0x12}) {
		t.Errorf("heard node's advert path = % x", f)
	}
}

// The app's send scope covers every flood to a contact, as the firmware's sendFloodScoped, and may be any key: a region OwlShack does not list goes out with exactly that key.
func TestAppSendScope(t *testing.T) {
	r := newRig(t)
	a := r.dial(t)
	rpt := r.contactOf(t, "Rpt", "REPEATER")
	nz, zz := meshcore.NewRegion("nz"), meshcore.NewRegion("zz")

	a.ask([]byte{mc.RespOk}, append([]byte{mc.CmdSetFloodScopeKey, 0}, nz.Key[:]...)...)
	a.send(append(append([]byte{mc.CmdSendLogin}, rpt.PublicKeyBytes()...), "pw"...)...)
	if f := a.next(); f[0] != mc.RespSent {
		t.Fatalf("login answered % x", f)
	}
	if pkt := r.modem.next(t, notType(meshcore.PayloadTypeAnonReq)); !nz.MatchesPacket(pkt) {
		t.Error("the app's login did not go in the app's scope")
	}

	a.ask([]byte{mc.RespOk}, append([]byte{mc.CmdSetFloodScopeKey, 0}, zz.Key[:]...)...)
	a.ask([]byte{mc.RespOk}, append(le32([]byte{mc.CmdSendChannelTxtMsg, 0, 0}, uint32(time.Now().Unix())), "hi all"...)...)
	if pkt := r.modem.next(t, notType(meshcore.PayloadTypeGrpTxt)); !zz.MatchesPacket(pkt) {
		t.Error("the post did not go out with the app's key")
	}
	r.st.WriteSync(func() {})
	if m, _ := r.st.Messages.List(context.Background(), r.id, "Public", 1, 0); len(m) != 1 || m[0].FloodScope != config.ScopeUnknown {
		t.Errorf("stored post = %+v, want it labelled with an unknown region", m)
	}
}

// The app's CLI needs no login of ours, as the firmware just sends it, with the app's own text type; the reply reaches the app whenever it comes.
func TestAppCLIWithoutLogin(t *testing.T) {
	r := newRig(t)
	rpt := r.contactOf(t, "Rpt", "REPEATER")
	a := r.dial(t)
	a.send(mc.CmdDeviceQuery, 3)
	a.next()
	a.send(append(le32([]byte{mc.CmdSendTxtMsg, meshcore.TxtTypeCLICommand, 0}, uint32(time.Now().Unix())), append(rpt.PublicKeyBytes()[:6], "get name"...)...)...)
	if f := a.next(); f[0] != mc.RespSent {
		t.Fatalf("CLI answered % x, want it sent", f)
	}
	plain := r.opened(t, rpt, r.modem.next(t, notType(meshcore.PayloadTypeTxtMsg)))
	if plain[4]>>2 != meshcore.TxtTypeCLICommand || string(bytes.TrimRight(plain[5:], "\x00")) != "get name" {
		t.Errorf("sent flags %#x text %q", plain[4], plain[5:])
	}

	secret, _ := rpt.SharedSecret(r.comp.Node().Identity().Identity)
	msg, err := meshcore.NewTextMessage(rpt, r.comp.Node().Identity().Identity, meshcore.BuildTextPlaintext(time.Unix(1700000500, 0), meshcore.TxtTypeCLIData<<2, []byte("> home")), secret)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := msg.ToBytes()
	r.modem.hear(t, &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTxtMsg, 0), Payload: payload})
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		a.send(mc.CmdSyncNextMessage)
		f := a.next()
		if f[0] == mc.RespNoMoreMessages {
			continue
		}
		got, err := mc.ParseResponse(f)
		m, ok := got.Data.(mc.ContactMsgRecvV3Response)
		if err != nil || !ok || m.TxtType != meshcore.TxtTypeCLIData || m.Text != "> home" || m.SenderTimestamp != 1700000500 || !bytes.Equal(m.PubKeyPrefix[:], rpt.PublicKeyBytes()[:6]) {
			t.Errorf("reply reached the app as %+v, %v", got.Data, err)
		}
		return
	}
	t.Error("the CLI reply never reached the app")
}

// A retry the app sends after ours are over keeps the first timestamp, so the receiver keeps one message, and goes as an attempt relays have not seen.
func TestLateAppRetryKeepsTimestamp(t *testing.T) {
	r := newRig(t)
	bob := r.contactOf(t, "Bob", "CHAT")
	a := r.dial(t)
	ts := uint32(time.Now().Unix())
	send := func(attempt byte) {
		a.send(append(le32([]byte{mc.CmdSendTxtMsg, meshcore.TxtTypePlain, attempt}, ts), append(bob.PublicKeyBytes()[:6], "hello bob"...)...)...)
		if f := a.next(); f[0] != mc.RespSent {
			t.Fatalf("attempt %d answered % x", attempt, f)
		}
	}
	send(0)
	firstPkt := r.modem.next(t, notType(meshcore.PayloadTypeTxtMsg))
	first := r.opened(t, bob, firstPkt)
	self := r.comp.Node().Identity()
	crc := meshcore.CalcAckHash(first[:5+len("hello bob")], self.PublicKeyBytes())
	r.modem.hear(t, &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeAck, 0), Payload: le32(nil, crc)})
	for f := a.frame(); f[0] != mc.PushSendConfirmed; f = a.frame() {
	}

	send(1)
	againPkt := r.modem.next(t, notType(meshcore.PayloadTypeTxtMsg))
	again := r.opened(t, bob, againPkt)
	if !bytes.Equal(again[:4], first[:4]) {
		t.Errorf("the retry went with timestamp % x, the first with % x", again[:4], first[:4])
	}
	if bytes.Equal(againPkt.Payload, firstPkt.Payload) {
		t.Error("the retry is the first attempt again, which relays drop as seen")
	}
}

// An app retry after a rebuild goes through the rebuilt companion, not the stopped one its first attempt went through.
func TestAppRetryAfterRebuildUsesTheNewCompanion(t *testing.T) {
	r := newRig(t)
	bob := r.contactOf(t, "Bob", "CHAT")
	a := r.dial(t)
	ts := uint32(time.Now().Unix())
	send := func(attempt byte) {
		a.send(append(le32([]byte{mc.CmdSendTxtMsg, meshcore.TxtTypePlain, attempt}, ts), append(bob.PublicKeyBytes()[:6], "hello bob"...)...)...)
		if f := a.next(); f[0] != mc.RespSent {
			t.Fatalf("attempt %d answered % x", attempt, f)
		}
	}
	send(0)
	r.modem.next(t, notType(meshcore.PayloadTypeTxtMsg))

	channels := config.ChannelList{{Name: "Public"}}
	modem := &tapModem{sent: make(chan []byte, 64)}
	rebuilt, err := companion.NewCompanion(config.CompanionConfig{ID: r.id, Name: "home", PrivateKey: strings.Repeat("11", 32), Channels: &channels},
		node.NewRadioMux(modem), r.st, nil, nil, nil, func(*meshcore.Packet) string { return "everywhere" })
	if err != nil {
		t.Fatal(err)
	}
	rebuilt.Node().Peers().Insert(&node.Peer{Identity: bob.Identity, Name: "Bob", Type: "CHAT"})
	r.srv.Use(rebuilt)

	send(1)
	modem.next(t, notType(meshcore.PayloadTypeTxtMsg))
}

// A channel post that cannot be sent is the firmware's NOT_FOUND, as its sendGroupMessage failing is.
func TestChannelSendFailureCode(t *testing.T) {
	r := newRig(t, node.WithMuxMaxTxQueue(1))
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	r.modem.mu.Lock()
	r.modem.stuck = stuck
	r.modem.mu.Unlock()
	a := r.dial(t)
	for range 4 {
		a.send(append(le32([]byte{mc.CmdSendChannelTxtMsg, 0, 0}, 1), "hi"...)...)
		if f := a.next(); f[0] != mc.RespOk {
			if !bytes.Equal(f, []byte{mc.RespErr, mc.ErrCodeNotFound}) {
				t.Errorf("a post the radio would not take answered % x", f)
			}
			return
		}
	}
	t.Fatal("precondition: every post was taken, so none could fail")
}

// A trace at the firmware's 8-byte hashes goes out, as its v1.11 path_sz 3 does.
func TestTraceEightByteHashes(t *testing.T) {
	r := newRig(t)
	a := r.dial(t)
	path := bytes.Repeat([]byte{0xab}, 16)
	a.send(append(le32(le32([]byte{mc.CmdSendTracePath}, 77), 88), append([]byte{3}, path...)...)...)
	if f := a.next(); f[0] != mc.RespSent {
		t.Fatalf("8-byte trace answered % x", f)
	}
	tr, err := meshcore.TraceFromBytes(r.modem.next(t, notType(meshcore.PayloadTypeTrace)).Payload)
	if err != nil || tr.Flags&3 != 3 || tr.Tag != 77 || !bytes.Equal(tr.PathHashes, path) {
		t.Errorf("trace sent = %+v, %v", tr, err)
	}
}

// A rebuild with a new key drops the app, which would otherwise go on with the old identity, as the firmware reboots on one.
func TestNewKeyDropsApp(t *testing.T) {
	r := newRig(t)
	a := r.dial(t)
	a.send(mc.CmdGetDeviceTime)
	a.next()
	channels := config.ChannelList{{Name: "Public"}}
	rekeyed, err := companion.NewCompanion(config.CompanionConfig{ID: r.id, Name: "home", PrivateKey: strings.Repeat("22", 32), Channels: &channels},
		node.NewRadioMux(silentModem{}), r.st, nil, nil, nil, func(*meshcore.Packet) string { return "everywhere" })
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Use(rekeyed)
	a.nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := a.nc.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("read after the new key: %v, want the app dropped", err)
	}
}

// A DM's ACK is sha256 of [timestamp][flags][text] and the sender's key, the extended attempt and a random byte (BaseChatMesh.cpp:241-248); a room push's is the bare hash keyed by ours (:290).
func TestReceivedTextAcks_FirmwareLayout(t *testing.T) {
	r := newRig(t)
	bob := r.contactOf(t, "Bob", "CHAT")
	room := r.contactOf(t, "Room", "ROOM")
	self := r.comp.Node().Identity()
	hear := func(from meshcore.LocalIdentity, plain []byte) []byte {
		t.Helper()
		secret, err := from.SharedSecret(self.Identity)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := meshcore.NewTextMessage(from, self.Identity, plain, secret)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := msg.ToBytes()
		r.modem.hear(t, &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTxtMsg, 0), Payload: payload})
		return r.modem.next(t, notType(meshcore.PayloadTypeAck)).Payload
	}
	hash := func(data, pub []byte) []byte {
		h := sha256.Sum256(append(append([]byte(nil), data...), pub...))
		return h[:4]
	}

	// Attempt 5 rides as 1 in the flags and in full after the text's terminator.
	dm := append(le32(nil, 1700000000), 1)
	dm = append(append(dm, "hi bob"...), 0, 5)
	if ack := hear(bob, dm); len(ack) != 6 || !bytes.Equal(ack[:4], hash(dm[:11], bob.PublicKeyBytes())) || ack[4] != 5 {
		t.Errorf("DM ack = % x, want hash % x then attempt 05", ack, hash(dm[:11], bob.PublicKeyBytes()))
	}
	post := append(le32(nil, 1700000001), meshcore.TextFlags(meshcore.TxtTypeSignedPlain)|2, 1, 2, 3, 4)
	post = append(post, "a post"...)
	if ack := hear(room, post); !bytes.Equal(ack, hash(post, self.PublicKeyBytes())) {
		t.Errorf("room push ack = % x, want % x", ack, hash(post, self.PublicKeyBytes()))
	}
}

// The app's status and telemetry requests carry the firmware's 9-byte body with its random tail (BaseChatMesh.cpp:690-698).
func TestAppRequestBodiesHaveRandomTail(t *testing.T) {
	r := newRig(t)
	rpt := r.contactOf(t, "Rpt", "REPEATER")
	a := r.dial(t)
	for _, tc := range []struct {
		reqType byte
		frame   []byte
	}{
		{meshcore.ReqTypeGetStatus, append([]byte{mc.CmdSendStatusReq}, rpt.PublicKeyBytes()...)},
		{meshcore.ReqTypeGetTelemetryData, append([]byte{mc.CmdSendTelemetryReq, 0, 0, 0}, rpt.PublicKeyBytes()...)},
	} {
		var tails [][]byte
		for range 2 {
			a.send(tc.frame...)
			if f := a.next(); f[0] != mc.RespSent {
				t.Fatalf("request %d answered % x", tc.reqType, f)
			}
			body := r.opened(t, rpt, r.modem.next(t, notType(meshcore.PayloadTypeReq)))[4:]
			if !bytes.Equal(body[:5], []byte{tc.reqType, 0, 0, 0, 0}) || !bytes.Equal(body[9:], make([]byte, len(body)-9)) {
				t.Errorf("request %d body = % x, want type, four zeros, four random", tc.reqType, body)
			}
			tails = append(tails, body[5:9])
		}
		if bytes.Equal(tails[0], tails[1]) {
			t.Errorf("request %d: two sends carried the same tail % x", tc.reqType, tails[0])
		}
	}
}
