package app

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/store"
)

// seedChannels stores cfg as an import would and returns it read back, with the ids the runtime keys on.
func seedChannels(t *testing.T, st *store.Store, cfg *config.Config) *config.Config {
	t.Helper()
	ctx := t.Context()
	if _, err := initConfigTables(ctx, st); err != nil {
		t.Fatal(err)
	}
	cfg.Companions[0].ID = 0
	var err error
	st.WriteSync(func() { err = writeConfigToTables(ctx, st, cfg) })
	if err != nil {
		t.Fatal(err)
	}
	back, err := readConfigFromTables(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func storedChannels(t *testing.T, st *store.Store, companionID int64) map[string]store.CompanionChannel {
	t.Helper()
	rows, err := st.Channels.ListByCompanion(t.Context(), companionID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]store.CompanionChannel, len(rows))
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

// A runtime channel edit saves only what changed: every other channel and bot keeps its id, so an open Channels or Bots page still finds them.
func TestPersistChannels_KeepsUnchangedIDs(t *testing.T) {
	st, reload := reloadRig(t)
	ctx := t.Context()
	cfg := oneCompanion(func(c *config.CompanionConfig) {
		c.Triggers = &[]config.TriggerConfig{{Type: "group", Template: "pong", Channels: &config.ChannelList{{Name: "#westest"}}}}
	})
	before := seedChannels(t, st, cfg)
	id := before.Companions[0].ID
	running := reload(nil, before, nil)
	was := storedChannels(t, st, id)
	trigs, err := st.Triggers.ListByCompanion(ctx, id)
	if err != nil || len(trigs) != 1 {
		t.Fatalf("triggers %v, %v", trigs, err)
	}

	if err := running[0].AddChannel(config.ChannelRef{Name: "#owl"}); err != nil {
		t.Fatal(err)
	}
	if err := persistChannels(ctx, st, running, false); err != nil {
		t.Fatal(err)
	}
	if err := running[0].RenameChannel("Public", "Town"); err != nil {
		t.Fatal(err)
	}
	if err := persistChannels(ctx, st, running, false); err != nil {
		t.Fatal(err)
	}

	now := storedChannels(t, st, id)
	if now["#westest"].ID != was["#westest"].ID {
		t.Errorf("#westest id %d -> %d", was["#westest"].ID, now["#westest"].ID)
	}
	if owl, ok := now["#owl"]; !ok || *owl.Slot != 1 {
		t.Errorf("#owl = %+v, want it saved in slot 1", owl)
	}
	if town, ok := now["Town"]; !ok || *town.Slot != 0 || town.PrivateKey != hex.EncodeToString(meshcore.PublicChannel().PSK) || town.ID != was["Public"].ID {
		t.Errorf("Town = %+v, want slot 0 with Public's key and its id %d", town, was["Public"].ID)
	}
	if _, ok := now["Public"]; ok || len(now) != 3 {
		t.Errorf("stored %v, want Town, #owl and #westest", now)
	}
	after, err := st.Triggers.ListByCompanion(ctx, id)
	if err != nil || len(after) != 1 || after[0].ID != trigs[0].ID || len(after[0].ChannelIDs) != 1 || after[0].ChannelIDs[0] != was["#westest"].ID {
		t.Errorf("triggers %+v (%v), want bot %d still on channel %d", after, err, trigs[0].ID, was["#westest"].ID)
	}
}

// The app renames any channel by setting its slot to the same key, and the stored row keeps its id under each new name.
func TestPersistChannels_AnAppRenameKeepsTheID(t *testing.T) {
	st, reload := reloadRig(t)
	ctx := t.Context()
	before := seedChannels(t, st, oneCompanion(nil))
	id := before.Companions[0].ID
	running := reload(nil, before, nil)
	was := storedChannels(t, st, id)["#westest"]
	key := meshcore.NewChannelFromHashtag("#westest").PSK
	for _, name := range []string{"west", "west two"} {
		if err := running[0].AppSetChannel(*was.Slot, name, key); err != nil {
			t.Fatal(err)
		}
		if err := persistChannels(ctx, st, running, false); err != nil {
			t.Fatal(err)
		}
		now := storedChannels(t, st, id)
		if got, ok := now[name]; !ok || got.ID != was.ID || len(now) != 2 {
			t.Errorf("renamed to %q: stored %+v, want it under id %d beside Public", name, now, was.ID)
		}
	}
}

// A v1.5.0 companion could hold more channels than the node has slots: one past them waits for a free slot, and a runtime edit never deletes it or its key.
func TestPersistChannels_KeepsChannelsNotLoaded(t *testing.T) {
	st, reload := reloadRig(t)
	ctx := t.Context()
	extra := node.DefaultMaxChannels + 2
	ch := make(config.ChannelList, extra)
	for i := range ch {
		slot := i
		ch[i] = config.ChannelRef{Name: fmt.Sprintf("#c%d", i), Slot: &slot}
	}
	key := strings.Repeat("5a", 16)
	ch[extra-1] = config.ChannelRef{Name: "club", PrivateKey: key, Slot: ch[extra-1].Slot}
	cc := config.CompanionConfig{ID: 7, Name: "home", PrivateKey: strings.Repeat("11", 32), Channels: &ch}
	before := seedChannels(t, st, &config.Config{Companions: []config.CompanionConfig{cc}})
	id := before.Companions[0].ID
	running := reload(nil, before, nil)

	if err := running[0].RemoveChannel("#c5"); err != nil {
		t.Fatal(err)
	}
	if err := persistChannels(ctx, st, running, false); err != nil {
		t.Fatal(err)
	}
	now := storedChannels(t, st, id)
	if _, ok := now["#c5"]; ok || len(now) != extra-1 {
		t.Fatalf("stored %d channels, want %d without #c5", len(now), extra-1)
	}
	first := fmt.Sprintf("#c%d", node.DefaultMaxChannels)
	if _, ok := now[first]; !ok || now["club"].PrivateKey != key {
		t.Fatalf("lost a channel the node never loaded: %s present %v, club %+v", first, ok, now["club"])
	}

	// The freed slot takes the first channel waiting for one.
	after, err := readConfigFromTables(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	again := reload(before, after, running)
	if got := again[0].Node().Channel(5); got == nil || got.Name != first {
		t.Fatalf("slot 5 = %+v, want %s moved into it", got, first)
	}
	if err := persistChannels(ctx, st, again, false); err != nil {
		t.Fatal(err)
	}
	now = storedChannels(t, st, id)
	if *now[first].Slot != 5 || now["club"].PrivateKey != key || len(now) != extra-1 {
		t.Errorf("%s in slot %d, club %+v, %d stored; want slot 5, club kept with its key", first, *now[first].Slot, now["club"], len(now))
	}
}

// A channel the Channels page added just before a runtime edit is kept, and the runtime one never shares its slot, which would stop the next start.
func TestPersistChannels_NeverDoublesASlot(t *testing.T) {
	st, reload := reloadRig(t)
	ctx := t.Context()
	before := seedChannels(t, st, oneCompanion(nil))
	id := before.Companions[0].ID
	running := reload(nil, before, nil)
	var err error
	st.WriteSync(func() { err = st.Channels.Create(ctx, &store.CompanionChannel{CompanionID: id, Name: "#pending"}) })
	if err != nil {
		t.Fatal(err)
	}
	if err := running[0].AddChannel(config.ChannelRef{Name: "#owl"}); err != nil {
		t.Fatal(err)
	}
	if err := persistChannels(ctx, st, running, false); err != nil {
		t.Fatal(err)
	}
	now := storedChannels(t, st, id)
	if _, ok := now["#pending"]; !ok || *now["#pending"].Slot == *now["#owl"].Slot {
		t.Fatalf("stored %+v, want #pending kept and #owl in a slot of its own", now)
	}
	back, err := readConfigFromTables(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("the saved config no longer loads: %v", err)
	}
}
