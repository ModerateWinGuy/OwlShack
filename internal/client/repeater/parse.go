package repeater

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	meshcore "github.com/OwlShack/meshcore-go"
)

type Neighbor struct {
	PubkeyPrefix string  `json:"pubkeyPrefix"`
	SecsAgo      uint32  `json:"secsAgo"`
	SNR          float64 `json:"snr"`
}

type Neighbors struct {
	TotalCount   int        `json:"totalCount"`
	ResultsCount int        `json:"resultsCount"`
	Neighbors    []Neighbor `json:"neighbors"`
}

type OwnerInfo struct {
	FirmwareVersion string `json:"firmwareVersion"`
	NodeName        string `json:"nodeName"`
	OwnerInfo       string `json:"ownerInfo"`
}

type AccessListEntry struct {
	PubkeyPrefix string `json:"pubkeyPrefix"`
	Permissions  uint8  `json:"permissions"`
}

type AccessList struct {
	Entries []AccessListEntry `json:"entries"`
}

type Status struct {
	BatteryMV   uint16  `json:"batteryMv"`
	QueueLen    uint16  `json:"queueLen"`
	NoiseFloor  int16   `json:"noiseFloor"`
	LastRSSI    int16   `json:"lastRssi"`
	PacketsRecv uint32  `json:"packetsRecv"`
	PacketsSent uint32  `json:"packetsSent"`
	TxAirSecs   uint32  `json:"txAirSecs"`
	RxAirSecs   uint32  `json:"rxAirSecs"`
	UptimeSecs  uint32  `json:"uptimeSecs"`
	FloodTx     uint32  `json:"floodTx"`
	DirectTx    uint32  `json:"directTx"`
	FloodRx     uint32  `json:"floodRx"`
	DirectRx    uint32  `json:"directRx"`
	ErrEvents   uint16  `json:"errEvents"`
	LastSNR     float64 `json:"lastSnr"`
	DirectDups  uint16  `json:"directDups"`
	FloodDups   uint16  `json:"floodDups"`
	RecvErrors  uint32  `json:"recvErrors"`
	ChanUtil    float64 `json:"chanUtil"`
	// Room servers only (ServerStats); nil on repeaters.
	Posted     *uint16 `json:"posted,omitempty"`
	PostPushes *uint16 `json:"postPushes,omitempty"`
}

func parseRepeaterNeighbors(data []byte, prefixLen int) (*Neighbors, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: neighbors data too short: got %d", ErrBadReply, len(data))
	}
	out := &Neighbors{
		TotalCount:   int(int16(binary.LittleEndian.Uint16(data[0:2]))),
		ResultsCount: int(int16(binary.LittleEndian.Uint16(data[2:4]))),
	}
	entrySize := prefixLen + 4 + 1
	pos := 4
	for i := 0; i < out.ResultsCount; i++ {
		if pos+entrySize > len(data) {
			break
		}
		secsAgo := binary.LittleEndian.Uint32(data[pos+prefixLen : pos+prefixLen+4])
		snr := float64(int8(data[pos+prefixLen+4])) / 4
		out.Neighbors = append(out.Neighbors, Neighbor{
			PubkeyPrefix: hex.EncodeToString(data[pos : pos+prefixLen]),
			SecsAgo:      secsAgo,
			SNR:          snr,
		})
		pos += entrySize
	}
	return out, nil
}

func parseRepeaterAccessList(data []byte) *AccessList {
	out := &AccessList{Entries: []AccessListEntry{}}
	const stride = 7 // 6-byte prefix + 1-byte permissions
	for pos := 0; pos+stride <= len(data); pos += stride {
		if data[pos+6] == 0 {
			continue // firmware skips deleted entries; this is cipher-block zero padding
		}
		out.Entries = append(out.Entries, AccessListEntry{
			PubkeyPrefix: hex.EncodeToString(data[pos : pos+6]),
			Permissions:  data[pos+6],
		})
	}
	return out
}

func parseRepeaterOwnerInfo(data []byte) *OwnerInfo {
	text := strings.TrimRight(string(data), "\x00")
	parts := strings.SplitN(text, "\n", 3)
	info := &OwnerInfo{}
	if len(parts) > 0 {
		info.FirmwareVersion = parts[0]
	}
	if len(parts) > 1 {
		info.NodeName = parts[1]
	}
	if len(parts) > 2 {
		info.OwnerInfo = parts[2]
	}
	return info
}

// parseRoomStatus reads a room server's ServerStats, which carries posts where a repeater has its rx airtime.
func parseRoomStatus(data []byte) (*Status, error) {
	st, err := meshcore.ParseRoomStats(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadReply, err)
	}
	s := commonStatus(st.CommonStats)
	s.Posted, s.PostPushes = &st.Posted, &st.PostPushes
	return s, nil
}

// commonStatus is the part RepeaterStats and ServerStats share.
func commonStatus(c meshcore.CommonStats) *Status {
	return &Status{
		BatteryMV:   c.BattMilliVolts,
		QueueLen:    c.TxQueueLen,
		NoiseFloor:  c.NoiseFloor,
		LastRSSI:    c.LastRSSI,
		PacketsRecv: c.PacketsRecv,
		PacketsSent: c.PacketsSent,
		TxAirSecs:   c.AirTimeSecs,
		UptimeSecs:  c.UpTimeSecs,
		FloodTx:     c.SentFlood,
		DirectTx:    c.SentDirect,
		FloodRx:     c.RecvFlood,
		DirectRx:    c.RecvDirect,
		ErrEvents:   c.ErrEvents,
		LastSNR:     float64(c.LastSNR) / 4,
		DirectDups:  c.DirectDups,
		FloodDups:   c.FloodDups,
	}
}

// parseRepeaterStatus reads RepeaterStats; a repeater from before n_recv_errors sends 52 bytes, and the block padding reads as 0 errors.
func parseRepeaterStatus(data []byte) (*Status, error) {
	st, err := meshcore.ParseRepeaterStats(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadReply, err)
	}
	s := commonStatus(st.CommonStats)
	s.RxAirSecs, s.RecvErrors = st.RxAirTimeSecs, st.RecvErrors
	if s.UptimeSecs > 0 {
		s.ChanUtil = float64(s.TxAirSecs+s.RxAirSecs) / float64(s.UptimeSecs) * 100
	}
	return s, nil
}
