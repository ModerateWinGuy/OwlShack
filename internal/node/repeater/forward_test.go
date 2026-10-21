package repeater

import (
	"testing"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
)

// Pin the firmware's max_loop_* tables (examples/simple_repeater MyMesh.cpp); a wrong threshold silently changes on-air relay behaviour.
func TestLoopThreshold(t *testing.T) {
	cases := []struct {
		level string
		size  int
		want  int
	}{
		{"off", 1, 0},
		{"off", 2, 0},
		{"minimal", 1, 4},
		{"minimal", 2, 2},
		{"minimal", 3, 1},
		{"moderate", 1, 2},
		{"moderate", 2, 1},
		{"moderate", 3, 1},
		{"strict", 1, 1},
		{"strict", 2, 1},
		{"strict", 3, 1},
		{"minimal", 0, 0}, // out-of-range hash size → no check
		{"minimal", 4, 0},
		{"bogus", 1, 0}, // unknown level → no check
	}
	for _, c := range cases {
		if got := loopThreshold(c.level, c.size); got != c.want {
			t.Errorf("loopThreshold(%q, %d) = %d, want %d", c.level, c.size, got, c.want)
		}
	}
}

// allowForward gates floods on the firmware's recv_pkt_region (simple_repeater MyMesh.cpp:440,556), code 0 being the wildcard regardless.
func TestAllowForward_RegionGate(t *testing.T) {
	nz := meshcore.NewRegionFromHashtag("#nz")
	payload := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	flood := func(rt byte, code func(*meshcore.Packet) uint16) *meshcore.Packet {
		p := &meshcore.Packet{Header: meshcore.MakeHeader(rt, meshcore.PayloadTypeTxtMsg, 0), Payload: payload}
		if code != nil {
			p.TransportCode1 = code(p)
		}
		return p
	}
	plain := flood(meshcore.RouteTypeFlood, nil)
	scoped := flood(meshcore.RouteTypeTransportFlood, nz.CalcTransportCode)
	unknown := flood(meshcore.RouteTypeTransportFlood, meshcore.NewRegionFromHashtag("#au").CalcTransportCode)
	share := flood(meshcore.RouteTypeTransportFlood, func(*meshcore.Packet) uint16 { return 0 })

	for _, tc := range []struct {
		name     string
		denyStar bool
		pkt      *meshcore.Packet
		want     bool
	}{
		{"plain flood, * allows", false, plain, true},
		{"plain flood, * denies", true, plain, false},
		{"known region", true, scoped, true},
		{"unknown region", false, unknown, false},
		{"code 0 ignores * deny", true, share, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := telemetryRepeater(t, nil)
			r.node = node.New(meshcore.NewLocalIdentityFromSeed([32]byte{1}), &recordingRadio{}, node.WithRegions(nz))
			t.Cleanup(r.node.Stop)
			if tc.denyStar {
				r.node.Regions().SetWildcardFlags(meshcore.RegionDenyFlood)
			}
			if got := r.allowForward(tc.pkt); got != tc.want {
				t.Errorf("allowForward = %v, want %v", got, tc.want)
			}
		})
	}
}
