package repeater

import "testing"

// A host with no cell must not render as a flat battery needing attention.
func TestBatteryReading_AbsentWhenTheHostHasNoCell(t *testing.T) {
	ds := DeviceStats{}
	r := testStatsRepeater(t)
	r.boardStats = func() DeviceStats { return ds }
	if got := r.Stats().BatteryMV; got != nil {
		t.Errorf("battery = %d, want nil when there is no battery to measure", *got)
	}

	// A board that does report one still publishes it, including a real 0.
	ds = DeviceStats{BatteryMV: 4168, HaveBattery: true}
	if got := r.Stats().BatteryMV; got == nil || *got != 4168 {
		t.Errorf("battery = %v, want 4168 once a board reports one", got)
	}
}
