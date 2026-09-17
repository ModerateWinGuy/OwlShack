package config

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/robfig/cron/v3"
)

type TriggerConfig struct {
	Type     string `json:"type" yaml:"type" toml:"type"` // group, private, dm, cron, cap, etc
	Template string `json:"template" yaml:"template" toml:"template"`

	CharLimitBehaviour *string `json:"charLimitBehaviour" yaml:"charLimitBehaviour" toml:"charLimitBehaviour"` // e.g. truncate or split

	Match    *[]string    `json:"match" yaml:"match" toml:"match"`          // Patterns to match against (supports wildcards/regex)
	Channels *ChannelList `json:"channels" yaml:"channels" toml:"channels"` // Channels to listen on (strings or {name, privateKey} objects)
	Contacts *[]string    `json:"contacts" yaml:"contacts" toml:"contact"`  // What Contacts to listen in for DMs

	FailoverPattern string `json:"failoverPattern,omitempty" yaml:"failoverPattern,omitempty" toml:"failoverPattern,omitempty"`
	FailoverTimeout int64  `json:"failoverTimeout,omitempty" yaml:"failoverTimeout,omitempty" toml:"failoverTimeout,omitempty"`

	RetryTimeout *int64 `json:"retryTimeout" yaml:"retryTimeout" toml:"retryTimeout"` // Stored as seconds
	MaxRetries   *int   `json:"maxRetries" yaml:"maxRetries" toml:"maxRetries"`

	// Path Hash Size: 1-4 = fixed size, 0 = mirror incoming packet's hash size, nil = default (1)
	PathHashSize *uint8 `json:"pathHashSize,omitempty" yaml:"pathHashSize,omitempty" toml:"pathHashSize,omitempty"`

	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty" toml:"schedule,omitempty"`

	URL string `json:"url,omitempty" yaml:"url,omitempty" toml:"url,omitempty"` // Feed to poll, for rss and cap triggers
}

// MirrorIncomingPathHashSize is the pathHashSize that means "answer with whatever size came in",
// rather than a byte count of its own.
const MirrorIncomingPathHashSize = 0

// mirrorsIncoming reports whether this trigger answers a message, and so has one to mirror.
func (t *TriggerConfig) mirrorsIncoming() bool {
	return t.Type == "channel" || t.Type == "group" || t.Type == "dm"
}

