package repeater

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/meshpath"
	"github.com/OwlShack/OwlShack/internal/telemetry"
)

func (rm *Client) SendStatusReq(pubkeyHex string, timeout time.Duration) (*Status, error) {
	data, err := rm.sendBinaryRequest(pubkeyHex, meshcore.BuildRequest(meshcore.ReqTypeGetStatus, random4()), timeout, "status")
	if err != nil {
		return nil, err
	}
	return parseRepeaterStatus(data)
}

// SendRoomKeepAlive is fire-and-forget: the firmware honours only a direct keep-alive and streams posts over the DM path.
func (rm *Client) SendRoomKeepAlive(pubkeyHex string, since uint32) error {
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return fmt.Errorf("%w: hex: %w", ErrBadPubkey, err)
	}
	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadPubkey, err)
	}
	peer := rm.node.Peers().Lookup(peerIdentity.PublicKey())
	if peer == nil {
		return ErrUnknownPeer
	}
	rm.mu.Lock()
	sess := rm.sessions[pubkeyHex]
	rm.mu.Unlock()
	if sess == nil || sess.sharedSecret == nil {
		return fmt.Errorf("%w to this room", ErrNotLoggedIn)
	}
	if outPath, _ := learnedRoute(peer); outPath == nil {
		return ErrNoDirectRoute
	}

	ts, release := rm.Stamp()
	defer release()
	plaintext := meshcore.BuildTaggedPlaintext(ts, meshcore.BuildKeepAliveRequest(since))
	encrypted, err := meshcore.EncryptThenMAC(sess.sharedSecret, plaintext)
	if err != nil {
		return fmt.Errorf("encrypting keep-alive: %w", err)
	}
	var mac [2]byte
	copy(mac[:], encrypted[:2])
	peerPub := peerIdentity.PublicKey()
	reqBytes, err := (&meshcore.Request{
		Destination:      peerPub[0],
		Source:           sess.localPubKey[0],
		MAC:              mac,
		EncryptedPayload: encrypted[2:],
	}).ToBytes()
	if err != nil {
		return fmt.Errorf("encoding keep-alive: %w", err)
	}
	pkt, _, _ := rm.routedPacket(peer, meshcore.PayloadTypeReq, reqBytes)
	return meshpath.Send(rm.node, pkt, rm.scopeFor(peerPub[:]), 0)
}

// SendRoomStatusReq is SendStatusReq for a room server, whose ServerStats trailer differs.
func (rm *Client) SendRoomStatusReq(pubkeyHex string, timeout time.Duration) (*Status, error) {
	data, err := rm.sendBinaryRequest(pubkeyHex, meshcore.BuildRequest(meshcore.ReqTypeGetStatus, random4()), timeout, "room status")
	if err != nil {
		return nil, err
	}
	return parseRoomStatus(data)
}

func (rm *Client) SendNeighborsReq(pubkeyHex string, count uint8, offset uint16, timeout time.Duration) (*Neighbors, error) {
	// Strongest first, with the 6-byte prefixes the CLI `neighbors` output shows.
	body := meshcore.BuildNeighboursRequest(meshcore.NeighboursRequest{Count: count, Offset: offset, OrderBy: 2, PrefixLen: 6}, random4())
	data, err := rm.sendBinaryRequest(pubkeyHex, body, timeout, "neighbors")
	if err != nil {
		return nil, err
	}
	return parseRepeaterNeighbors(data, 6)
}

func (rm *Client) SendOwnerInfoReq(pubkeyHex string, timeout time.Duration) (*OwnerInfo, error) {
	data, err := rm.sendBinaryRequest(pubkeyHex, meshcore.BuildRequest(meshcore.ReqTypeGetOwnerInfo, random4()), timeout, "owner")
	if err != nil {
		return nil, err
	}
	return parseRepeaterOwnerInfo(data), nil
}

func (rm *Client) SendAccessListReq(pubkeyHex string, timeout time.Duration) (*AccessList, error) {
	data, err := rm.sendBinaryRequest(pubkeyHex, meshcore.BuildRequest(meshcore.ReqTypeGetAccessList, random4()), timeout, "access list")
	if err != nil {
		return nil, err
	}
	return parseRepeaterAccessList(data), nil
}

// SetAccessPerm needs an admin session; perms 0 revokes and accepts a prefix, granting needs the full key.
func (rm *Client) SetAccessPerm(pubkeyHex, targetPubkeyHex string, perms uint8, timeout time.Duration) error {
	cmd := fmt.Sprintf("setperm %s %d", targetPubkeyHex, perms)
	resp, err := rm.SendCLI(pubkeyHex, cmd, timeout)
	if err != nil {
		return err
	}
	resp = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(resp), ">"))
	resp = strings.TrimSpace(resp)
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("setperm %w: %s", ErrRejected, resp)
	}
	return nil
}

