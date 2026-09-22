package modem

import "testing"

func TestLookupBoard(t *testing.T) {
	if _, err := LookupBoard("ultrapeaterzero-e22p"); err != nil {
		t.Fatalf("LookupBoard: %v", err)
	}
	if _, err := LookupBoard("no-such-hat"); err == nil {
		t.Error("LookupBoard(unknown) = nil error, want one naming the known boards")
	}
}

// A half-added board otherwise compiles and only fails on someone's roof.
func TestBoardRegistryIsComplete(t *testing.T) {
	for _, b := range Boards() {
		t.Run(b.Name, func(t *testing.T) {
			if b.Name == "" || b.Label == "" || b.Chip == "" || b.SPIPort == "" {
				t.Errorf("board has an empty identity field: %+v", b)
			}
			if b.MaxTxPower == 0 {
				t.Error("MaxTxPower is 0, which rejects every transmit power")
			}
			o := b.Opts()
			if o.ResetPin == "" {
				t.Error("ResetPin unset: the radio can never be brought up")
			}
			if o.BusyPin == "" {
				t.Error("BusyPin unset: every SPI command races the chip")
			}
			if o.Speed == 0 {
				t.Error("Speed unset: the SPI clock would default to the bus minimum")
			}
			if b.Verified != "hardware" && b.Verified != "community" {
				t.Errorf("verified = %q, want a declared provenance", b.Verified)
			}
			// DIO2 and a TX-enable pin are the two RF-switch mechanisms; with neither, a transmit goes into a terminated switch and reads as a quiet mesh.
			if !o.UseDIO2AsRfSwitch && o.TxEnPin == "" {
				t.Error("no RF switch control: this entry cannot be driven and should not ship")
			}
		})
	}
}

// Both RAK6421 slots ran on the hat: each received from the live mesh, and two neighbours
// re-flooded the advert each transmitted. The chip-select is the difference between the two
// slots, so a swap would silence one.
func TestRAK6421SlotsAreUsable(t *testing.T) {
	for _, tc := range []struct {
		name, reset, busy, dio1, port string
	}{
		{"rak6421-13300x-slot1", "GPIO16", "GPIO24", "GPIO22", "SPI0.0"},
		{"rak6421-13300x-slot2", "GPIO24", "GPIO19", "GPIO18", "SPI0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := LookupBoard(tc.name)
			if err != nil {
				t.Fatalf("LookupBoard: %v", err)
			}
			if b.Verified != "hardware" {
				t.Errorf("verified = %q, want hardware", b.Verified)
			}
			if b.SPIPort != tc.port {
				t.Errorf("SPIPort = %q, want %q", b.SPIPort, tc.port)
			}
			o := b.Opts()
			for _, p := range []struct{ field, got, want string }{
				{"reset", o.ResetPin, tc.reset},
				{"busy", o.BusyPin, tc.busy},
				{"dio1", o.Dio1Pin, tc.dio1},
			} {
				if p.got != p.want {
					t.Errorf("%s = %q, want %q", p.field, p.got, p.want)
				}
			}
			if !o.UseDIO2AsRfSwitch {
				t.Error("UseDIO2AsRfSwitch false: the module switches its own RF path and has no txen line")
			}
			if o.TCXOVoltage == 0 {
				t.Error("TCXO unset: DIO3 powers the oscillator at 1.8 V, and without it the radio hears nothing")
			}
		})
	}
}

// periph resolves pins by name on the default chip, so a banked pin map would drive whatever Pi
// header line happens to share the number. Louder than a refusal: the entry is not drivable at all.
func TestGPIOChipIsRejected(t *testing.T) {
	const doc = `{"boards":{
		"banked":{"gpio_chip":1,"reset_pin":25,"busy_pin":5,"irq_pin":22,"tx_power":22,"use_dio2_rf":true,"verified":"community"}
	}}`
	if _, err := parseBoards([]byte(doc)); err == nil {
		t.Error("a board on gpiochip 1 loaded; its pin numbers mean something else on that chip")
	}
}

