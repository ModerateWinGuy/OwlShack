package telemetry

import (
	"encoding/binary"
	"math"
	"testing"

	meshcore "github.com/OwlShack/meshcore-go"
)

// putFloat mirrors the firmware encoder so the test is a genuine round trip, not a restatement of the decoder.
func putFloat(v float64, size int, mult uint32, signed bool) []byte {
	neg := v < 0
	if neg {
		v = -v
	}
	u := uint64(v * float64(mult))
	if signed && neg {
		mask := uint64(1)<<(uint(size)*8) - 1
		u = (mask - (u & mask) + 1) & mask
	}
	out := make([]byte, size)
	for i := size - 1; i >= 0; i-- {
		out[i] = byte(u)
		u >>= 8
	}
	return out
}

func TestParseSeries(t *testing.T) {
	var b []byte
	now := make([]byte, 4)
	binary.LittleEndian.PutUint32(now, 1_700_000_000)
	b = append(b, now...)

	b = append(b, 2, meshcore.LPPTemperature)
	b = append(b, putFloat(-3.5, 2, 10, true)...)
	b = append(b, putFloat(21.7, 2, 10, true)...)
	b = append(b, putFloat(12.2, 2, 10, true)...)
	b = append(b, 1, meshcore.LPPVoltage)
	b = append(b, putFloat(3.71, 2, 100, false)...)
	b = append(b, putFloat(4.16, 2, 100, false)...)
	b = append(b, putFloat(3.98, 2, 100, false)...)
	b = append(b, 3, meshcore.LPPRelativeHumidity)
	b = append(b, putFloat(40.5, 2, 10, false)...)
	b = append(b, putFloat(90.0, 2, 10, false)...)
	b = append(b, putFloat(65.2, 2, 10, false)...)
	b = append(b, 4, meshcore.LPPGenericSensor)
	b = append(b, putFloat(100000, 4, 1, false)...)
	b = append(b, putFloat(300000, 4, 1, false)...)
	b = append(b, putFloat(200000, 4, 1, false)...)

	s, err := ParseSeries(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.NodeTime != 1_700_000_000 {
		t.Errorf("nodeTime = %d", s.NodeTime)
	}
	if len(s.Entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(s.Entries))
	}
	close := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

	e := s.Entries[0]
	if e.Channel != 2 || e.Name != "Temperature" || e.Unit != "°C" {
		t.Errorf("entry0 meta = %+v", e)
	}
	if !close(e.Min, -3.5) || !close(e.Max, 21.7) || !close(e.Avg, 12.2) {
		t.Errorf("temperature min/max/avg = %v/%v/%v", e.Min, e.Max, e.Avg)
	}
	e = s.Entries[1]
	if !close(e.Min, 3.71) || !close(e.Max, 4.16) || !close(e.Avg, 3.98) || e.Unit != "V" {
		t.Errorf("voltage = %+v", e)
	}
	e = s.Entries[2]
	if !close(e.Min, 40.5) || !close(e.Max, 90) || !close(e.Avg, 65.2) {
		t.Errorf("humidity = %+v", e)
	}
	e = s.Entries[3]
	if !close(e.Min, 100000) || !close(e.Max, 300000) || !close(e.Avg, 200000) {
		t.Errorf("generic = %+v", e)
	}

	if _, err := ParseSeries(b[:len(b)-1]); err == nil {
		t.Error("truncated series parsed without error")
	}
	// Bare header = empty window.
	s, err = ParseSeries(now)
	if err != nil || len(s.Entries) != 0 {
		t.Errorf("empty window: %v entries=%d", err, len(s.Entries))
	}
}

// Two 2-byte channels leave 8 bytes of AES-ECB block padding that must not parse as entries.
func TestParseSeries_BlockPadding(t *testing.T) {
	body := []byte{0x00, 0x00, 0x00, 0x00} // now = 0
	body = append(body,
		0x02, 0x67, 0x00, 0xC8, 0x01, 0x2C, 0x00, 0xFA, // ch2 temp 20.0/30.0/25.0
		0x03, 0x68, 0x00, 0x64, 0x00, 0xC8, 0x00, 0x96, // ch3 humidity
	)
	padded := append(body, make([]byte, 16-len(body)%16)...)
	if len(padded)%16 != 0 {
		t.Fatalf("test payload not block aligned: %d", len(padded))
	}

	s, err := ParseSeries(padded)
	if err != nil {
		t.Fatalf("ParseSeries: %v", err)
	}
	if len(s.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(s.Entries), s.Entries)
	}
	if s.Entries[0].Channel != 2 || s.Entries[1].Channel != 3 {
		t.Errorf("channels = %d,%d; want 2,3", s.Entries[0].Channel, s.Entries[1].Channel)
	}
	if s.Entries[0].Min != 20 || s.Entries[0].Max != 30 || s.Entries[0].Avg != 25 {
		t.Errorf("temp = %v/%v/%v; want 20/30/25", s.Entries[0].Min, s.Entries[0].Max, s.Entries[0].Avg)
	}
}

// Firmware putFloat holds a value in a uint32, so a 6-byte gyrometer reading has two zero top bytes; channel 0 ends the data (LPPReader::readHeader).
func TestParseSeries_FirmwareGolden(t *testing.T) {
	body := []byte{0x00, 0xF1, 0x53, 0x65}                                          // now = 1700000000
	body = append(body, 1, meshcore.LPPVoltage, 0x01, 0x73, 0x01, 0xA0, 0x01, 0x8E) // 3.71 / 4.16 / 3.98 V
	body = append(body, 2, meshcore.LPPGyrometer,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, // 256, multiplier 1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x64, // 100
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00) // 0
	body = append(body, 0, meshcore.LPPVoltage, 0x01, 0x73, 0x01, 0xA0, 0x01, 0x8E)

	s, err := ParseSeries(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.NodeTime != 1_700_000_000 || len(s.Entries) != 2 {
		t.Fatalf("got time %d and %d entries, want 1700000000 and 2: %+v", s.NodeTime, len(s.Entries), s.Entries)
	}
	if e := s.Entries[0]; e.Channel != 1 || e.Min != 3.71 || e.Max != 4.16 || e.Avg != 3.98 || e.Unit != "V" {
		t.Errorf("voltage = %+v", e)
	}
	if e := s.Entries[1]; e.Channel != 2 || e.Min != 256 || e.Max != 100 || e.Avg != 0 {
		t.Errorf("gyrometer = %+v", e)
	}
}
