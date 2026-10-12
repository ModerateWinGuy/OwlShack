package companion

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/meshpath"
	"github.com/OwlShack/OwlShack/internal/store"
)

// MaxDMTextBytes is the longest DM that every retry still fits, so a message that sends can also be retried.
const MaxDMTextBytes = meshcore.MaxRetryTextLen

// ErrUnknownChannel is a send to a channel this companion doesn't have.
var ErrUnknownChannel = errors.New("channel not found")

// SendScope is the region a send floods in: Label is what its message is stored under, Region the key it goes out with, nil unscoped.
type SendScope struct {
	Label  config.FloodScope
	Region *meshcore.Region
}

func scopeOf(s config.FloodScope) SendScope { return SendScope{Label: s, Region: s.MeshRegion()} }

func (c *Companion) SendChannelMessage(channelName, text string) error {
	ch := c.findChannel(channelName)
	if ch == nil {
		return fmt.Errorf("%w: %q", ErrUnknownChannel, channelName)
	}
	return c.sendGroupReply(ch, text, c.pathHashSize(), 5*time.Second, 3, c.channelScope(ch.Name))
}

func (c *Companion) sendGroupReply(ch *meshcore.ChannelEntry, text string, hashSize uint8, retryTimeout time.Duration, maxRetries int, scope config.FloodScope) error {
	return c.sendGroup(ch, text, hashSize, retryTimeout, maxRetries, scopeOf(scope))
}

// sendGroup is the shared send path for the chat API, the app and trigger replies, so every post is persisted, broadcast and echo-tracked alike.
func (c *Companion) sendGroup(ch *meshcore.ChannelEntry, text string, hashSize uint8, retryTimeout time.Duration, maxRetries int, scope SendScope) error {
	name := c.conf().Name
	payload := &meshcore.GroupTextPayload{
		Timestamp: c.node.UniqueTimestamp(),
		Sender:    name,
		Text:      text,
	}

	now := time.Now()
	msg := &store.Message{
		CompanionID: c.conf().ID,
		Channel:     ch.Name,
		ChannelHash: ch.Hash,
		Sender:      name,
		Text:        text,
		Direction:   "tx",
		Timestamp:   now,
		ReceivedAt:  now,
		FloodScope:  string(scope.Label),
	}

	c.store.WriteSync(func() {
		if insertErr := c.store.Messages.Insert(context.Background(), msg); insertErr != nil {
			c.log.Error("failed to persist outgoing message", "error", insertErr)
		}
	})

	msgID := msg.ID

	if c.hub != nil {
		c.hub.Broadcast("messages", map[string]any{
			"companion":   name,
			"companionId": c.conf().ID,
			"channel":     ch.Name,
			"sender":      name,
			"text":        text,
			"direction":   "tx",
			"timestamp":   msg.Timestamp.UTC().Format(time.RFC3339),
			"receivedAt":  msg.ReceivedAt.UTC().Format(time.RFC3339),
			"id":          msgID,
			"floodScope":  msg.FloodScope,
		})
	}

	c.pendingOutbound.Lock()
	c.pendingOutbound.msgID = msgID
	c.pendingOutbound.channel = ch.Name
	c.pendingOutbound.Unlock()

	err := c.node.SendGroupTextScoped(
		ch, scope.Region, payload, hashSize, retryTimeout, maxRetries,
		func(gsr node.GroupSendResult) {
			c.log.Debug("group reply result", "channel", ch.Name, "confirmed", gsr.Confirmed)
		},
	)
	if err != nil { // never sent, so the row already shown must not read as sent, nor take an earlier post's echoes
		c.pendingOutbound.Lock()
		unhanded := c.pendingOutbound.msgID == msgID
		if unhanded {
			c.pendingOutbound.msgID, c.pendingOutbound.channel = 0, ""
		}
		c.pendingOutbound.Unlock()
		if unhanded { // one the radio took is settled by its transmit result
			c.setMessageStatus(msgID, ch.Name, "failed")
		}
	}
	return err
}

// setMessageStatus records an outgoing message's status and tells the open chats.
func (c *Companion) setMessageStatus(msgID int64, channel, status string) {
	c.store.WriteAsync(func() {
		if err := c.store.Messages.UpdateStatus(context.Background(), msgID, status); err != nil {
			c.log.Error("failed to update message status", "id", msgID, "error", err)
		}
	})
	if c.hub != nil {
		c.hub.Broadcast("messages", map[string]any{
			"action":      "status",
			"companion":   c.conf().Name,
			"companionId": c.conf().ID,
			"channel":     channel,
			"id":          msgID,
			"status":      status,
		})
	}
}

