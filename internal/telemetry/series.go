package telemetry

import (
	"fmt"

	meshcore "github.com/OwlShack/meshcore-go"
)

// SeriesEntry is one sensor channel's min/max/avg over the requested window (firmware MinMaxAvg).
type SeriesEntry struct {
	Channel int     `json:"channel"`
	Type    int     `json:"type"`
	Name    string  `json:"name"`
	Unit    string  `json:"unit,omitempty"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Avg     float64 `json:"avg"`
}

// Series is a decoded REQ_TYPE_GET_AVG_MIN_MAX reply.
type Series struct {
	NodeTime uint32        `json:"nodeTime"` // the sensor's clock when it answered
	Entries  []SeriesEntry `json:"entries"`
	Raw      string        `json:"raw"`
}

// ParseSeries decodes a GET_AVG_MIN_MAX body (after the reflected tag).
func ParseSeries(data []byte) (*Series, error) {
	r, err := meshcore.ParseAvgMinMaxReply(data)
	if err != nil {
		return nil, err
	}
	s := &Series{NodeTime: r.Clock, Entries: []SeriesEntry{}, Raw: fmt.Sprintf("%x", data)}
	for _, e := range r.Series {
		name, unit := lppTypeMeta(false, e.Type)
		s.Entries = append(s.Entries, SeriesEntry{Channel: int(e.Channel), Type: int(e.Type), Name: name, Unit: unit, Min: e.Min, Max: e.Max, Avg: e.Avg})
	}
	return s, nil
}
