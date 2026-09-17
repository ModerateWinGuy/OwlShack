package trigger

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/meshcore-go/OwlShack/internal/config"
	"github.com/meshcore-go/OwlShack/internal/logging"
	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/node"
)

type ChannelTrigger struct {
	cfg      config.TriggerConfig
	botName  string
	node     *node.Node
	patterns []*regexp.Regexp
	channels map[string]bool // channel names this trigger listens on; nil = all
	log      *slog.Logger

	mu          sync.Mutex
	callback    Callback
	ctx         context.Context
	stopContext func() bool
	pending     map[groupRequest]*pendingReply
}

type groupRequest struct {
	channel, sender, text string
	timestamp             uint32
}

type pendingReply struct {
	pattern    *regexp.Regexp
	deadline   time.Time
	timer      *time.Timer
	suppressed bool
}

func NewChannelTrigger(botName string, cfg config.TriggerConfig, n *node.Node, channels []*meshcore.ChannelEntry, log *slog.Logger) (*ChannelTrigger, error) {
	patterns, err := compilePatterns(cfg.Match)
	if err != nil {
		return nil, err
	}

	var channelFilter map[string]bool
	if len(channels) > 0 {
		channelFilter = make(map[string]bool, len(channels))
		for _, ch := range channels {
			channelFilter[ch.Name] = true
		}
	}

	return &ChannelTrigger{
		cfg:      cfg,
		botName:  botName,
		node:     n,
		patterns: patterns,
		channels: channelFilter,
		log:      log.With("trigger", "channel"),
	}, nil
}

// Group text arrives via the companion's persistent handler; node.OnPacket cannot deregister.
func (t *ChannelTrigger) Start(ctx context.Context, callback Callback) error {
	t.mu.Lock()
	t.ctx = ctx
	t.callback = callback
	t.pending = make(map[groupRequest]*pendingReply)
	t.stopContext = context.AfterFunc(ctx, func() { t.Stop() })
	t.mu.Unlock()
	return nil
}

// Stop cancels pending replies and waits for any committed callback to finish.
func (t *ChannelTrigger) Stop() error {
	t.mu.Lock()
	t.callback = nil
	if t.stopContext != nil {
		t.stopContext()
	}
	if len(t.pending) > 0 {
		t.log.Debug("cancelling pending failover replies", "count", len(t.pending))
	}
	for key, pending := range t.pending {
		pending.timer.Stop()
		delete(t.pending, key)
	}
	t.mu.Unlock()
	return nil
}

// HandleGroupText is invoked by the companion's persistent GrpTxt handler for every received group message.
func (t *ChannelTrigger) HandleGroupText(pkt *meshcore.Packet) {
	t.mu.Lock()
	cb := t.callback
	t.mu.Unlock()
	if cb == nil {
		return // stopped or not yet started
	}

	msg, ch, err := t.node.DecryptGroupText(pkt)
	if err != nil {
		t.log.Log(context.Background(), logging.LevelTrace, "group decrypt failed", "error", err)
		return
	}
	t.handleGroupText(msg, ch, pkt)
}

func (t *ChannelTrigger) handleGroupText(msg *meshcore.GroupTextPayload, ch *meshcore.ChannelEntry, pkt *meshcore.Packet) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.callback == nil || t.ctx.Err() != nil {
		return
	}

	t.log.Log(context.Background(), logging.LevelTrace, "group message received",
		"channel", ch.Name, "sender", msg.Sender,
		"text", msg.Text, "snr", pkt.SNR, "rssi", pkt.RSSI)

	// Our own sends are heard back over the air; matching them would let a bot trigger on itself and loop.
	if msg.Sender == t.botName {
		t.log.Log(context.Background(), logging.LevelTrace, "own message, skipping trigger",
			"channel", ch.Name)
		return
	}

	if t.channels != nil && !t.channels[ch.Name] {
		t.log.Log(context.Background(), logging.LevelTrace, "channel not matched, skipping",
			"received", ch.Name, "listening", t.channelNames())
		return
	}

	now := time.Now()
	for key, pending := range t.pending {
		if !pending.suppressed && now.Before(pending.deadline) && key.channel == ch.Name && key.sender != msg.Sender && pending.pattern.MatchString(msg.Text) {
			pending.suppressed = true
			t.log.Debug("failover reply suppressed", "channel", ch.Name, "requester", key.sender, "responder", msg.Sender)
		}
	}

	captures := t.matchesAny(msg.Text)
	if captures == nil {
		t.log.Log(context.Background(), logging.LevelTrace, "no pattern matched",
			"channel", ch.Name, "text", msg.Text, "patterns", t.patternStrings())
		return
	}

	t.log.Log(context.Background(), logging.LevelTrace, "trigger matched", "captures", captures)

	evt := Event{
		Type:    "channel",
		BotName: t.botName,
		Data: map[string]any{
			"Sender":       msg.Sender,
			"Channel":      ch.Name,
			"ChannelEntry": ch,
			"Message":      msg.Text,
			"Match":        captures,
			"Timestamp":    msg.Timestamp,
			"SNR":          pkt.SNR,
			"RSSI":         pkt.RSSI,
			"Hops":         pkt.PathHashCount(),
			"PathHashes":   pkt.PathHashes(),
			"PathHashSize": pkt.PathHashSize(),
		},
	}
	if t.cfg.FailoverPattern == "" {
		t.callback(evt)
		return
	}
	key := groupRequest{ch.Name, msg.Sender, msg.Text, msg.Timestamp}
	if _, exists := t.pending[key]; exists {
		return
	}
	if len(t.pending) >= 256 {
		t.log.Warn("failover pending limit reached", "channel", ch.Name)
		return
	}
	pattern, err := t.cfg.FailoverRegexp(msg.Sender)
	if err != nil {
		t.log.Error("failover pattern error", "error", err)
		return
	}
	wait := time.Duration(t.cfg.FailoverTimeout) * time.Second
	pending := &pendingReply{pattern: pattern, deadline: now.Add(wait)}
	t.pending[key] = pending
	pending.timer = time.AfterFunc(time.Until(pending.deadline), func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.pending[key] != pending {
			return
		}
		delete(t.pending, key)
		if pending.suppressed || t.callback == nil || t.ctx.Err() != nil {
			return
		}
		t.log.Debug("failover deadline expired, replying", "channel", key.channel, "sender", key.sender)
		t.callback(evt)
	})
	t.log.Debug("waiting for failover response", "channel", ch.Name, "sender", msg.Sender, "wait", wait)
}

func (t *ChannelTrigger) matchesAny(text string) map[string]string {
	captures := matchCaptures(t.patterns, text)
	t.log.Log(context.Background(), logging.LevelTrace, "regex check",
		"text", text, "patterns", t.patternStrings(), "matched", captures != nil)
	return captures
}

func (t *ChannelTrigger) channelNames() []string {
	names := make([]string, 0, len(t.channels))
	for name := range t.channels {
		names = append(names, name)
	}
	return names
}

func (t *ChannelTrigger) patternStrings() []string {
	return patternStrings(t.patterns)
}

var _ Trigger = (*ChannelTrigger)(nil)