// SendContactMessage is the chat API's DM send: a flood or 0-hop DM goes out at the contact's bytes per hop.
func (c *Companion) SendContactMessage(pubkeyHex, text string) error {
	_, err := c.sendDM(pubkeyHex, text, c.bytesPerHopHex(pubkeyHex), 5*time.Second, scopeOf(c.contactScopeHex(pubkeyHex)), nil)
	return err
}

// sendDMReply is a DM trigger's answer: the trigger's pathHashSize frames it only when no route is stored, since a stored path already fixes its own hash width.
func (c *Companion) sendDMReply(pubkeyHex, text string, hashSize uint8, ackTimeout time.Duration, scope config.FloodScope) error {
	_, err := c.sendDM(pubkeyHex, text, hashSize, ackTimeout, scopeOf(scope), nil)
	return err
}

// dmAckTimeout mirrors the firmware's calcFloodTimeoutMillisFor / calcDirectTimeoutMillisFor
// (MyMesh.cpp:851-858): the wait has to scale with airtime and hop count or a slow preset gives up
// before the ack can physically arrive. floor keeps the caller's configured value as a minimum,
// and is used outright when the radio params are unknown and airtime reads 0.
func (c *Companion) dmAckTimeout(textLen int, outPath []byte, hashSize uint8, floor time.Duration) time.Duration {
	if c.stats == nil {
		return floor
	}
	// [4 ts][1 flags][text] padded to an AES block, plus dest+src+MAC, header and path-length byte.
	cipherLen := 5 + textLen
	if rem := cipherLen % 16; rem != 0 {
		cipherLen += 16 - rem
	}
	airtime := c.stats.EstAirtimeMs(2 + len(outPath) + 4 + cipherLen)
	if airtime == 0 {
		return floor
	}

	timeout := node.CalcFloodTimeout(airtime)
	if outPath != nil { // non-nil, even empty, routes direct
		hops := 0
		if hashSize > 0 {
			hops = len(outPath) / int(hashSize)
		}
		timeout = node.CalcDirectTimeout(airtime, uint8(min(hops, 255)))
	}
	return max(timeout, floor)
}

