package trigger

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/meshcore-go/OwlShack/internal/config"
	"github.com/mmcdole/gofeed"
	"github.com/tuzzmaniandevil/cap-go"
)

// CAPTrigger fires once per new alert in a CAP feed: the feed entry only announces the alert, so
// each new one is fetched and decoded as a CAP 1.2 document before the template sees it.
type CAPTrigger struct{ *feedPoller }

func NewCAPTrigger(botName string, cfg config.TriggerConfig, log *slog.Logger) (*CAPTrigger, error) {
	p, err := newFeedPoller("cap", botName, cfg, log)
	if err != nil {
		return nil, err
	}
	t := &CAPTrigger{p}
	p.decode = t.decodeAlert
	return t, nil
}

// capMaxBytes bounds one alert document. Real CAP alerts run to tens of kilobytes; anything past
// this is not an alert and should not be read into memory.
const capMaxBytes = 4 << 20

// decodeAlert fetches the alert document a feed item links to and overlays its fields onto the
// entry's data. The CAP fields win where they collide: a CAP trigger's template is written against
// the alert, not the entry that announced it.
func (t *CAPTrigger) decodeAlert(ctx context.Context, feed *gofeed.Feed, item *gofeed.Item) (map[string]any, map[string]string, error) {
	data, _, err := decodeEntry(ctx, feed, item)
	if err != nil {
		return nil, nil, err
	}
	if item.Link == "" {
		return nil, nil, fmt.Errorf("%w: item has no link to an alert document", errItemPermanent)
	}

	alert, err := t.fetchAlert(ctx, item.Link)
	if err != nil {
		return nil, nil, err
	}

	info := primaryInfo(alert)
	if info == nil {
		return nil, nil, fmt.Errorf("%w: alert %s carries no info block", errItemPermanent, alert.Identifier)
	}

	areas := make([]string, 0, len(info.Area))
	for _, a := range info.Area {
		if a != nil && a.AreaDesc != "" {
			areas = append(areas, a.AreaDesc)
		}
	}

	data["Alert"] = alert
	data["Info"] = info
	data["Identifier"] = alert.Identifier
	data["Sender"] = alert.Sender
	data["Sent"] = alert.Sent.Time
	data["Status"] = alert.Status.String()
	data["MsgType"] = alert.MsgType.String()
	data["Event"] = info.Event
	data["Headline"] = deref(info.Headline)
	data["Severity"] = info.Severity.String()
	data["Urgency"] = info.Urgency.String()
	data["Certainty"] = info.Certainty.String()
	data["Description"] = deref(info.Description)
	data["Instruction"] = deref(info.Instruction)
	data["SenderName"] = deref(info.SenderName)
	data["Web"] = deref(info.Web)
	data["Areas"] = strings.Join(areas, ", ")
	data["Effective"] = capTime(info.Effective)
	data["Onset"] = capTime(info.Onset)
	data["Expires"] = capTime(info.Expires)
	data["Categories"] = categoryNames(info.Categories)

	fields := map[string]string{
		"event":       info.Event,
		"headline":    deref(info.Headline),
		"description": deref(info.Description),
		"instruction": deref(info.Instruction),
		"severity":    info.Severity.String(),
		"urgency":     info.Urgency.String(),
		"certainty":   info.Certainty.String(),
		"msgtype":     alert.MsgType.String(),
		"status":      alert.Status.String(),
		"area":        strings.Join(areas, "\n"),
		"sender":      deref(info.SenderName),
		"category":    strings.Join(categoryNames(info.Categories), "\n"),
	}
	return data, fields, nil
}

func (t *CAPTrigger) fetchAlert(ctx context.Context, url string) (*cap.Alert, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errItemPermanent, err)
	}
	req.Header.Set("User-Agent", t.parser.UserAgent)
	req.Header.Set("Accept", "application/cap+xml, application/xml;q=0.9, */*;q=0.1")

	resp, err := t.parser.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching alert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// A 4xx is the publisher saying this URL will never be an alert; a 5xx may pass.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("%w: alert fetch returned %s", errItemPermanent, resp.Status)
		}
		return nil, fmt.Errorf("alert fetch returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, capMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading alert: %w", err)
	}
	if len(body) > capMaxBytes {
		return nil, fmt.Errorf("%w: alert document exceeds %d bytes", errItemPermanent, capMaxBytes)
	}

	var alert cap.Alert
	if err := xml.Unmarshal(body, &alert); err != nil {
		return nil, fmt.Errorf("%w: parsing alert: %v", errItemPermanent, err)
	}
	return &alert, nil
}

// primaryInfo picks the info block a template renders. CAP repeats the whole block per language;
// English is preferred because that is what the mesh reads.
// ponytail: no per-trigger language setting until someone runs a bot that needs one.
func primaryInfo(alert *cap.Alert) *cap.Info {
	var first *cap.Info
	for _, info := range alert.Info {
		if info == nil {
			continue
		}
		if first == nil {
			first = info
		}
		if info.Language == nil || strings.HasPrefix(strings.ToLower(*info.Language), "en") {
			return info
		}
	}
	return first
}

func categoryNames(categories []cap.Category) []string {
	names := make([]string, len(categories))
	for i, c := range categories {
		names[i] = c.String()
	}
	return names
}

func capTime(t *cap.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var _ Trigger = (*CAPTrigger)(nil)
