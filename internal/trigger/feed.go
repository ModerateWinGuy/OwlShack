package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/meshcore-go/OwlShack/internal/buildinfo"
	"github.com/meshcore-go/OwlShack/internal/config"
	"github.com/meshcore-go/OwlShack/internal/logging"
	"github.com/mmcdole/gofeed"
	"github.com/robfig/cron/v3"
)

const (
	// feedDefaultSchedule is used when a feed trigger names no schedule. Public feeds publish on
	// the order of minutes, and every install polling harder is rude to the operator.
	feedDefaultSchedule = "@every 5m"
	feedPollTimeout     = 30 * time.Second
	feedMaxBytes        = 8 << 20

	// feedMaxPerPoll bounds one poll's sends. A feed that republishes its whole backlog would
	// otherwise queue dozens of transmissions onto a duty-cycle-limited radio; the newest win.
	feedMaxPerPoll = 5

	// feedForgetAfter is how many polls an item may be missing from the feed before it is
	// forgotten, which bounds the seen set without letting a briefly-absent item fire twice.
	feedForgetAfter = 10
)

// errItemPermanent marks a failure that will repeat on every poll — a link that is not an alert,
// a 4xx — so the item is recorded as seen instead of being retried forever.
var errItemPermanent = errors.New("permanent item failure")

// itemDecoder turns one feed item into template data plus the named pieces of text match patterns
// run against, one entry per matchable field.
type itemDecoder func(ctx context.Context, feed *gofeed.Feed, item *gofeed.Item) (data map[string]any, fields map[string]string, err error)

// feedPoller is the polling half shared by RSSTrigger and CAPTrigger: fetch on a schedule,
// deduplicate, and fire once per item not seen before. What an item *becomes* is the trigger's
// business, through decode.
type feedPoller struct {
	botName  string
	kind     string
	url      string
	schedule string
	matcher  fieldMatcher
	parser   *gofeed.Parser
	decode   itemDecoder
	log      *slog.Logger

	mu       sync.Mutex
	cron     *cron.Cron
	callback Callback

	// pollMu serialises the startup poll against the scheduled ones; seen, polls and primed
	// belong to whichever poll holds it.
	pollMu sync.Mutex
	seen   map[string]int
	polls  int
	primed bool
}

func newFeedPoller(kind, botName string, cfg config.TriggerConfig, log *slog.Logger) (*feedPoller, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("%s trigger requires a url", kind)
	}
	matcher, err := newFieldMatcher(cfg.Match)
	if err != nil {
		return nil, err
	}

	schedule := cfg.Schedule
	if schedule == "" {
		schedule = feedDefaultSchedule
	}

	parser := gofeed.NewParser()
	parser.UserAgent = "OwlShack/" + buildinfo.Version
	parser.MaxByteSize = feedMaxBytes
	parser.Client = &http.Client{Timeout: feedPollTimeout}

	return &feedPoller{
		botName:  botName,
		kind:     kind,
		url:      cfg.URL,
		schedule: schedule,
		matcher:  matcher,
		parser:   parser,
		log:      log.With("trigger", kind, "url", cfg.URL),
		seen:     map[string]int{},
	}, nil
}

func (p *feedPoller) Start(ctx context.Context, callback Callback) error {
	// SkipIfStillRunning keeps a slow feed from stacking polls when the fetch outlasts the interval.
	c := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	if _, err := c.AddFunc(p.schedule, func() { p.poll(ctx) }); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", p.schedule, err)
	}

	p.mu.Lock()
	p.callback = callback
	p.cron = c
	p.mu.Unlock()

	c.Start()

	// The first poll only records what is already published, so running it now rather than one
	// interval from now is what makes the second poll able to fire.
	go p.poll(ctx)

	go func() {
		<-ctx.Done()
		p.Stop()
	}()

	return nil
}

func (p *feedPoller) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.callback = nil
	if p.cron != nil {
		p.cron.Stop()
		p.cron = nil
	}
	return nil
}

func (p *feedPoller) poll(ctx context.Context) {
	p.mu.Lock()
	cb := p.callback
	p.mu.Unlock()
	if cb == nil {
		return // stopped, or Start has not stored the callback yet
	}

	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	pollCtx, cancel := context.WithTimeout(ctx, feedPollTimeout)
	defer cancel()

	feed, err := p.parser.ParseURLWithContext(p.url, pollCtx)
	if err != nil {
		p.log.Warn("feed poll failed", "error", err)
		return
	}

	p.polls++
	sort.Sort(feed) // oldest first, so a backlog goes out in the order it happened

	for _, item := range p.freshItems(feed) {
		data, fields, err := p.decode(pollCtx, feed, item)
		if err != nil {
			// Only a permanent failure is recorded; one that failed once is retried next poll.
			if errors.Is(err, errItemPermanent) {
				p.seen[itemID(item)] = p.polls
			}
			p.log.Warn("feed item skipped", "item", itemID(item), "error", err)
			continue
		}
		p.seen[itemID(item)] = p.polls

		captures := p.matcher.match(fields)
		if captures == nil {
			p.log.Log(ctx, logging.LevelTrace, "no pattern matched",
				"item", itemID(item), "patterns", p.matcher)
			continue
		}
		data["Match"] = captures

		p.log.Info("feed item fired", "item", itemID(item), "title", item.Title)
		cb(Event{Type: p.kind, BotName: p.botName, Data: data})
	}

	p.primed = true
	p.forgetStale()
}

// freshItems marks every item it sees and returns only those worth firing on: none at all on the
// first poll, which records the existing backlog so a restart does not replay it onto the mesh.
func (p *feedPoller) freshItems(feed *gofeed.Feed) []*gofeed.Item {
	fresh := make([]*gofeed.Item, 0, len(feed.Items))
	for _, item := range feed.Items {
		id := itemID(item)
		if id == "" {
			p.log.Log(context.Background(), logging.LevelTrace, "item has no usable id, skipping")
			continue
		}
		if _, ok := p.seen[id]; ok {
			p.seen[id] = p.polls
			continue
		}
		if !p.primed {
			p.seen[id] = p.polls
			continue
		}
		fresh = append(fresh, item)
	}

	if len(fresh) > feedMaxPerPoll {
		p.log.Warn("feed burst clamped", "new", len(fresh), "sending", feedMaxPerPoll)
		for _, item := range fresh[:len(fresh)-feedMaxPerPoll] {
			p.seen[itemID(item)] = p.polls
		}
		fresh = fresh[len(fresh)-feedMaxPerPoll:]
	}
	return fresh
}

func (p *feedPoller) forgetStale() {
	for id, last := range p.seen {
		if p.polls-last > feedForgetAfter {
			delete(p.seen, id)
		}
	}
}

// itemID is the identity a feed item is deduplicated on, preferring the publisher's own id.
func itemID(item *gofeed.Item) string {
	for _, candidate := range []string{item.GUID, item.Link, item.Title} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func itemTime(item *gofeed.Item) time.Time {
	if item.PublishedParsed != nil {
		return *item.PublishedParsed
	}
	if item.UpdatedParsed != nil {
		return *item.UpdatedParsed
	}
	return time.Time{}
}

func authorName(item *gofeed.Item) string {
	if len(item.Authors) > 0 && item.Authors[0] != nil {
		return item.Authors[0].Name
	}
	return ""
}