// sendDM stores, shows and sends a DM; done, when set, also hears the outcome.
func (c *Companion) sendDM(pubkeyHex, text string, fallbackHashSize uint8, ackTimeout time.Duration, scope SendScope, done func(node.DMSendResult)) (AppDMSend, error) {
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return AppDMSend{}, fmt.Errorf("invalid pubkey hex: %w", err)
	}

	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return AppDMSend{}, fmt.Errorf("invalid pubkey: %w", err)
	}

	// The UI counts characters; the wire counts bytes, and a retry past attempt 3 appends 2 more.
	if len(text) > MaxDMTextBytes {
		return AppDMSend{}, fmt.Errorf("%w: message is %d bytes, over the %d-byte limit (multibyte characters cost more than one)", node.ErrTextTooLong, len(text), MaxDMTextBytes)
	}

	// A nil path floods, so an unrouted contact still sends.
	outPath, hashSize, haveRoute := c.learnedRoute(pubkeyBytes)
	if !haveRoute {
		outPath = nil // flood
	}
	// Nothing to frame on a flood or a 0-hop neighbour, so the caller's width decides the header.
	if len(outPath) == 0 {
		hashSize = fallbackHashSize
	}

	channelKey := "dm:" + pubkeyHex
	statusSending := "sending"

	now := time.Now()
	msg := &store.Message{
		CompanionID: c.conf().ID,
		Channel:     channelKey,
		ChannelHash: 0,
		Sender:      c.conf().Name,
		Text:        text,
		Direction:   "tx",
		Timestamp:   now,
		ReceivedAt:  now,
		Status:      &statusSending,
		FloodScope:  string(scope.Label),
	}

	c.store.WriteSync(func() {
		if insertErr := c.store.Messages.Insert(context.Background(), msg); insertErr != nil {
			c.log.Error("failed to persist outgoing DM", "error", insertErr)
		}
	})

	if c.hub != nil {
		c.hub.Broadcast("messages", map[string]any{
			"companion":   c.conf().Name,
			"companionId": c.conf().ID,
			"channel":     channelKey,
			"sender":      c.conf().Name,
			"text":        text,
			"direction":   "tx",
			"timestamp":   msg.Timestamp.UTC().Format(time.RFC3339),
			"receivedAt":  msg.ReceivedAt.UTC().Format(time.RFC3339),
			"id":          msg.ID,
			"status":      "sending",
			"floodScope":  msg.FloodScope,
		})
	}

	msgID := msg.ID
	settle := func(result node.DMSendResult) bool {
		if !c.dmsInFlight.end(msgID) {
			return false
		}
		status := "failed"
		if result.Confirmed {
			status = "delivered"
			c.log.Debug("DM delivered", "peer", pubkeyHex[:12], "roundTrip", result.RoundTrip)
		}
		c.setMessageStatus(msgID, channelKey, status)
		if done != nil {
			done(result)
		}
		return true
	}
	c.dmsInFlight.begin(msgID, func() { settle(node.DMSendResult{}) })
	giveUp := func() {
		if !settle(node.DMSendResult{}) {
			return
		}
		c.log.Warn("DM delivery failed", "peer", pubkeyHex[:12])
		// Every attempt failed on a route we believed in, so drop it as the app's CMD_RESET_PATH does; the next path return re-learns it.
		if haveRoute {
			c.node.Peers().ResetOutPath(peerIdentity.PublicKey())
			c.store.WriteAsync(func() {
				if err := c.store.Contacts.UpdateOutPath(context.Background(), c.conf().ID, pubkeyBytes, nil, 0); err != nil {
					c.log.Error("failed to clear stale route", "peer", pubkeyHex[:12], "error", err)
				}
			})
			c.log.Info("cleared stale route after failed delivery", "peer", pubkeyHex[:12])
		}
	}

	// Firmware's sendMessage per attempt, as the app retries: direct ones first when routed, then floods.
	attempts, direct := dmUnroutedAttempts, 0
	if outPath != nil {
		attempts, direct = dmAttempts, dmDirectAttempts
	}
	a := node.TextAttempt{Peer: peerIdentity, Scope: scope.Region, Text: []byte(text)}
	var send func(attempt int) error
	send = func(attempt int) error {
		a.Attempt, a.Path, a.PathHashSize = attempt, nil, fallbackHashSize
		if attempt < direct {
			a.Path, a.PathHashSize = outPath, hashSize
		}
		sent, err := c.node.SendTextAttempt(a, nil)
		if err != nil {
			return err
		}
		c.sentAcks.add(sent.ACK, peerIdentity)
		wait := max(sent.Timeout, ackTimeout)
		c.log.Debug("DM attempt sent", "peer", pubkeyHex[:12], "attempt", attempt, "flood", sent.Flood, "estTimeout", sent.Timeout, "wait", wait)
		start := time.Now()
		c.node.ExpectACK(sent.ACK, wait, func(time.Duration) {
			settle(node.DMSendResult{Confirmed: true, RoundTrip: time.Since(start)})
		}, func() {
			if attempt+1 >= attempts {
				giveUp()
				return
			}
			if err := send(attempt + 1); err != nil {
				c.log.Warn("DM retry refused", "peer", pubkeyHex[:12], "attempt", attempt+1, "error", err)
				settle(node.DMSendResult{})
			}
		})
		return nil
	}
	var sentAt uint32
	err = c.node.SendStamped(func(ts uint32) error {
		sentAt, a.Timestamp = ts, ts
		return send(0)
	})
	if err != nil { // never sent, so no attempt will report
		settle(node.DMSendResult{})
		return AppDMSend{}, err
	}
	total := time.Duration(attempts-direct) * c.dmAckTimeout(len(text), nil, fallbackHashSize, ackTimeout)
	if direct > 0 {
		total += time.Duration(direct) * c.dmAckTimeout(len(text), outPath, hashSize, ackTimeout)
	}
	resend := func(attempt int, done func(node.DMSendResult)) (AppDMSend, error) {
		return c.resendDM(peerIdentity, text, sentAt, attempt, msgID, channelKey, fallbackHashSize, scope, done)
	}
	return AppDMSend{Flood: outPath == nil, Timeout: total, Resend: resend}, nil
}

// A routed DM tries dmDirectAttempts times along its route then floods up to dmAttempts in all; an unrouted one floods dmUnroutedAttempts times.
const (
	dmAttempts         = 5
	dmDirectAttempts   = 3
	dmUnroutedAttempts = 3
)

