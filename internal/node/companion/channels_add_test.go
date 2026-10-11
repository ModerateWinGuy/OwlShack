package companion

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	meshcore "github.com/OwlShack/meshcore-go"

	"github.com/OwlShack/OwlShack/internal/api"
	"github.com/OwlShack/OwlShack/internal/config"
)

// A key the stored config would refuse at the next start must be refused here, or OwlShack saves it and won't boot.
func TestAddChannel_RefusesWhatConfigRefuses(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	var verr *api.ValidationError
	if err := c.AddChannel(config.ChannelRef{Name: "Long", PrivateKey: strings.Repeat("ab", 32)}); !errors.As(err, &verr) {
		t.Fatalf("32-byte key: err = %v, want a validation error", err)
	}
	if c.findChannel("Long") != nil {
		t.Error("the refused channel was added")
	}
	if err := c.AddChannel(config.ChannelRef{Name: "Ok", PrivateKey: strings.Repeat("ab", 16)}); err != nil {
		t.Fatalf("16-byte key: %v", err)
	}
}

// A blank or space-padded name is refused rather than becoming a hashtag channel of spaces.
func TestAddChannel_RefusesABlankOrPaddedName(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	for _, name := range []string{"   ", " #padded "} {
		var verr *api.ValidationError
		if err := c.AddChannel(config.ChannelRef{Name: name}); !errors.As(err, &verr) {
			t.Errorf("name %q: err = %v, want a validation error", name, err)
		}
	}
}

// Only Public is renamed, as the thread menu offers; a hashtag channel's name is its key.
func TestRenameChannel_OnlyPublic(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	for i, ref := range []config.ChannelRef{{Name: "Public"}, {Name: "#jokes"}} {
		ch, err := channelFromRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		c.node.SetChannel(i, ch)
	}
	var verr *api.ValidationError
	if err := c.RenameChannel("#jokes", "#other"); !errors.As(err, &verr) {
		t.Errorf("renaming a hashtag channel: err = %v, want a validation error", err)
	}
	if err := c.RenameChannel("Public", "  "); !errors.As(err, &verr) {
		t.Errorf("renaming to a blank name: err = %v, want a validation error", err)
	}
	if err := c.RenameChannel("Public", "General"); err != nil {
		t.Fatalf("renaming Public: %v", err)
	}
	if err := c.RenameChannel("General", "Public"); err != nil {
		t.Errorf("renaming the renamed Public back: %v", err)
	}
}

// Renaming a channel that is not there is not found, even onto a name that is taken.
func TestRenameChannel_MissingIsNotFound(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	ch, err := channelFromRef(config.ChannelRef{Name: "#test"})
	if err != nil {
		t.Fatal(err)
	}
	c.node.SetChannel(0, ch)
	var serr *api.StatusError
	if err := c.RenameChannel("#test2", "#test"); !errors.As(err, &serr) || serr.Status != http.StatusNotFound {
		t.Errorf("renaming a missing channel onto #test: err = %v, want 404 not found", err)
	}
}

// "#test256" shares Public's one-byte channel hash (0x11), so only the whole key tells them apart.
func TestChannels_PublicIsKnownByItsKey(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	for i, ref := range []config.ChannelRef{{Name: "Public"}, {Name: "#test256"}} {
		ch, err := channelFromRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		c.node.SetChannel(i, ch)
	}
	var verr *api.ValidationError
	if err := c.RenameChannel("#test256", "General"); !errors.As(err, &verr) {
		t.Errorf("renaming #test256: err = %v, want a validation error", err)
	}
	c.node.SetChannel(1, nil)
	if err := c.RenameChannel("Public", "#test256"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range c.StandaloneChannels() {
		if ref.Name == "#test256" && ref.PrivateKey == "" {
			t.Error("Public renamed #test256 is saved without its key, so it comes back as a different channel")
		}
	}
}

// A rename puts a new entry in the slot: readers hold the old one without a lock, so writing its name in place races them.
func TestRenameChannel_ReplacesTheEntry(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	pub, err := channelFromRef(config.ChannelRef{Name: "Public"})
	if err != nil {
		t.Fatal(err)
	}
	c.node.SetChannel(0, pub)
	if err := c.RenameChannel("Public", "Town"); err != nil {
		t.Fatal(err)
	}
	if got := c.node.Channel(0); pub.Name != "Public" || got.Name != "Town" || !bytes.Equal(got.PSK, pub.PSK) {
		t.Fatalf("held entry %q, slot 0 %+v: want the held one untouched and Town with Public's key in the slot", pub.Name, got)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			_ = c.ChannelSlots()
		}
	})
	for i := range 100 {
		name := []string{"Public", "Town"}[i%2]
		if err := c.AppSetChannel(0, name, meshcore.PublicChannel().PSK); err != nil {
			t.Fatalf("rename to %s: %v", name, err)
		}
	}
	wg.Wait()
}

// The MeshCore app renames any channel by setting its slot to the same key under a new name, as the firmware allows.
func TestAppSetChannel_RenamesAPrivateChannel(t *testing.T) {
	c, _, _, _ := scopeCompanion(t)
	key := bytes.Repeat([]byte{0x5a}, 16)
	if err := c.AppSetChannel(3, "Club", key); err != nil {
		t.Fatal(err)
	}
	if err := c.AppSetChannel(3, "Club 2", key); err != nil {
		t.Fatalf("renaming a private channel from the app: %v", err)
	}
	if ch := c.node.Channel(3); ch == nil || ch.Name != "Club 2" || !bytes.Equal(ch.PSK, key) {
		t.Errorf("slot 3 = %+v, want Club 2 with its key", ch)
	}
	var verr *api.ValidationError
	if err := c.RenameChannel("Club 2", "Club 3"); !errors.As(err, &verr) {
		t.Errorf("the thread menu renaming a private channel: err = %v, want a validation error", err)
	}
}