// The same device spelled two ways must not read as a mismatch, or every SPI start warns.
func TestSPIPortKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"SPI0.1", "/dev/spidev0.1", true},
		{"SPI0.0", "SPI0.0", true},
		{"SPI0.0", "SPI0.1", false},
	} {
		if got := spiPortKey(tc.a) == spiPortKey(tc.b); got != tc.same {
			t.Errorf("spiPortKey(%q)==spiPortKey(%q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// The one hat this project has run must stay usable.
func TestVerifiedBoardIsUsable(t *testing.T) {
	b, err := LookupBoard("ultrapeaterzero-e22p")
	if err != nil {
		t.Fatalf("LookupBoard: %v", err)
	}
	if b.Verified != "hardware" {
		t.Errorf("verified = %q, want hardware", b.Verified)
	}
	// The wiring confirmed on the hat, so a boards.json edit that moves a pin fails here.
	o := b.Opts()
	for _, tc := range []struct{ field, got, want string }{
		{"reset", o.ResetPin, "GPIO25"},
		{"busy", o.BusyPin, "GPIO5"},
		{"dio1", o.Dio1Pin, "GPIO12"},
		{"cs", o.CSPin, "GPIO24"},
		{"txen", o.TxEnPin, "GPIO27"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if len(o.EnablePins) != 2 || o.EnablePins[0] != "GPIO17" || o.EnablePins[1] != "GPIO16" {
		t.Errorf("EnablePins = %v, want [GPIO17 GPIO16]: a missed enable line is a radio that never answers", o.EnablePins)
	}
	if o.TCXOVoltage == 0 {
		t.Error("TCXO unset: DIO3 powers the oscillator on this board, and without it the radio hears nothing")
	}
	if o.TxLedPin != "GPIO21" || o.RxLedPin != "GPIO20" {
		t.Errorf("LED pins = tx %q / rx %q, want GPIO21 / GPIO20", o.TxLedPin, o.RxLedPin)
	}
	if !b.HasLEDs() {
		t.Error("HasLEDs false: the board picker would offer this hat without mentioning its LEDs")
	}
}

// Reading an omitted pin as GPIO0 would drive an unrelated line every transaction.
func TestParseBoards_PinAbsenceIsNotGPIO0(t *testing.T) {
	const doc = `{"boards":{"t":{
		"reset_pin":25,"busy_pin":5,"tx_power":22,"use_dio2_rf":true,
		"cs_pin":-1,"txen_pin":0,"verified":"community"}}}`
	got, err := parseBoards([]byte(doc))
	if err != nil {
		t.Fatalf("parseBoards: %v", err)
	}
	o := got["t"].Opts()
	if o.CSPin != "" {
		t.Errorf("cs_pin -1 became %q, want no pin at all", o.CSPin)
	}
	if o.TxEnPin != "GPIO0" {
		t.Errorf("txen_pin 0 became %q, want GPIO0: 0 is a real pin", o.TxEnPin)
	}
	if o.Dio1Pin != "" {
		t.Errorf("an omitted irq_pin became %q, want no pin", o.Dio1Pin)
	}
}

func TestParseBoards_Rejections(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"provenance is required", `{"boards":{"t":{"reset_pin":1,"busy_pin":2,"tx_power":22}}}`},
		{"provenance must be known", `{"boards":{"t":{"reset_pin":1,"busy_pin":2,"tx_power":22,"verified":"probably"}}}`},
		{"reset is required", `{"boards":{"t":{"busy_pin":2,"tx_power":22,"verified":"community"}}}`},
		{"busy is required", `{"boards":{"t":{"reset_pin":1,"tx_power":22,"verified":"community"}}}`},
		{"tx power is required", `{"boards":{"t":{"reset_pin":1,"busy_pin":2,"verified":"community"}}}`},
		{"tx power is bounded", `{"boards":{"t":{"reset_pin":1,"busy_pin":2,"tx_power":40,"verified":"community"}}}`},
		{"tcxo voltage must be one the chip can make", `{"boards":{"t":{"reset_pin":1,"busy_pin":2,"tx_power":22,"verified":"community","use_dio3_tcxo":true,"dio3_tcxo_voltage":5}}}`},
		{"an empty file is not a board list", `{"boards":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBoards([]byte(tc.doc)); err == nil {
				t.Error("parseBoards accepted it; a board that reaches the radio on wrong or missing pins has no symptom to debug")
			}
		})
	}
}

// A pasted entry whose en_pin was dropped is a module that never powers up.
func TestParseBoards_AcceptsSingularEnPin(t *testing.T) {
	const doc = `{"boards":{"t":{"reset_pin":25,"busy_pin":5,"tx_power":22,
		"use_dio2_rf":true,"en_pin":26,"verified":"community"}}}`
	got, err := parseBoards([]byte(doc))
	if err != nil {
		t.Fatalf("parseBoards: %v", err)
	}
	if o := got["t"].Opts(); len(o.EnablePins) != 1 || o.EnablePins[0] != "GPIO26" {
		t.Errorf("EnablePins = %v, want [GPIO26]", o.EnablePins)
	}
}