// dmsInFlight settles each DM once, and fails those still waiting when the node stops, which ends their ACK waits unreported.
type dmsInFlight struct {
	sync.Mutex
	abort map[int64]func()
}

func (d *dmsInFlight) begin(id int64, abort func()) {
	d.Lock()
	defer d.Unlock()
	if d.abort == nil {
		d.abort = map[int64]func(){}
	}
	d.abort[id] = abort
}

// end reports whether id was still in flight, so only its first outcome counts.
func (d *dmsInFlight) end(id int64) bool {
	d.Lock()
	defer d.Unlock()
	_, ok := d.abort[id]
	delete(d.abort, id)
	return ok
}

func (d *dmsInFlight) abortAll() {
	d.Lock()
	fns := slices.Collect(maps.Values(d.abort))
	d.Unlock()
	for _, f := range fns {
		f()
	}
}

// sentAcks is the firmware's expected_ack_table (MyMesh.h:303): the last 8 DM attempts' ACK CRCs and their recipients.
type sentAcks struct {
	sync.Mutex
	next int
	tab  [8]struct {
		crc  uint32
		peer meshcore.Identity
	}
}

func (s *sentAcks) add(crc uint32, peer meshcore.Identity) {
	s.Lock()
	defer s.Unlock()
	s.tab[s.next].crc, s.tab[s.next].peer = crc, peer
	s.next = (s.next + 1) % len(s.tab)
}

// take clears and returns the recipient of crc, as the firmware's processAck clears a matched entry.
func (s *sentAcks) take(crc uint32) (meshcore.Identity, bool) {
	s.Lock()
	defer s.Unlock()
	for i := range s.tab {
		if e := &s.tab[i]; e.crc == crc && crc != 0 {
			e.crc = 0
			return e.peer, true
		}
	}
	return meshcore.Identity{}, false
}

func (c *Companion) SendTrace(path []byte, pathHashSize uint8) (uint32, error) {
	if len(path) == 0 {
		return 0, fmt.Errorf("path is required")
	}
	if pathHashSize != 1 && pathHashSize != 2 && pathHashSize != 4 {
		return 0, fmt.Errorf("pathHashSize must be 1, 2, or 4")
	}
	if len(path)%int(pathHashSize) != 0 {
		return 0, fmt.Errorf("path length %d is not divisible by pathHashSize %d", len(path), pathHashSize)
	}

	var tagBytes [4]byte
	if _, err := rand.Read(tagBytes[:]); err != nil {
		return 0, fmt.Errorf("generating trace tag: %w", err)
	}
	tag := binary.LittleEndian.Uint32(tagBytes[:])

	var authBytes [4]byte
	if _, err := rand.Read(authBytes[:]); err != nil {
		return 0, fmt.Errorf("generating trace auth: %w", err)
	}
	auth := binary.LittleEndian.Uint32(authBytes[:])

	if err := c.sendTracePacket(tag, auth, path, pathHashSize); err != nil {
		return 0, err
	}
	return tag, nil
}

// sendTracePacket is split out of SendTrace so RunTrace can register its waiter under the tag before the packet hits the radio.
func (c *Companion) sendTracePacket(tag, auth uint32, path []byte, pathHashSize uint8) error {
	var flags byte
	switch pathHashSize {
	case 1:
		flags = 0
	case 2:
		flags = 1
	case 4:
		flags = 2
	case 8:
		flags = 3
	}

	trace := &meshcore.Trace{
		Tag:        tag,
		AuthCode:   auth,
		Flags:      flags,
		PathHashes: path,
	}

	payload, err := trace.ToBytes()
	if err != nil {
		return fmt.Errorf("encoding trace: %w", err)
	}

	pkt := meshcore.Packet{
		Header:     meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTrace, 0),
		PathLength: 0,
		Payload:    payload,
	}

	if err := meshpath.Send(c.node, &pkt, nil, 0); err != nil {
		return fmt.Errorf("sending trace: %w", err)
	}

	c.log.Debug("trace sent", "tag", fmt.Sprintf("%08x", tag), "hops", len(path)/int(pathHashSize), "pathHashSize", pathHashSize)
	return nil
}

func (c *Companion) findChannel(name string) *meshcore.ChannelEntry {
	for _, ch := range c.node.Channels() {
		if ch.Name == name {
			return ch
		}
	}
	return nil
}