// SendSeriesReq bounds are seconds before the sensor's now, start being the older edge; needs read-only or above.
func (rm *Client) SendSeriesReq(pubkeyHex string, startSecsAgo, endSecsAgo uint32, timeout time.Duration) (*telemetry.Series, error) {
	body := meshcore.BuildAvgMinMaxRequest(meshcore.AvgMinMaxRequest{StartSecsAgo: startSecsAgo, EndSecsAgo: endSecsAgo})
	data, err := rm.sendBinaryRequest(pubkeyHex, body, timeout, "series")
	if err != nil {
		return nil, err
	}
	return badReply(telemetry.ParseSeries(data))
}

// badReply marks a reply that arrived but would not parse, so it reads as a bad answer (502) rather than a fault here.
func badReply[T any](v T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %w", ErrBadReply, err)
	}
	return v, nil
}

// random4 is the random tail that keeps a request's packet hash unique (BaseChatMesh.cpp:697).
func random4() (r [4]byte) {
	rand.Read(r[:])
	return r
}

func (rm *Client) SendTelemetryReq(pubkeyHex string, timeout time.Duration) (*telemetry.Telemetry, error) {
	data, err := rm.sendBinaryRequest(pubkeyHex, meshcore.BuildRequest(meshcore.ReqTypeGetTelemetryData, random4()), timeout, "telemetry")
	if err != nil {
		return nil, err
	}
	return badReply(telemetry.Parse(data))
}

// SendContactTelemetryReq needs no login: it encrypts with the ECDH secret shared with the contact.
func (rm *Client) SendContactTelemetryReq(pubkeyHex string, timeout time.Duration) (*telemetry.Telemetry, error) {
	data, err := rm.contactRequest(pubkeyHex, meshcore.BuildRequest(meshcore.ReqTypeGetTelemetryData, random4()), timeout, "contact-telemetry", &pendingRequest{})
	if err != nil {
		return nil, err
	}
	return badReply(telemetry.Parse(data))
}

// AppRequest sends the MeshCore app's request body as its firmware does, under the contact's own shared secret, and returns the reply after its tag.
func (rm *Client) AppRequest(pubkeyHex string, body []byte, timeout time.Duration, app AppSend) ([]byte, error) {
	return rm.contactRequest(pubkeyHex, body, timeout, "app", &pendingRequest{app: &app})
}

// AppPathDiscovery is the app's path discovery: a flooded base-telemetry request whose reply brings back both paths and leaves the stored route alone.
func (rm *Client) AppPathDiscovery(pubkeyHex string, timeout time.Duration, app AppSend) (out, in PathBytes, err error) {
	body := meshcore.BuildTelemetryRequest(meshcore.TelemPermBase, random4())
	paths := make(chan [2]PathBytes, 1)
	pr := &pendingRequest{app: &app, discovery: true, onPath: func(o, i PathBytes) {
		select {
		case paths <- [2]PathBytes{o, i}: // a repeat of the return finds it full and is dropped, never blocking RX
		default:
		}
	}}
	if _, err = rm.contactRequest(pubkeyHex, body, timeout, "path-discovery", pr); err != nil {
		return out, in, err
	}
	select {
	case p := <-paths:
		return p[0], p[1], nil
	default:
		return out, in, ErrNoPath
	}
}

// AppCLI sends the app's CLI text as the firmware's sendCommandData does: under the contact's own secret with no login of ours, its ACL deciding; the reply comes back as a received message.
func (rm *Client) AppCLI(pubkeyHex, text string, txtType, attempt byte, timeout time.Duration, app AppSend) error {
	if len(text) > meshcore.MaxTextLen {
		return node.ErrTextTooLong
	}
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return fmt.Errorf("%w: hex: %w", ErrBadPubkey, err)
	}
	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadPubkey, err)
	}
	peer := rm.node.Peers().Lookup(peerIdentity.PublicKey())
	if peer == nil {
		return ErrUnknownPeer
	}
	secret, err := rm.node.SharedSecret(peerIdentity)
	if err != nil {
		return fmt.Errorf("deriving shared secret: %w", err)
	}
	ts, release := rm.Stamp()
	defer release()
	plaintext := meshcore.BuildTextPlaintext(time.Unix(int64(ts), 0), txtType<<2|attempt&3, []byte(text))
	msg, err := meshcore.NewTextMessage(rm.node.Identity(), peerIdentity, plaintext, secret)
	if err != nil {
		return fmt.Errorf("encrypting CLI: %w", err)
	}
	msgBytes, err := msg.ToBytes()
	if err != nil {
		return fmt.Errorf("encoding CLI: %w", err)
	}
	pkt, outPath, hashSize := rm.routedPacket(peer, meshcore.PayloadTypeTxtMsg, msgBytes)
	if err := meshpath.Send(rm.node, pkt, app.Scope, 0); err != nil {
		return fmt.Errorf("sending CLI: %w", err)
	}
	app.Sent(0, rm.replyTimeout(len(msgBytes), outPath, hashSize, timeout), outPath == nil)
	return nil
}

