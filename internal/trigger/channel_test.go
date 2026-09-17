package trigger

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/meshcore-go/OwlShack/internal/config"
	meshcore "github.com/meshcore-go/meshcore-go"
)

const responsePattern = `^@\[{{.Sender | reQuote}}\].+`

func testChannelTrigger(t *testing.T, ctx context.Context, pattern string, fired chan Event) *ChannelTrigger {
	t.Helper()
	patterns := []string{`(?i)^wlg$`}
	tr, err := NewChannelTrigger("backup", config.TriggerConfig{
		Match: &patterns, FailoverPattern: pattern, FailoverTimeout: 10,
	}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Start(ctx, func(e Event) { fired <- e }); err != nil {
		t.Fatal(err)
	}
	return tr
}

func receiveGroup(tr *ChannelTrigger, channel, sender, text string) {
	tr.handleGroupText(&meshcore.GroupTextPayload{Sender: sender, Text: text, Timestamp: 42},
		&meshcore.ChannelEntry{Name: channel}, &meshcore.Packet{SNR: 8})
}

func TestChannelFailoverDeadlineAndDuplicate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fired := make(chan Event, 8)
		tr := testChannelTrigger(t, context.Background(), responsePattern, fired)
		defer tr.Stop()
		receiveGroup(tr, "testing", "Alice", "WLG")
		time.Sleep(5 * time.Second)
		receiveGroup(tr, "testing", "Alice", "WLG")
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if len(fired) != 0 {
			t.Fatal("replied before deadline")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(fired) != 1 {
			t.Fatalf("got %d replies, want one at original deadline", len(fired))
		}
		event := <-fired
		if event.Data["Sender"] != "Alice" || event.Data["Message"] != "WLG" || event.Data["SNR"] != float32(8) {
			t.Fatalf("original request data lost: %+v", event.Data)
		}
	})
}

func TestChannelFailoverResponse(t *testing.T) {
	for _, tc := range []struct {
		name, channel, sender, text string
		suppressed                  bool
	}{
		{"matching response", "testing", "primary", "@[Alice.+] sunny", true},
		{"different channel", "other", "primary", "@[Alice.+] sunny", false},
		{"different requester", "testing", "primary", "@[Bob] sunny", false},
		{"literal sender", "testing", "primary", "@[AliceXYZ] sunny", false},
		{"own echo", "testing", "backup", "@[Alice.+] sunny", false},
		{"requester message", "testing", "Alice.+", "@[Alice.+] sunny", false},
		{"unrelated", "testing", "primary", "hello", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fired := make(chan Event, 8)
				tr := testChannelTrigger(t, context.Background(), responsePattern, fired)
				defer tr.Stop()
				receiveGroup(tr, "testing", "Alice.+", "wlg")
				time.Sleep(5 * time.Second)
				receiveGroup(tr, tc.channel, tc.sender, tc.text)
				// A repeated request must not re-arm a suppressed reply.
				receiveGroup(tr, "testing", "Alice.+", "wlg")
				time.Sleep(5 * time.Second)
				synctest.Wait()
				want := 1
				if tc.suppressed {
					want = 0
				}
				if len(fired) != want {
					t.Fatalf("got %d replies, want %d", len(fired), want)
				}
			})
		})
	}
}

func TestChannelFailoverIsolatesPendingRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fired := make(chan Event, 8)
		tr := testChannelTrigger(t, context.Background(), responsePattern, fired)
		defer tr.Stop()
		receiveGroup(tr, "testing", "Alice", "wlg")
		receiveGroup(tr, "testing", "Bob", "wlg")
		receiveGroup(tr, "other", "Alice", "wlg")
		time.Sleep(time.Second)
		receiveGroup(tr, "testing", "primary", "@[Alice] sunny")
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if len(fired) != 2 {
			t.Fatalf("got %d replies, want 2", len(fired))
		}
		for len(fired) > 0 {
			e := <-fired
			if e.Data["Channel"] == "testing" && e.Data["Sender"] == "Alice" {
				t.Fatal("suppressed request fired")
			}
		}
	})
}

func TestChannelFailoverLifecycle(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired := make(chan Event, 8)
			tr := testChannelTrigger(t, ctx, responsePattern, fired)
			defer tr.Stop()
			receiveGroup(tr, "testing", "Alice", "wlg")
			time.Sleep(5 * time.Second)
			if cancelContext {
				cancel()
			} else {
				tr.Stop()
			}
			receiveGroup(tr, "testing", "Bob", "wlg")
			time.Sleep(10 * time.Second)
			synctest.Wait()
			if len(fired) != 0 {
				t.Fatal("stopped trigger replied")
			}
			tr.mu.Lock()
			remaining := len(tr.pending)
			tr.mu.Unlock()
			if remaining != 0 {
				t.Fatal("pending state survived stop")
			}
		})
	}
}

func TestChannelImmediateReplyAndRuntimePatternError(t *testing.T) {
	for _, pattern := range []string{"", `{{.Sender}}`} {
		synctest.Test(t, func(t *testing.T) {
			fired := make(chan Event, 8)
			tr := testChannelTrigger(t, context.Background(), pattern, fired)
			defer tr.Stop()
			receiveGroup(tr, "testing", "[", "wlg")
			want := 0
			if pattern == "" {
				want = 1
			}
			if len(fired) != want {
				t.Fatalf("immediate replies = %d, want %d", len(fired), want)
			}
			time.Sleep(10 * time.Second)
			synctest.Wait()
			if len(fired) != want {
				t.Fatal("invalid pattern caused fallback reply")
			}
		})
	}
}
