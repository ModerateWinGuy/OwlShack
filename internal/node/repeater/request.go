package repeater

import (
	"encoding/binary"
	"encoding/hex"
	"sort"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"

	"github.com/OwlShack/OwlShack/internal/buildinfo"
	"github.com/OwlShack/OwlShack/internal/sensor"
	"github.com/OwlShack/OwlShack/internal/store"
)

// Admin request types (firmware REQ_TYPE_*), mirroring internal/client/repeater.
const (
	telemChannelSelf = sensor.ChannelSelf
	// Matches the firmware's results_buffer so a neighbours reply still fits one packet.
	neighboursMaxBody = 130
)

// handleReq fires for every REQ we hear and self-filters by destination hash plus MAC-against-ACL.
func (r *Repeater) handleReq(pkt *meshcore.Packet) {
	req, err := meshcore.RequestFromBytes(pkt.Payload)
	if err != nil {
		return
	}
	if req.Destination != r.node.Identity().PublicKey()[0] {
		return
	}
	client, secret, ok := r.aclClient(req.Source, req.VerifyMAC)
	if !ok {
		return
	}
	plain := req.Decrypt(secret)
	if len(plain) < 5 {
		return
	}
	pkt.MarkDoNotRetransmit()
	tag := binary.LittleEndian.Uint32(plain[:4]) // client timestamp, reflected back
	if tag <= client.LastTimestamp {
		return // firmware: `timestamp > client->last_timestamp` or it's a replay
	}
	body, ok := r.buildReqResponse(client, plain[4:], sensor.MaxReplyBody(pkt))
	if !ok {
		return
	}
	// The client matches its pending request by the reflected tag.
	reply := meshcore.BuildTaggedPlaintext(tag, body)

	r.touchClient(client, tag)

	var clientPub [32]byte
	if pub, err := hex.DecodeString(client.PubKey); err == nil && len(pub) == 32 {
		copy(clientPub[:], pub)
	}
	if err := r.sendServerReply(pkt, clientPub, secret, reply); err != nil {
		r.log.Error("request reply failed", "reqType", plain[4], "error", err)
	}
}

// buildReqResponse builds everything after the reflected tag from the request body req; false answers nothing, and only telemetry can exceed maxBody.
func (r *Repeater) buildReqResponse(client *store.RepeaterACLEntry, req []byte, maxBody int) ([]byte, bool) {
	switch req[0] {
	case meshcore.ReqTypeGetStatus:
		return r.statusBody(), true
	case meshcore.ReqTypeGetNeighbours:
		nr, err := meshcore.ParseNeighboursRequest(req)
		if err != nil {
			return nil, false // short, or an unknown request version
		}
		return r.neighboursBody(nr), true
	case meshcore.ReqTypeGetOwnerInfo:
		return meshcore.BuildOwnerInfoReply(meshcore.OwnerInfo{FirmwareVersion: buildinfo.Version, Name: r.cfg.Name, Owner: r.cfg.OwnerInfo}), true
	case meshcore.ReqTypeGetAccessList:
		if client.Permissions&permRoleMask != permAdmin {
			return nil, false // admin-only
		}
		if len(req) >= 3 && (req[1] != 0 || req[2] != 0) {
			return nil, false // reserved query params
		}
		return r.accessListBody(), true
	case meshcore.ReqTypeGetTelemetryData:
		// handleRequest sends base whatever the mask says, and a guest nothing more.
		perms, err := meshcore.ParseTelemetryRequest(req)
		if err != nil {
			perms = sensor.PermAll
		}
		if client.Permissions&permRoleMask == permGuest {
			perms = 0
		}
		perms |= meshcore.TelemPermBase
		var entries []sensor.ChannelEntry
		var statuses []sensor.Status
		if r.telemetry != nil {
			entries, statuses = r.telemetry()
		}
		body, dropped := sensor.BuildReply(perms, r.selfReadings(), entries, statuses, maxBody)
		if dropped {
			r.log.Info("telemetry map too long for one packet, sending the node's own readings only", "max", maxBody)
		}
		return body, true
	default:
		return nil, false
	}
}

// selfReadings is what the radio board says about itself; one with no cell, or gone quiet, leaves the battery at 0 rather than a stale reading.
func (r *Repeater) selfReadings() sensor.SelfReadings {
	var out sensor.SelfReadings
	ds := r.deviceStats()
	if ds.HaveBattery {
		out.BatteryVolts = float64(ds.BatteryMV) / 1000
	}
	if ds.HaveMCUTemp {
		out.TempC = &ds.MCUTempC
	}
	return out
}

