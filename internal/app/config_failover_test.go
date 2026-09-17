package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcore-go/OwlShack/internal/config"
	"github.com/meshcore-go/OwlShack/internal/store"
)

func TestFailoverConfigRoundTrip(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	triggers := []config.TriggerConfig{{Type: "group", Template: "reply", Channels: &config.ChannelList{{Name: "Public"}},
		FailoverPattern: `^@\[{{.Sender | reQuote}}\].+`, FailoverTimeout: 10}}
	cfg := &config.Config{Companions: []config.CompanionConfig{{Name: "backup", PrivateKey: strings.Repeat("01", 32), Triggers: &triggers}}}
	for _, enabled := range []bool{true, false} {
		if !enabled {
			triggers[0].FailoverPattern = ""
			triggers[0].FailoverTimeout = 0
		}
		st.WriteSync(func() { err = writeConfigToTables(t.Context(), st, cfg) })
		if err != nil {
			t.Fatal(err)
		}
		got, err := readConfigFromTables(t.Context(), st)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Companions) != 1 || got.Companions[0].Triggers == nil || len(*got.Companions[0].Triggers) != 1 {
			t.Fatalf("lost trigger: %+v", got.Companions)
		}
		actual := (*got.Companions[0].Triggers)[0]
		if actual.FailoverPattern != triggers[0].FailoverPattern || actual.FailoverTimeout != triggers[0].FailoverTimeout {
			t.Fatalf("failover did not survive config round trip: %+v", actual)
		}
	}
}