// ErrNoPath is a path discovery answered without the path return that carries both routes.
var ErrNoPath = errors.New("the node answered without a path return, so no route was learned")

func (rm *Client) contactRequest(pubkeyHex string, body []byte, timeout time.Duration, label string, pr *pendingRequest) ([]byte, error) {
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return nil, fmt.Errorf("%w: hex: %w", ErrBadPubkey, err)
	}
	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadPubkey, err)
	}
	peer := rm.node.Peers().Lookup(peerIdentity.PublicKey())
	if peer == nil {
		return nil, ErrUnknownPeer
	}
	sharedSecret, err := rm.node.SharedSecret(peerIdentity)
	if err != nil {
		return nil, fmt.Errorf("deriving shared secret: %w", err)
	}
	pr.sharedSecret, pr.peerPubKey = sharedSecret, peerIdentity.PublicKey()
	return rm.roundtripRequest(peerIdentity.PublicKey(), peer, sharedSecret, rm.node.Identity().PublicKey()[0], body, timeout, label, pr)
}

// ErrRegionLoad: the firmware reads load-mode lines before stripping our "XX|" tag, so none parse and it swallows every later command until rebooted.
var ErrRegionLoad = errors.New("region load can't run over the mesh: the repeater would take every later command as a region line until it restarts; use region put or region def")

func (rm *Client) SendCLI(pubkeyHex, command string, timeout time.Duration) (string, error) {
	if f := strings.Fields(command); len(f) >= 2 && f[0] == "region" && f[1] == "load" {
		return "", ErrRegionLoad
	}
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return "", fmt.Errorf("%w: hex: %w", ErrBadPubkey, err)
	}

	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadPubkey, err)
	}

	peer := rm.node.Peers().Lookup(peerIdentity.PublicKey())
	if peer == nil {
		return "", ErrUnknownPeer
	}

	rm.mu.Lock()
	sess := rm.sessions[pubkeyHex]
	rm.mu.Unlock()
	if sess == nil || sess.sharedSecret == nil {
		return "", fmt.Errorf("%w to this repeater", ErrNotLoggedIn)
	}
	if !sess.IsAdmin {
		return "", ErrNotAdmin
	}

	resultCh := make(chan string, 1)
	var prefix string
	rm.cliMu.Lock()
	// The prefix is one hex byte, so a full map would spin the random search below forever holding cliMu.
	if len(rm.cliPending) >= 256 {
		rm.cliMu.Unlock()
		return "", ErrBusy
	}
	for {
		var b [1]byte
		rand.Read(b[:])
		prefix = fmt.Sprintf("%02X", b[0])
		if _, taken := rm.cliPending[prefix]; !taken {
			break
		}
	}
	rm.cliPending[prefix] = resultCh
	rm.cliMu.Unlock()
	framedCommand := prefix + "|" + command

	defer func() {
		rm.cliMu.Lock()
		delete(rm.cliPending, prefix)
		rm.cliMu.Unlock()
	}()

	ts, release := rm.Stamp()
	defer release()
	plaintext := meshcore.BuildTextPlaintext(time.Unix(int64(ts), 0), meshcore.TxtTypeCLIData<<2, []byte(framedCommand))

	encrypted, err := meshcore.EncryptThenMAC(sess.sharedSecret, plaintext)
	if err != nil {
		return "", fmt.Errorf("encrypting CLI: %w", err)
	}

	var mac [2]byte
	copy(mac[:], encrypted[:2])

	txtMsg := &meshcore.TextMessage{
		Destination:      peerIdentity.PublicKey()[0],
		Source:           sess.localPubKey[0],
		MAC:              mac,
		EncryptedPayload: encrypted[2:],
	}

	msgBytes, err := txtMsg.ToBytes()
	if err != nil {
		return "", fmt.Errorf("encoding text message: %w", err)
	}

	pkt, outPath, hashSize := rm.routedPacket(peer, meshcore.PayloadTypeTxtMsg, msgBytes)

	pub := peer.Identity.PublicKey()
	err = meshpath.Send(rm.node, pkt, rm.scopeFor(pub[:]), 0)
	release()
	if err != nil {
		return "", fmt.Errorf("sending CLI: %w", err)
	}

	wait := rm.replyTimeout(len(msgBytes), outPath, hashSize, timeout)
	rm.log.Debug("CLI sent", "peer", pubkeyHex[:12], "prefix", prefix, "command", command, "wait", wait)

	select {
	case response := <-resultCh:
		return response, nil
	case <-time.After(wait):
		return "", fmt.Errorf("CLI command timed out after %s: %w", wait, ErrNoReply)
	}
}