// Validate rejects trigger configs that would fail companion construction, which after a reload exits the process.
func (t *TriggerConfig) Validate() error {
	switch t.Type {
	case "channel", "group":
		if t.Channels == nil || len(*t.Channels) == 0 {
			return fmt.Errorf("%s trigger requires at least one channel", t.Type)
		}
	case "cron":
		if t.Schedule == "" {
			return fmt.Errorf("cron trigger requires a schedule")
		}
		if _, err := cron.ParseStandard(t.Schedule); err != nil {
			return fmt.Errorf("invalid cron schedule %q: %w", t.Schedule, err)
		}
	case "dm":
		// No channel or contact is required: an empty contact list listens to every sender the DM policy already let through.
	case "rss", "cap":
		if t.URL == "" {
			return fmt.Errorf("%s trigger requires a url", t.Type)
		}
		u, err := url.Parse(t.URL)
		if err != nil {
			return fmt.Errorf("invalid url %q: %w", t.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("url %q must be http or https", t.URL)
		}
		// A feed trigger answers nobody, so with neither a channel nor a contact it can never
		// say anything.
		if (t.Channels == nil || len(*t.Channels) == 0) && (t.Contacts == nil || len(*t.Contacts) == 0) {
			return fmt.Errorf("%s trigger requires at least one channel or contact", t.Type)
		}
		// An empty schedule takes the trigger's own default rather than failing here.
		if t.Schedule != "" {
			if err := validateFeedSchedule(t.Schedule); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown trigger type %q (supported: group, dm, cron, rss, cap)", t.Type)
	}

	if t.FailoverPattern != "" || t.FailoverTimeout != 0 {
		if t.Type != "group" && t.Type != "channel" {
			return fmt.Errorf("failover is only supported for group triggers")
		}
		if strings.TrimSpace(t.FailoverPattern) == "" || t.FailoverTimeout < 1 || t.FailoverTimeout > 3600 {
			return fmt.Errorf("failover requires a response pattern and a timeout of 1-3600 seconds")
		}
		if _, err := t.FailoverRegexp("sender"); err != nil {
			return err
		}
	}

	if t.Template == "" {
		return fmt.Errorf("template is required")
	}
	// Stubs for the trigger func map (templater.go), so typo'd function names are caught here.
	stubs := template.FuncMap{
		"formatPathBytes": func(any, ...string) (string, error) { return "", nil },
		"now":             func() any { return nil },
		"date":            func(any, string, ...string) (string, error) { return "", nil },
	}
	if _, err := template.New("trigger").Funcs(stubs).Parse(t.Template); err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}

	if t.Match != nil {
		fields := matchFields[t.Type]
		for _, entry := range *t.Match {
			if fields != nil {
				if err := validateFieldPattern(entry, fields); err != nil {
					return err
				}
				continue
			}
			if _, err := regexp.Compile(entry); err != nil {
				return fmt.Errorf("invalid match pattern %q: %w", entry, err)
			}
		}
	}

	if t.Channels != nil {
		for _, ch := range *t.Channels {
			if err := ch.Validate(); err != nil {
				return err
			}
		}
	}

	if t.Contacts != nil {
		for _, c := range *t.Contacts {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("contacts must not contain a blank entry")
			}
		}
	}

	if t.PathHashSize != nil {
		if *t.PathHashSize > MaxPathHashSize {
			return fmt.Errorf("pathHashSize must be %d-%d", MirrorIncomingPathHashSize, MaxPathHashSize)
		}
		// 0 means "mirror the incoming message". Nothing comes in to mirror on a scheduled or
		// feed trigger, and silently falling back to the companion's size would leave the config
		// saying one thing and the radio doing another.
		if *t.PathHashSize == MirrorIncomingPathHashSize && !t.mirrorsIncoming() {
			return fmt.Errorf("%s trigger cannot mirror an incoming pathHashSize: it answers no message", t.Type)
		}
	}

	return nil
}

// MinPollInterval floors how often a feed trigger may poll, so a bot cannot hammer a publisher.
const MinPollInterval = time.Minute

// validateFeedSchedule accepts anything cron does but floors the interval. A crontab spec has no
// seconds field, so a minute is already its finest granularity and only an "@every" descriptor
// can ask for less.
func validateFeedSchedule(spec string) error {
	const every = "@every " // the exact prefix cron matches; descriptors are case-sensitive
	if rest, ok := strings.CutPrefix(spec, every); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return fmt.Errorf("invalid poll interval %q: %w", spec, err)
		}
		if d < MinPollInterval {
			return fmt.Errorf("poll interval %s is below the %s minimum", d, MinPollInterval)
		}
		return nil
	}
	if _, err := cron.ParseStandard(spec); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	return nil
}

func (cr *ChannelRef) Validate() error {
	if cr.Name == "" {
		return fmt.Errorf("channel name is required")
	}
	if cr.PrivateKey != "" {
		psk, err := hex.DecodeString(cr.PrivateKey)
		if err != nil {
			return fmt.Errorf("channel %q: privateKey must be hex: %w", cr.Name, err)
		}
		// NewChannelFromPSK rejects any other length at companion construction, which exits the process.
		if len(psk) != 16 {
			return fmt.Errorf("channel %q: privateKey must be 16 bytes (32 hex chars), got %d bytes", cr.Name, len(psk))
		}
	}
	return nil
}

// FailoverRegexp binds the response pattern to the original request's sender.
func (t *TriggerConfig) FailoverRegexp(sender string) (*regexp.Regexp, error) {
	tmpl, err := template.New("failover").Funcs(template.FuncMap{"reQuote": regexp.QuoteMeta}).Parse(t.FailoverPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid failover template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, struct{ Sender string }{sender}); err != nil {
		return nil, fmt.Errorf("invalid failover template: %w", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		return nil, fmt.Errorf("failover pattern must not render empty")
	}
	re, err := regexp.Compile(out.String())
	if err != nil {
		return nil, fmt.Errorf("invalid failover pattern: %w", err)
	}
	return re, nil
}
