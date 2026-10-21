package companionapp

import (
	"context"
	"math"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	mc "github.com/OwlShack/meshcore-go/companion"

	"github.com/OwlShack/OwlShack/internal/node/companion"
	"github.com/OwlShack/OwlShack/internal/store"
)

// The Server is the companion's AppSink: what it hears goes to the app's queue or straight to a connected app.

func (s *Server) AppMessage(m companion.AppMessage) {
	if m.Channel >= 0 {
		s.queue(true, mc.ChannelMsgRecvV3Response{SNR: m.Packet.SNR, ChannelIdx: byte(m.Channel), PathLen: pathLenByte(m.Packet),
			TxtType: meshcore.TxtTypePlain, SenderTimestamp: m.Timestamp, Text: m.Text}.ToBytes())
		return
	}
	s.queue(false, mc.ContactMsgRecvV3Response{SNR: m.Packet.SNR, PubKeyPrefix: prefix(m.From), PathLen: pathLenByte(m.Packet),
		TxtType: m.TxtType, SenderPrefix: m.Signer, SenderTimestamp: m.Timestamp, Text: m.Text}.ToBytes())
}

// queue holds a message frame for the app's next sync and tickles a connected app once it is stored.
func (s *Server) queue(channel bool, frame []byte) {
	s.st.WriteAsync(func() {
		if err := s.st.AppQueue.Push(context.Background(), s.id, channel, frame, s.env.Load().QueueSize); err != nil {
			s.log.Error("queueing a message for the app", "error", err)
			return
		}
		s.push(mc.PushMsgWaitingResponse{}.ToBytes())
	})
}

// AppAdvert tells the app a contact re-advertised, or offers it a node it does not have, as the firmware does with manual adding.
func (s *Server) AppAdvert(pub [32]byte) {
	if !s.connected() {
		return
	}
	if _, ok := s.contact(pub[:]); ok {
		s.push(mc.PushAdvertResponse{PublicKey: pub}.ToBytes())
		return
	}
	p, err := s.st.Peers.GetByPubKey(context.Background(), pub[:])
	if err != nil || p == nil {
		return
	}
	s.push(mc.PushNewAdvertResponse(peerContact(p)).ToBytes())
}

func (s *Server) AppPathUpdated(pub [32]byte) {
	s.push(mc.PushPathUpdatedResponse{PublicKey: pub}.ToBytes())
}

func (s *Server) AppRawRX(raw []byte, snr float32, rssi int8) {
	if len(raw) > 0 {
		// Route types 0 and 1 are the transport and plain floods, 2 and 3 direct.
		if raw[0]&0x03 <= 1 {
			s.rx.flood.Add(1)
		} else {
			s.rx.direct.Add(1)
		}
		s.rx.airMs.Add(uint64(s.env.Load().Stats.EstAirtimeMs(len(raw))))
		s.rx.lastSNR.Store(math.Float32bits(snr))
		s.rx.lastRSSI.Store(int32(rssi))
	}
	if len(raw)+3 <= mc.MaxFrameSize {
		s.push(mc.PushLogRxDataResponse{LastSNR: snr, LastRSSI: rssi, Raw: raw}.ToBytes())
	}
}

func (s *Server) AppTrace(pkt *meshcore.Packet, tr *meshcore.Trace) {
	s.push(mc.PushTraceDataResponse{PathLen: byte(len(tr.PathHashes)), Flags: tr.Flags, Tag: tr.Tag, AuthCode: tr.AuthCode,
		PathHashes: tr.PathHashes, PathSnrs: pkt.Path, LastSNR: pkt.SNR}.ToBytes())
}

func (s *Server) AppControl(pkt *meshcore.Packet) {
	if len(pkt.Payload)+4 <= mc.MaxFrameSize {
		s.push(mc.PushControlDataResp{SNR: pkt.SNR, RSSI: pkt.RSSI, PathLen: pkt.PathLength, Payload: pkt.Payload}.ToBytes())
	}
}

// advTypes is the firmware's ADV_TYPE_* by the names peers are stored under.
var advTypes = map[string]byte{"CHAT": meshcore.AdvertTypeChat, "REPEATER": meshcore.AdvertTypeRepeater, "ROOM": meshcore.AdvertTypeRoom, "SENSOR": meshcore.AdvertTypeSensor}

var advNames = map[byte]string{meshcore.AdvertTypeChat: "CHAT", meshcore.AdvertTypeRepeater: "REPEATER", meshcore.AdvertTypeRoom: "ROOM", meshcore.AdvertTypeSensor: "SENSOR"}

// peerContact is a heard node as the firmware offers it before it is a contact: no route yet.
func peerContact(p *store.Peer) mc.ContactResponse {
	c := mc.ContactResponse{Type: advTypes[p.Type], OutPathLen: mc.OutPathUnknown, AdvertName: p.Name,
		LastAdvert: p.LastAdvertTS, AdvertLatitude: p.Lat, AdvertLongitude: p.Lon, LastModified: uint32(time.Now().Unix())}
	copy(c.PublicKey[:], p.PubKey)
	return c
}

// storedContact is a contact as RESP_CODE_CONTACT carries it; lastmod is when anything in it last changed, as the firmware bumps it.
func storedContact(k *store.Contact) mc.ContactResponse {
	flags := k.Metadata.TelemPerms << 1
	if k.Metadata.Favourite {
		flags |= mc.ContactFlagFavourite
	}
	c := mc.ContactResponse{Type: advTypes[k.Type], Flags: flags, OutPathLen: mc.OutPathUnknown,
		AdvertName: k.Name, LastAdvert: k.LastAdvertTS, AdvertLatitude: k.Lat, AdvertLongitude: k.Lon, LastModified: uint32(k.ModifiedAt.Unix())}
	copy(c.PublicKey[:], k.PeerPubKey)
	copy(c.OutPath[:], k.OutPath)
	if k.OutPath != nil {
		size := max(k.OutPathHashSize, 1)
		c.OutPathLen = meshcore.MakePathLen(size, uint8(len(k.OutPath)/int(size)))
	}
	return c
}