// statusBody is the repeater's RepeaterStats; untracked counters stay zero.
func (r *Repeater) statusBody() []byte {
	rc := r.routeCounters()
	r.mu.Lock()
	started := r.startedAt
	r.mu.Unlock()
	var s meshcore.RepeaterStats
	if !started.IsZero() {
		s.UpTimeSecs = uint32(time.Since(started).Seconds())
	}
	ds := r.deviceStats()
	if ds.HaveBattery {
		s.BattMilliVolts = ds.BatteryMV
	}
	if ds.HaveNoiseFloor { // the firmware reports 0 until it has sampled
		s.NoiseFloor = ds.NoiseFloor
	}
	if r.node != nil {
		s.TxQueueLen = uint16(r.node.TxQueueLen())
	}
	if r.haveSignal.Load() {
		s.LastRSSI, s.LastSNR = int16(r.lastRSSI.Load()), int16(r.lastSNRx4.Load())
	}
	s.PacketsRecv = uint32(r.recvCount.Load())
	s.PacketsSent = uint32(r.sentFlood.Load() + r.sentDirect.Load()) // radio getPacketsSent: every TX
	s.AirTimeSecs = uint32(r.txAirtimeMs.Load() / 1000)
	s.SentFlood, s.SentDirect = uint32(r.sentFlood.Load()), uint32(r.sentDirect.Load())
	s.RecvFlood, s.RecvDirect = uint32(rc.FloodReceived), uint32(rc.DirectReceived)
	s.DirectDups, s.FloodDups = uint16(rc.DirectDuplicates), uint16(rc.FloodDuplicates)
	s.RxAirTimeSecs = uint32(r.rxAirtimeMs.Load() / 1000)
	return meshcore.BuildRepeaterStats(s)
}

// sortNeighbours applies the firmware order_by selector: 0=newest (native order), 1=oldest, 2=strongest, 3=weakest.
func sortNeighbours(list []neighbor, orderBy byte) {
	switch orderBy {
	case 1:
		sort.Slice(list, func(i, j int) bool { return list[i].heard.Before(list[j].heard) })
	case 2:
		sort.Slice(list, func(i, j int) bool { return list[i].snr > list[j].snr })
	case 3:
		sort.Slice(list, func(i, j int) bool { return list[i].snr < list[j].snr })
	}
}

// neighboursBody pages the neighbours as the firmware does, stopping where its 130-byte results_buffer is full.
func (r *Repeater) neighboursBody(req meshcore.NeighboursRequest) []byte {
	now := time.Now()
	list := r.snapshotNeighbors()
	sortNeighbours(list, req.OrderBy)
	reply := meshcore.NeighboursReply{Total: uint16(len(list))}
	page := list[min(int(req.Offset), len(list)):]
	page = page[:min(int(req.Count), len(page), neighboursMaxBody/(int(req.PrefixLen)+5))]
	for _, n := range page {
		reply.Neighbours = append(reply.Neighbours, meshcore.Neighbour{Prefix: n.pubkey[:req.PrefixLen], HeardSecsAgo: uint32(now.Sub(n.heard).Seconds()), SNR: meshcore.SNRToWire(float32(n.snr))})
	}
	return meshcore.BuildNeighboursReply(reply)
}

// accessListBody lists each non-guest client, capped like the firmware's `ofs + 7 <= sizeof(reply_data) - 4`.
func (r *Repeater) accessListBody() []byte {
	r.acl.RLock()
	keys := make([]string, 0, len(r.acl.m))
	perms := make(map[string]int, len(r.acl.m))
	for k, e := range r.acl.m {
		keys = append(keys, k)
		perms[k] = e.Permissions
	}
	r.acl.RUnlock()
	sort.Strings(keys)

	var entries []meshcore.ACLEntry
	for _, k := range keys {
		pub, err := hex.DecodeString(k)
		if perms[k] == 0 || err != nil || len(pub) < 6 || 4+7*(len(entries)+1) > meshcore.MaxPacketPayload-4 {
			continue
		}
		entries = append(entries, meshcore.ACLEntry{Prefix: [6]byte(pub), Permissions: byte(perms[k])})
	}
	return meshcore.BuildAccessListReply(entries)
}
