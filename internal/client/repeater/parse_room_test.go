package repeater

import (
	"encoding/binary"
	"testing"
)

// Pins the ServerStats trailer: bytes 48-52 are n_posted and n_post_push, not rx_air_time_secs.
func TestParseRoomStatus(t *testing.T) {
	b := make([]byte, 60)                         // 52 bytes padded to the cipher block, as every reply arrives
	binary.LittleEndian.PutUint16(b[0:2], 4100)   // batt mV
	binary.LittleEndian.PutUint32(b[20:24], 3600) // uptime
	binary.LittleEndian.PutUint16(b[42:44], 42)   // last_snr x4 → 10.5
	binary.LittleEndian.PutUint16(b[48:50], 17)   // n_posted
	binary.LittleEndian.PutUint16(b[50:52], 9)    // n_post_push

	s, err := parseRoomStatus(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.BatteryMV != 4100 || s.UptimeSecs != 3600 || s.LastSNR != 10.5 {
		t.Errorf("shared fields wrong: %+v", s)
	}
	if s.Posted == nil || *s.Posted != 17 || s.PostPushes == nil || *s.PostPushes != 9 {
		t.Errorf("posted/pushes = %v/%v, want 17/9", s.Posted, s.PostPushes)
	}
	if s.RxAirSecs != 0 {
		t.Errorf("room status must not report rx airtime, got %d", s.RxAirSecs)
	}
	// The repeater parser would misread the same bytes as airtime.
	r, _ := parseRepeaterStatus(b)
	if r.RxAirSecs == 0 {
		t.Error("sanity: repeater parser should have read the counters as airtime")
	}
	if _, err := parseRoomStatus(b[:51]); err == nil {
		t.Error("short room status parsed")
	}
}
