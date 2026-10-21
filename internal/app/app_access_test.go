package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/api"
	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/modem"
	"github.com/OwlShack/OwlShack/internal/node/companion"
	"github.com/OwlShack/OwlShack/internal/store"
)

// App access is only ever set by its own endpoint, with both fields named, on a port in the range no other companion holds.
func TestSetCompanionApp(t *testing.T) {
	b, st := regionBackend(t)
	ctx := t.Context()
	one, err := b.SaveCompanion(ctx, api.CompanionInput{Name: "one", FloodScope: "inherit", ShareLocation: &share})
	if err != nil {
		t.Fatal(err)
	}
	two, err := b.SaveCompanion(ctx, api.CompanionInput{Name: "two", FloodScope: "inherit", ShareLocation: &share})
	if err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	port := func(p int) *int { return &p }
	size := port

	for name, in := range map[string]api.CompanionAppInput{
		"no enabled":      {Port: port(5000), QueueSize: size(256)},
		"no port":         {Enabled: &on, QueueSize: size(256)},
		"below the range": {Enabled: &on, Port: port(4999), QueueSize: size(256)},
		"above the range": {Enabled: &on, Port: port(5016), QueueSize: size(256)},
		"off, port 80":    {Enabled: &off, Port: port(80), QueueSize: size(256)},
	} {
		if err := b.SetCompanionApp(ctx, one, in); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
	for name, tc := range map[string]struct {
		in   api.CompanionAppInput
		want string
	}{
		"no queue size":      {api.CompanionAppInput{Enabled: &on, Port: port(5000)}, "enabled, port and queueSize are all required"},
		"queue of 15":        {api.CompanionAppInput{Enabled: &on, Port: port(5000), QueueSize: size(15)}, "the messages held for the app must be 16-10000"},
		"queue of 10001":     {api.CompanionAppInput{Enabled: &on, Port: port(5000), QueueSize: size(10001)}, "the messages held for the app must be 16-10000"},
		"off, queue of zero": {api.CompanionAppInput{Enabled: &off, Port: port(5000), QueueSize: size(0)}, "the messages held for the app must be 16-10000"},
	} {
		err := b.SetCompanionApp(ctx, one, tc.in)
		var invalid *api.ValidationError
		if !errors.As(err, &invalid) || err.Error() != tc.want {
			t.Errorf("%s: %v, want a 422 saying %q", name, err, tc.want)
		}
	}

	if err := b.SetCompanionApp(ctx, one, api.CompanionAppInput{Enabled: &on, Port: port(5000), QueueSize: size(1000)}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetCompanionApp(ctx, two, api.CompanionAppInput{Enabled: &on, Port: port(5000), QueueSize: size(256)}); err == nil {
		t.Error("a second companion was let in on 5000")
	}
	if err := b.SetCompanionApp(ctx, two, api.CompanionAppInput{Enabled: &off, Port: port(5000), QueueSize: size(256)}); err != nil {
		t.Errorf("a port kept while access is off may match another's: %v", err)
	}

	// Saving the companion's other settings leaves its access as it was.
	if _, err := b.SaveCompanion(ctx, api.CompanionInput{ID: one, Name: "one", FloodScope: "inherit", ShareLocation: &share}); err != nil {
		t.Fatal(err)
	}
	c, err := st.Companions.Get(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	if !c.AppEnabled || c.AppPort != 5000 || c.AppQueueSize != 1000 {
		t.Errorf("after a companion save: enabled %v port %d queue %d, want on at 5000 holding 1000", c.AppEnabled, c.AppPort, c.AppQueueSize)
	}
}

// share is the shareLocation every test companion is saved with.
var share = true

// The app's changes go through the same checks as a save from the UI: a rename onto another companion's name is refused.
func TestUpdateCompanionChecks(t *testing.T) {
	b, st := regionBackend(t)
	ctx := t.Context()
	one, err := b.SaveCompanion(ctx, api.CompanionInput{Name: "one", FloodScope: "inherit", ShareLocation: &share})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SaveCompanion(ctx, api.CompanionInput{Name: "two", FloodScope: "inherit", ShareLocation: &share}); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateCompanion(ctx, one, func(c *store.Companion) error { c.Name = "two"; return nil }); err == nil {
		t.Error("renamed onto another companion's name")
	}
	if err := b.UpdateCompanion(ctx, one, func(c *store.Companion) error { c.Name = "uno"; c.ShareLocation = false; return nil }); err != nil {
		t.Fatal(err)
	}
	c, _ := st.Companions.Get(ctx, one)
	if c.Name != "uno" || c.ShareLocation {
		t.Errorf("saved %q share %v", c.Name, c.ShareLocation)
	}
}

// The Channels page cannot add a channel past the node's table.
func TestSaveChannelRefusesPastTable(t *testing.T) {
	b, _ := regionBackend(t)
	ctx := t.Context()
	id, err := b.SaveCompanion(ctx, api.CompanionInput{Name: "one", FloodScope: "inherit", ShareLocation: &share})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 50; i++ { // Public is the first
		if _, err := b.SaveChannel(ctx, api.ChannelInput{CompanionID: id, Name: fmt.Sprintf("#c%d", i), FloodScope: "inherit"}); err != nil {
			t.Fatalf("channel %d: %v", i, err)
		}
	}
	if _, err := b.SaveChannel(ctx, api.ChannelInput{CompanionID: id, Name: "#one-too-many", FloodScope: "inherit"}); err == nil {
		t.Error("a 51st channel was saved")
	}
}

// A new queue size applies to the running port: the connected app stays, and the next message heard trims the queue to it.
func TestAppQueueSize_AppliesWithoutDroppingTheApp(t *testing.T) {
	st, reload := reloadRig(t)
	ctx := t.Context()
	if _, err := initConfigTables(ctx, st); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	seed := oneCompanion(func(c *config.CompanionConfig) {
		c.ID, c.App = 0, config.AppAccess{Enabled: true, Port: port, QueueSize: 256}
	})
	st.WriteSync(func() { err = writeConfigToTables(ctx, st, seed) })
	if err != nil {
		t.Fatal(err)
	}
	before, err := readConfigFromTables(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	running := reload(nil, before, nil)
	id := running[0].ID()
	ms, mux := &modem.State{Stats: &countingStats{}}, node.NewRadioMux(silentModem{})
	apps := newAppServers("127.0.0.1", st, nil)
	t.Cleanup(apps.stop)
	apps.sync(before, running, ms, mux)
	srv := apps.server(id)

	nc, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for i := 0; srv.Status().Client == ""; i++ {
		if i == 100 {
			t.Fatal("the app never connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	connected := srv.Status()
	pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeGrpTxt, 0)}
	for i := range 20 {
		srv.AppMessage(companion.AppMessage{Channel: 0, Text: fmt.Sprint("old ", i), Packet: pkt})
	}

	st.WriteSync(func() {
		var c *store.Companion
		if c, err = st.Companions.Get(ctx, id); err == nil {
			c.AppQueueSize = 16
			err = st.Companions.Update(ctx, c)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := readConfigFromTables(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	again := reload(before, after, running)
	if again[0] != running[0] {
		t.Fatal("the companion was rebuilt for a queue size")
	}
	apps.sync(after, again, ms, mux)
	if apps.server(id) != srv {
		t.Fatal("the app port was reopened for a queue size")
	}
	if now := srv.Status(); now.Client != connected.Client || !now.Since.Equal(connected.Since) {
		t.Errorf("connection %+v after the change, want %+v kept", now, connected)
	}
	nc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := io.Copy(io.Discard, nc); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the app's connection: %v, want it still open", err)
	}

	srv.AppMessage(companion.AppMessage{Channel: 0, Text: "new", Packet: pkt})
	var held [][]byte
	st.WriteSync(func() {
		for {
			f, ok, err := st.AppQueue.Pop(ctx, id)
			if err != nil || !ok {
				return
			}
			held = append(held, f)
		}
	})
	if len(held) != 16 || !bytes.HasSuffix(held[0], []byte("old 5")) || !bytes.HasSuffix(held[15], []byte("new")) {
		t.Errorf("held %d messages, want 16 from old 5 to the new one: %q", len(held), held)
	}
}
