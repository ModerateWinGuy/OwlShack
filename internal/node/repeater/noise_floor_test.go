package repeater

import (
	"crypto/rand"
	"encoding/binary"
	"testing"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
)

type stubRadio struct{}

func (stubRadio) SendData([]byte) error                               { return nil }
func (stubRadio) SetDataHandler(func(*meshcore.Packet))               {}
func (stubRadio) SetRawDataHandler(func([]byte, float32, int8, bool)) {}
func (stubRadio) AddOutboundHandler(func([]byte))                     {}
func (stubRadio) Close() error                                        { return nil }

// testStatsRepeater has the node Stats reads its key and queue from.
func testStatsRepeater(t *testing.T) *Repeater {
	t.Helper()
	id, err := meshcore.GenerateLocalIdentity(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n := node.New(id, stubRadio{})
	t.Cleanup(n.Stop)
	return &Repeater{node: n}
}

// An unmeasured noise floor is absent from our stats, while the STATUS reply carries 0 as firmware does.
func TestNoiseFloor_UnmeasuredIsAbsent(t *testing.T) {
	r := testStatsRepeater(t)
	ds := DeviceStats{NoiseFloor: -104, BatteryMV: 4100, HaveBattery: true}
	r.boardStats = func() DeviceStats { return ds }
	if nf := r.Stats().NoiseFloor; nf != nil {
		t.Errorf("stats noise floor = %d before a measurement, want none", *nf)
	}
	if got := int16(binary.LittleEndian.Uint16(r.statusBody()[4:6])); got != 0 {
		t.Errorf("STATUS noise floor = %d before a measurement, want 0", got)
	}

	ds = DeviceStats{NoiseFloor: -104, HaveNoiseFloor: true}
	if nf := r.Stats().NoiseFloor; nf == nil || *nf != -104 {
		t.Errorf("stats noise floor = %v, want -104", nf)
	}
	if got := int16(binary.LittleEndian.Uint16(r.statusBody()[4:6])); got != -104 {
		t.Errorf("STATUS noise floor = %d, want -104", got)
	}
}

// A reply carries what the provider vouches for when it is sent, never a copy kept past the provider's staleness window.
func TestStatus_DropsAReadingTheProviderHasWithdrawn(t *testing.T) {
	r := testStatsRepeater(t)
	ds := DeviceStats{BatteryMV: 4100, HaveBattery: true, MCUTempC: 31.5, HaveMCUTemp: true, NoiseFloor: -104, HaveNoiseFloor: true}
	r.boardStats = func() DeviceStats { return ds }
	battery := func() uint16 { return binary.LittleEndian.Uint16(r.statusBody()[0:2]) }
	if got := battery(); got != 4100 {
		t.Fatalf("STATUS battery = %d mV, want 4100 while the provider vouches for it", got)
	}

	ds = DeviceStats{BatteryMV: 4100, MCUTempC: 31.5, NoiseFloor: -104} // the provider's staleness window passed
	if got := battery(); got != 0 {
		t.Errorf("STATUS battery = %d mV after the provider withdrew it, want 0", got)
	}
	if got := int16(binary.LittleEndian.Uint16(r.statusBody()[4:6])); got != 0 {
		t.Errorf("STATUS noise floor = %d after the provider withdrew it, want 0", got)
	}
	if s := r.Stats(); s.BatteryMV != nil || s.NoiseFloor != nil {
		t.Errorf("stats battery=%v noise=%v after the provider withdrew them, want none", s.BatteryMV, s.NoiseFloor)
	}
	if got := r.selfReadings().TempC; got != nil {
		t.Errorf("MCU temperature = %v after the provider withdrew it, want none", *got)
	}
}
