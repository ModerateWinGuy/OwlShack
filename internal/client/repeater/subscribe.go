package repeater

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"

	"github.com/OwlShack/OwlShack/internal/telemetry"
)

// ErrSubscribeRefused is a sensor's expiry of 0: it pushes only within a region, and a request sent without one needs its default region.
var ErrSubscribeRefused = errors.New("the sensor refused to push: set a default region on it (its Regions tab)")

// TelemetryPushHandler gets a subscribed sensor's readings each time one moves past its threshold.
type TelemetryPushHandler func(pubkeyHex string, t *telemetry.Telemetry)

// SetTelemetryPushHandler sets where pushes go; nil drops them.
func (rm *Client) SetTelemetryPushHandler(f TelemetryPushHandler) {
	rm.subsMu.Lock()
	rm.onPush = f
	rm.subsMu.Unlock()
}

// SendSubscribe asks a firmware 1.18+ sensor to push its telemetry whenever a reading moves by its delta (SensorMesh.cpp:213); it needs a login.
func (rm *Client) SendSubscribe(pubkeyHex string, timeoutSecs uint16, deltas []meshcore.LPPDataValue, timeout time.Duration) (meshcore.SubscribeReply, error) {
	var tagB [4]byte
	var tag uint32
	for tag == 0 {
		tagB = random4()
		tag = binary.LittleEndian.Uint32(tagB[:])
	}
	body, err := meshcore.BuildSubscribeRequest(meshcore.SubscribeRequest{PushTag: tag, TimeoutSecs: timeoutSecs, MinDeltas: deltas})
	if err != nil {
		return meshcore.SubscribeReply{}, fmt.Errorf("%w: %w", ErrRejected, err)
	}
	var secret []byte
	if sess := rm.Session(pubkeyHex); sess != nil {
		secret = sess.sharedSecret
	}
	// The sensor takes the tag before it answers (SensorMesh.cpp:214), so a lost reply must not lose its pushes.
	rm.subsMu.Lock()
	for t, s := range rm.subs {
		if time.Now().After(s.Expires) {
			delete(rm.subs, t)
		}
	}
	rm.subs[tag] = Subscription{pubkeyHex: pubkeyHex, secret: secret, tentative: true, Expires: time.Now().Add(time.Duration(timeoutSecs) * time.Second)}
	rm.subsMu.Unlock()

	data, err := rm.sendBinaryRequest(pubkeyHex, body, timeout, "subscribe")
	if err != nil {
		return meshcore.SubscribeReply{}, err
	}
	reply, err := badReply(meshcore.ParseSubscribeReply(data))
	if err != nil {
		return reply, err
	}
	rm.subsMu.Lock()
	defer rm.subsMu.Unlock()
	old, had := rm.dropSubsLocked(pubkeyHex)
	rm.refused[pubkeyHex] = reply.ExpirySecs == 0
	if reply.ExpirySecs == 0 {
		// A refusal keeps the running watch, now under the new tag.
		if had {
			rm.subs[tag] = old
		}
		return reply, fmt.Errorf("%w: %w", ErrRejected, ErrSubscribeRefused)
	}
	rm.subs[tag] = Subscription{pubkeyHex: pubkeyHex, secret: secret, Expires: time.Now().Add(time.Duration(reply.ExpirySecs) * time.Second), Scope: reply.Scope}
	return reply, nil
}

// Subscription is a sensor watch this client holds; the sensor keeps one per client.
type Subscription struct {
	pubkeyHex string
	// secret is the static shared secret pushes are encrypted with, so a logout doesn't lose them.
	secret []byte
	// tentative is a tag sent but not yet answered, which the sensor may already push under.
	tentative bool
	Expires   time.Time
	Scope     string
}

// SubscribeRefused reports whether the sensor refused this client's last subscribe.
func (rm *Client) SubscribeRefused(pubkeyHex string) bool {
	rm.subsMu.Lock()
	defer rm.subsMu.Unlock()
	return rm.refused[pubkeyHex]
}

