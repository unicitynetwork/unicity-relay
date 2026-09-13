package zooid

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/coder/websocket"
)

// khatru handles every websocket message in its own goroutine. Before
// fiatjaf.com/nostr bdef5ac the AUTH handler read len(AuthedPublicKeys) before
// taking authLock, so two valid AUTH messages for the same pubkey arriving
// together on one connection could index AuthedPublicKeys[-1] and crash the
// whole process (issue #30). Under -race the unsynchronized read is reported
// on every run; without it the panic reproduces probabilistically.
func TestAuth_ConcurrentDuplicateAuth(t *testing.T) {
	instance := &Instance{}
	relay := khatru.NewRelay()
	relay.OnConnect = instance.OnConnect

	server := httptest.NewServer(relay)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	const workers = 8
	const connectionsPerWorker = 50

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range connectionsPerWorker {
				if err := authTwice(t.Context(), url); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// authTwice opens a connection, answers the challenge sent by OnConnect with
// the same signed AUTH event twice back to back, and expects both to be accepted.
func authTwice(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}
	envelope, err := nostr.ParseMessage(string(data))
	if err != nil {
		return fmt.Errorf("parse challenge %s: %w", data, err)
	}
	challenge, ok := envelope.(*nostr.AuthEnvelope)
	if !ok || challenge.Challenge == nil {
		return fmt.Errorf("expected AUTH challenge, got %s", data)
	}

	event := nostr.Event{
		Kind:      nostr.KindClientAuthentication,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"relay", url},
			{"challenge", *challenge.Challenge},
		},
	}
	if err := event.Sign(nostr.Generate()); err != nil {
		return fmt.Errorf("sign AUTH: %w", err)
	}
	msg, err := nostr.AuthEnvelope{Event: event}.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal AUTH: %w", err)
	}

	for range 2 {
		if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
			return fmt.Errorf("write AUTH: %w", err)
		}
	}

	for range 2 {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read OK: %w", err)
		}
		envelope, err := nostr.ParseMessage(string(data))
		if err != nil {
			return fmt.Errorf("parse OK %s: %w", data, err)
		}
		if result, isOK := envelope.(*nostr.OKEnvelope); !isOK || !result.OK {
			return fmt.Errorf("expected accepted OK, got %s", data)
		}
	}

	return nil
}
