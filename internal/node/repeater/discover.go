package repeater

import (
	crand "crypto/rand"
	"encoding/binary"
	mrand "math/rand/v2"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
)

// Node discovery (firmware CTL_TYPE_NODE_DISCOVER_REQ/RESP): a zero-hop request whose matching responses become neighbours.

const discoverWindow = 60 * time.Second // collect responses this long (firmware)

// sendDiscover broadcasts a zero-hop NODE_DISCOVER_REQ for repeaters.
func (r *Repeater) sendDiscover() error {
	var tagB [4]byte
	_, _ = crand.Read(tagB[:])
	tag := binary.LittleEndian.Uint32(tagB[:])

	r.discover.Lock()
	r.discover.tag = tag
	r.discover.until = time.Now().Add(discoverWindow)
	r.discover.Unlock()

	payload, err := meshcore.NewDiscoverRequest(meshcore.DiscoverRequest{TypeFilter: 1 << meshcore.AdvertTypeRepeater, Tag: tag}).ToBytes()
	if err != nil {
		return err
	}
	// Zero-hop: direct route, no path — reaches direct neighbours only.
	return r.sendPkt(&meshcore.Packet{
		Header:  meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeControl, 0),
		Payload: payload,
	}, nil, 0)
}

func (r *Repeater) SendDiscover() error { return r.sendDiscover() }

// handleControl ports firmware onControlDataRecv: answer discover requests, record responses matching our in-flight tag.
func (r *Repeater) handleControl(pkt *meshcore.Packet) {
	ctl, err := meshcore.ControlFromBytes(pkt.Payload)
	if err != nil || !pkt.IsRouteDirect() {
		return // firmware: only zero-hop DIRECT control packets are handled
	}
	switch ctl.SubType() {
	case meshcore.ControlSubTypeDiscoverReq:
		r.answerDiscover(pkt, ctl)
	case meshcore.ControlSubTypeDiscoverResp:
		r.recordDiscoverResp(pkt, ctl)
	}
}

// answerDiscover replies with our key unless relaying is off or both advert intervals are 0 (hidden), rate-limited 4/2min.
func (r *Repeater) answerDiscover(pkt *meshcore.Packet, ctl *meshcore.Control) {
	req, err := ctl.DiscoverRequest()
	if err != nil || req.TypeFilter&(1<<meshcore.AdvertTypeRepeater) == 0 {
		return // not asking about repeaters
	}
	if r.cfg.IsFwdDisabled() || r.isHidden() || !r.discoverLimiter.allow() {
		return
	}
	// ponytail: firmware's discovery_mod_timestamp freshness gate isn't modelled — we always answer.

	pub := r.node.Identity().PublicKeyBytes()
	resp := meshcore.DiscoverResponse{NodeType: meshcore.AdvertTypeRepeater, Tag: req.Tag, PubKey: pub[:]}
	if pkt.HasSignalInfo {
		resp.SNR = pkt.SNR
	}
	payload, err := meshcore.NewDiscoverResponse(resp, req.PrefixOnly).ToBytes()
	if err != nil {
		return
	}

	// Widened ×4 because multiple nodes answer the same request (firmware getRetransmitDelay*4, tx_delay_factor 0.5).
	est := uint32(100) // ms fallback when no ToA estimator is available
	if r.airtime != nil {
		est = r.airtime(len(payload))
	}
	delay := time.Duration(mrand.IntN(int(5*est/2)+1)) * 4 * time.Millisecond
	if err := r.sendPkt(&meshcore.Packet{
		Header:  meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeControl, 0),
		Payload: payload,
	}, nil, delay); err != nil {
		r.log.Error("discover response failed", "error", err)
	}
}

// isHidden is firmware isHiddenNode (simple_repeater MyMesh.h:190).
func (r *Repeater) isHidden() bool {
	return r.cfg.AdvertIntervalOr() == 0 && r.cfg.FloodAdvertIntervalOr() == 0
}

// recordDiscoverResp stores the neighbour at the SNR *we* heard it at, not the byte it reports (firmware putNeighbour).
func (r *Repeater) recordDiscoverResp(pkt *meshcore.Packet, ctl *meshcore.Control) {
	resp, err := ctl.DiscoverResponse()
	if err != nil || resp.NodeType != meshcore.AdvertTypeRepeater || len(resp.PubKey) < 32 {
		return
	}

	r.discover.Lock()
	active := r.discover.tag != 0 && resp.Tag == r.discover.tag && time.Now().Before(r.discover.until)
	r.discover.Unlock()
	if !active {
		return
	}

	var pub [32]byte
	copy(pub[:], resp.PubKey)
	if pub == r.node.Identity().Identity.PublicKey() {
		return // don't record ourselves
	}

	r.neighbors.Lock()
	name := ""
	if existing := r.neighbors.m[pub]; existing != nil {
		name = existing.name // keep a name already learned from an advert
	}
	r.neighbors.m[pub] = &neighbor{pubkey: pub, name: name, snr: float64(pkt.SNR), heard: time.Now()}
	r.neighbors.Unlock()
}