// ActiveSubscription is this client's unexpired, answered watch on the sensor, if any.
func (rm *Client) ActiveSubscription(pubkeyHex string) (Subscription, bool) {
	rm.subsMu.Lock()
	defer rm.subsMu.Unlock()
	for _, s := range rm.subs {
		if s.pubkeyHex == pubkeyHex && !s.tentative && time.Now().Before(s.Expires) {
			return s, true
		}
	}
	return Subscription{}, false
}

// Watching reports whether this client holds any unexpired tag for the sensor, answered or not, so a stop covers a watch whose reply was lost.
func (rm *Client) Watching(pubkeyHex string) bool {
	rm.subsMu.Lock()
	defer rm.subsMu.Unlock()
	for _, s := range rm.subs {
		if s.pubkeyHex == pubkeyHex && time.Now().Before(s.Expires) {
			return true
		}
	}
	return false
}

// dropSubsLocked forgets every tag for the sensor and returns its answered watch, if any.
func (rm *Client) dropSubsLocked(pubkeyHex string) (Subscription, bool) {
	var (
		old Subscription
		had bool
	)
	for t, s := range rm.subs {
		if s.pubkeyHex != pubkeyHex {
			continue
		}
		if !s.tentative {
			old, had = s, true
		}
		delete(rm.subs, t)
	}
	return old, had
}

// SendUnsubscribe ends this client's subscription on the sensor; pushes are ignored from the moment it is asked, answered or not.
func (rm *Client) SendUnsubscribe(pubkeyHex string, timeout time.Duration) error {
	rm.subsMu.Lock()
	rm.dropSubsLocked(pubkeyHex)
	delete(rm.refused, pubkeyHex)
	rm.subsMu.Unlock()
	data, err := rm.sendBinaryRequest(pubkeyHex, []byte{meshcore.ReqTypeUnsubscribe}, timeout, "unsubscribe")
	if err != nil {
		return err
	}
	_, err = badReply(struct{}{}, meshcore.ParseUnsubscribeReply(data))
	return err
}

// pushWithoutSession takes a push that no session decrypted, as after a logout, with the secret its watch kept.
func (rm *Client) pushWithoutSession(resp *meshcore.Response) {
	rm.subsMu.Lock()
	var cands []Subscription
	for _, s := range rm.subs {
		if s.secret != nil && len(s.pubkeyHex) >= 2 && s.pubkeyHex[:2] == fmt.Sprintf("%02x", resp.Source) {
			cands = append(cands, s)
		}
	}
	rm.subsMu.Unlock()
	for _, s := range cands {
		if !resp.VerifyMAC(s.secret) {
			continue
		}
		plaintext := resp.Decrypt(s.secret)
		if len(plaintext) >= 4 {
			rm.handlePush(s.pubkeyHex, binary.LittleEndian.Uint32(plaintext[:4]), plaintext[4:])
		}
		return
	}
}

// handlePush delivers a response no request waits for when its tag is a subscription of the sensor it decrypted under.
func (rm *Client) handlePush(pubkeyHex string, tag uint32, data []byte) {
	rm.subsMu.Lock()
	sub, ok := rm.subs[tag]
	f := rm.onPush
	rm.subsMu.Unlock()
	if !ok || sub.pubkeyHex != pubkeyHex {
		return
	}
	push, err := meshcore.ParseTelemetryPush(data)
	if err != nil {
		rm.log.Debug("telemetry push unreadable", "peer", pubkeyHex[:12], "error", err)
		return
	}
	t, err := telemetry.Parse(push.LPP)
	if err != nil {
		rm.log.Debug("telemetry push unreadable", "peer", pubkeyHex[:12], "error", err)
		return
	}
	if f != nil {
		f(pubkeyHex, t)
	}
}
