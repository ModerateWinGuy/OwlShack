package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	meshcore "github.com/OwlShack/meshcore-go"

	"github.com/OwlShack/OwlShack/internal/store"
)

// maxPushIntervalSecs is the longest poll a pushing sensor may have: each poll renews the watch, which the sensor keeps for at most 65535 s.
const maxPushIntervalSecs = 64800

// lppField is how firmware LPPData stores one value (LPPDataHelpers.h:66-110): a delta must fit and not round to 0.
type lppField struct {
	mult   float64
	maxRaw float64
}

// Single-value LPP types the sensor's delta check reads. Humidity is left out because firmware 1.18 reads it as 2 bytes where the telemetry holds 1,
// on/off types because a change of 1 never beats the firmware's strict "> delta", and 4-byte values stop at what a float holds exactly.
var subscribableLPP = map[byte]lppField{
	meshcore.LPPPercentage:  {1, 255},
	meshcore.LPPAnalogInput: {100, 32767}, meshcore.LPPTemperature: {10, 32767}, meshcore.LPPAltitude: {1, 32767},
	meshcore.LPPLuminosity: {1, 65535}, meshcore.LPPBarometricPressure: {10, 65535}, meshcore.LPPVoltage: {100, 65535},
	meshcore.LPPCurrent: {1000, 65535}, meshcore.LPPConcentration: {1, 65535}, meshcore.LPPPower: {1, 65535},
	meshcore.LPPDirection:     {1, 65535},
	meshcore.LPPGenericSensor: {1, 1 << 24}, meshcore.LPPDistance: {1000, 1 << 24}, meshcore.LPPEnergy: {1000, 1 << 24},
}

// checkPushDeltas holds monitorPushDeltas to what a 1.18 sensor can watch; channel, type and value are required.
func checkPushDeltas(raw json.RawMessage) error {
	var in []struct {
		Channel *int     `json:"channel"`
		Type    *int     `json:"type"`
		Value   *float64 `json:"value"`
		Name    string   `json:"name"`
		Unit    string   `json:"unit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errors.New("monitorPushDeltas must be a list of {channel, type, value, name, unit}")
	}
	if len(in) > 3 {
		return errors.New("monitorPushDeltas can watch at most three readings")
	}
	deltas := make([]meshcore.LPPDataValue, 0, len(in))
	for i, d := range in {
		if d.Channel == nil || d.Type == nil || d.Value == nil {
			return fmt.Errorf("monitorPushDeltas %d needs channel, type and value", i+1)
		}
		if *d.Channel < 0 || *d.Channel > 255 || *d.Type < 0 || *d.Type > 255 {
			return fmt.Errorf("monitorPushDeltas %d: channel and type are 0-255", i+1)
		}
		if len(d.Name) > 64 || len(d.Unit) > 16 || strings.ContainsFunc(d.Name+d.Unit, unicode.IsControl) {
			return fmt.Errorf("monitorPushDeltas %d: name or unit is too long or has a control character", i+1)
		}
		if byte(*d.Type) == meshcore.LPPRelativeHumidity {
			return errors.New("humidity can't be watched: MeshCore 1.18 sensors misread humidity changes")
		}
		f, ok := subscribableLPP[byte(*d.Type)]
		if !ok {
			return fmt.Errorf("monitorPushDeltas %d: type %d is not a single reading the sensor can compare", i+1, *d.Type)
		}
		if raw := *d.Value * f.mult; raw < 1 || raw > f.maxRaw {
			label := d.Name
			if label == "" {
				label = fmt.Sprintf("monitorPushDeltas %d", i+1)
			}
			return fmt.Errorf("%s: the change must be from %s to %s", label, strconv.FormatFloat(1/f.mult, 'f', -1, 64), strconv.FormatFloat(f.maxRaw/f.mult, 'f', -1, 64))
		}
		deltas = append(deltas, meshcore.LPPDataValue{Channel: byte(*d.Channel), Type: byte(*d.Type), Value: *d.Value})
	}
	if _, err := meshcore.BuildSubscribeRequest(meshcore.SubscribeRequest{MinDeltas: deltas}); len(deltas) > 0 && err != nil {
		return errors.New("monitorPushDeltas don't fit: a sensor keeps 14 bytes of changes, and a generic, distance or energy reading takes 6")
	}
	return nil
}

// checkPushSettings is the rule across fields, checked on the merged metadata so a partial patch can't get round it.
func checkPushSettings(m store.ContactMetadata, contactType string) error {
	if !m.MonitorPush {
		return nil
	}
	if !strings.EqualFold(contactType, "SENSOR") {
		return errors.New("monitorPush is for sensors only")
	}
	if len(m.MonitorPushDeltas) == 0 {
		return errors.New("monitorPush needs at least one reading in monitorPushDeltas")
	}
	if m.MonitorIntervalSecs > maxPushIntervalSecs {
		return fmt.Errorf("monitorIntervalSecs %d is over %d: a pushing sensor's watch would lapse between polls", m.MonitorIntervalSecs, maxPushIntervalSecs)
	}
	return nil
}

func (s *Server) handleSensorSubscription(w http.ResponseWriter, r *http.Request) {
	ops, ok := s.repeaterOps(r.PathValue("name"))
	if !ok {
		s.companionNotRunning(w, r)
		return
	}
	writeJSON(w, http.StatusOK, ops.Subscription(r.PathValue("pubkey")))
}
