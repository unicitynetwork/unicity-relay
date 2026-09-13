package zooid

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/coder/websocket"
)

func TestInstance_PreventBroadcast(t *testing.T) {
	instance := createTestInstance()

	owner := instance.Config.GetOwner()
	member := nostr.Generate().Public()
	outsider := nostr.Generate().Public()
	stranger := nostr.Generate().Public()

	for _, pubkey := range []nostr.PubKey{owner, member, outsider} {
		if err := instance.Management.AddMember(pubkey); err != nil {
			t.Fatalf("Management.AddMember: %v", err)
		}
	}

	createGroup := func(h, content string) {
		t.Helper()
		instance.Groups.membershipFullyLoaded.Store(h, struct{}{})
		if err := instance.Groups.UpdateMetadata(nostr.Event{
			Kind:      nostr.KindSimpleGroupCreateGroup,
			CreatedAt: nostr.Now(),
			Tags:      nostr.Tags{{"h", h}},
			Content:   content,
		}); err != nil {
			t.Fatalf("UpdateMetadata(%s): %v", h, err)
		}
		if err := instance.Groups.AddMember(h, member); err != nil {
			t.Fatalf("Groups.AddMember(%s): %v", h, err)
		}
	}
	createGroup("private", `{"private":true}`)
	createGroup("hidden", `{"private":true,"hidden":true}`)
	createGroup("public", `{}`)

	hiddenMetadata, found := instance.Groups.GetMetadata("hidden")
	if !found {
		t.Fatal("hidden group metadata not found")
	}

	chat := func(h string) nostr.Event {
		return nostr.Event{Kind: nostr.KindSimpleGroupChatMessage, Tags: nostr.Tags{{"h", h}}}
	}
	note := nostr.Event{Kind: nostr.KindTextNote}

	tests := []struct {
		name        string
		open        bool
		adminAccess bool
		authed      []nostr.PubKey
		event       nostr.Event
		prevent     bool
	}{
		{name: "private group message to group member", authed: []nostr.PubKey{member}, event: chat("private"), prevent: false},
		{name: "private group message to non-member", authed: []nostr.PubKey{outsider}, event: chat("private"), prevent: true},
		{name: "private group message to non-member on open relay", open: true, authed: []nostr.PubKey{outsider}, event: chat("private"), prevent: true},
		{name: "private group message to relay admin", authed: []nostr.PubKey{owner}, event: chat("private"), prevent: true},
		{name: "private group message to relay admin with private_relay_admin_access", adminAccess: true, authed: []nostr.PubKey{owner}, event: chat("private"), prevent: false},
		{name: "hidden group metadata to non-member", open: true, authed: []nostr.PubKey{outsider}, event: hiddenMetadata, prevent: true},
		{name: "hidden group metadata to group member", authed: []nostr.PubKey{member}, event: hiddenMetadata, prevent: false},
		{name: "public group message to non-member on open relay", open: true, authed: []nostr.PubKey{outsider}, event: chat("public"), prevent: false},
		{name: "public group message to non-member on closed relay", authed: []nostr.PubKey{outsider}, event: chat("public"), prevent: true},
		{name: "last authenticated pubkey is not a member", authed: []nostr.PubKey{member, outsider}, event: chat("private"), prevent: true},
		{name: "last authenticated pubkey is a member", authed: []nostr.PubKey{outsider, member}, event: chat("private"), prevent: false},
		{name: "unauthenticated connection", open: true, event: chat("public"), prevent: true},
		{name: "non-group event to relay member", authed: []nostr.PubKey{outsider}, event: note, prevent: false},
		{name: "non-group event to relay non-member", authed: []nostr.PubKey{stranger}, event: note, prevent: true},
		{name: "non-group event to relay non-member on open relay", open: true, authed: []nostr.PubKey{stranger}, event: note, prevent: false},
		{name: "relay-level admin list", authed: []nostr.PubKey{outsider}, event: nostr.Event{Kind: nostr.KindSimpleGroupAdmins, Tags: nostr.Tags{{"d", "_"}}}, prevent: false},
		{name: "write-only event", authed: []nostr.PubKey{member}, event: nostr.Event{Kind: RELAY_JOIN}, prevent: true},
		{name: "group members list", authed: []nostr.PubKey{member}, event: nostr.Event{Kind: nostr.KindSimpleGroupMembers, Tags: nostr.Tags{{"d", "public"}}}, prevent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance.Config.Policy.Open = tt.open
			instance.Config.Groups.PrivateRelayAdminAccess = tt.adminAccess

			ws := &khatru.WebSocket{AuthedPublicKeys: tt.authed}
			if got := instance.PreventBroadcast(ws, nostr.Filter{}, tt.event); got != tt.prevent {
				t.Errorf("PreventBroadcast() = %v, want %v", got, tt.prevent)
			}
		})
	}
}

// khatru's AUTH handler changes AuthedPublicKeys under WebSocket.authLock.
// Under -race this fails if lastAuthedPubkey reads the slice without that lock
// while AUTH messages are being handled.
func TestLastAuthedPubkey_ConcurrentWithAuth(t *testing.T) {
	relay := khatru.NewRelay()
	connections := make(chan *khatru.WebSocket, 1)
	relay.OnConnect = func(ctx context.Context) {
		connections <- khatru.GetConnection(ctx)
		khatru.RequestAuth(ctx)
	}

	server := httptest.NewServer(relay)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	client := dialBroadcastClient(t, url)
	ws := <-connections

	env, ok := client.next(5 * time.Second)
	challenge, isAuth := env.(*nostr.AuthEnvelope)
	if !ok || !isAuth || challenge.Challenge == nil {
		t.Fatalf("expected AUTH challenge, got %#v", env)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			lastAuthedPubkey(ws)
		}
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	// More pubkeys than khatru's MaxAuthenticatedClients, so the handler both
	// appends and replaces entries.
	var last nostr.PubKey
	for range 20 {
		secret := nostr.Generate()
		last = secret.Public()

		auth := nostr.Event{
			Kind:      nostr.KindClientAuthentication,
			CreatedAt: nostr.Now(),
			Tags:      nostr.Tags{{"relay", url}, {"challenge", *challenge.Challenge}},
		}
		if err := auth.Sign(secret); err != nil {
			t.Fatal(err)
		}
		msg, err := nostr.AuthEnvelope{Event: auth}.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		client.write(msg)
		client.expectOK(auth.ID)
	}

	if got, ok := lastAuthedPubkey(ws); !ok || got != last {
		t.Errorf("lastAuthedPubkey() = %s, %v; want %s", got.Hex(), ok, last.Hex())
	}
}

// The tests below run the same scenarios end to end over websockets against a
// relay built by MakeInstance. Each subscribes as a connection that must not
// see an event, publishes afterwards, and checks that live delivery withholds
// the event the same way a stored query does.

const broadcastWait = 2 * time.Second

func TestBroadcast_PrivateGroupMessageToNonMember(t *testing.T) {
	instance, url := startBroadcastTestRelay(t)
	h := "private-" + strings.ToLower(RandomString(8))
	filter := fmt.Sprintf(`{"kinds":[9],"#h":[%q]}`, h)

	creatorKey, outsiderKey := nostr.Generate(), nostr.Generate()
	creator := dialAuthedBroadcastClient(t, url, creatorKey)
	creator.publish(creatorKey, nostr.KindSimpleGroupCreateGroup, `{"name":"Secret","private":true}`, h)
	if !instance.Groups.IsPrivateGroup(h) {
		t.Fatalf("group %s was not created as private", h)
	}

	outsider := dialAuthedBroadcastClient(t, url, outsiderKey)
	if events, closed := outsider.subscribe("live", filter); closed != "" || len(events) != 0 {
		t.Fatalf("outsider subscription: closed=%q events=%d", closed, len(events))
	}

	creator.publish(creatorKey, nostr.KindSimpleGroupChatMessage, "members only", h)

	if events, _ := creator.subscribe("stored", filter); len(events) != 1 {
		t.Fatalf("creator stored query returned %d events, want 1", len(events))
	}
	if events, _ := dialAuthedBroadcastClient(t, url, outsiderKey).subscribe("stored", filter); len(events) != 0 {
		t.Fatalf("outsider stored query returned %d events, want 0", len(events))
	}

	if evt := outsider.waitEvent("live", broadcastWait); evt != nil {
		t.Errorf("non-member received private group message live: %q", evt.Content)
	}
}

func TestBroadcast_UnauthenticatedLimitZero(t *testing.T) {
	instance, url := startBroadcastTestRelay(t)
	h := "private-" + strings.ToLower(RandomString(8))

	creatorKey := nostr.Generate()
	creator := dialAuthedBroadcastClient(t, url, creatorKey)
	creator.publish(creatorKey, nostr.KindSimpleGroupCreateGroup, `{"name":"Secret","private":true}`, h)
	if !instance.Groups.IsPrivateGroup(h) {
		t.Fatalf("group %s was not created as private", h)
	}

	// Depending on the khatru version, a limit:0 filter is either closed with
	// auth-required or registered as a listener without calling OnRequest.
	anonymous := dialBroadcastClient(t, url)
	anonymous.subscribe("live", fmt.Sprintf(`{"kinds":[9],"#h":[%q],"limit":0}`, h))

	creator.publish(creatorKey, nostr.KindSimpleGroupChatMessage, "members only", h)

	if evt := anonymous.waitEvent("live", broadcastWait); evt != nil {
		t.Errorf("unauthenticated connection received private group message live: %q", evt.Content)
	}
}

func TestBroadcast_HiddenGroupMetadataToNonMember(t *testing.T) {
	instance, url := startBroadcastTestRelay(t)
	h := "hidden-" + strings.ToLower(RandomString(8))
	filter := fmt.Sprintf(`{"kinds":[39000],"#d":[%q]}`, h)

	creatorKey, outsiderKey := nostr.Generate(), nostr.Generate()
	outsider := dialAuthedBroadcastClient(t, url, outsiderKey)
	if events, closed := outsider.subscribe("live", filter); closed != "" || len(events) != 0 {
		t.Fatalf("outsider subscription: closed=%q events=%d", closed, len(events))
	}

	creator := dialAuthedBroadcastClient(t, url, creatorKey)
	creator.publish(creatorKey, nostr.KindSimpleGroupCreateGroup, `{"name":"Hidden","private":true,"hidden":true}`, h)
	if meta, found := instance.Groups.GetMetadata(h); !found || !HasTag(meta.Tags, "hidden") {
		t.Fatalf("group %s was not created as hidden", h)
	}

	if events, _ := creator.subscribe("stored", filter); len(events) != 1 {
		t.Fatalf("creator stored query returned %d events, want 1", len(events))
	}
	if events, _ := dialAuthedBroadcastClient(t, url, outsiderKey).subscribe("stored", filter); len(events) != 0 {
		t.Fatalf("outsider stored query returned %d events, want 0", len(events))
	}

	if evt := outsider.waitEvent("live", broadcastWait); evt != nil {
		t.Errorf("non-member received hidden group metadata live: tags=%v", evt.Tags)
	}
}

// startBroadcastTestRelay builds an instance through MakeInstance with the
// policy Sphere runs (open, public_join, groups with auto_join) and serves it.
func startBroadcastTestRelay(t *testing.T) (*Instance, string) {
	t.Helper()

	name := "broadcast-" + strings.ToLower(RandomString(8)) + ".toml"
	path := filepath.Join(Env("CONFIG"), name)
	config := fmt.Sprintf(`host = "localhost"
schema = %q
secret = %q

[info]
name = "broadcast test"
pubkey = %q

[policy]
open = true
public_join = true

[groups]
enabled = true
auto_join = true
`, "broadcast"+strings.ToLower(RandomString(8)), nostr.Generate().Hex(), nostr.Generate().Public().Hex())

	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })

	instance, err := MakeInstance(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Cleanup)

	server := httptest.NewServer(instance.Relay)
	t.Cleanup(server.Close)

	return instance, "ws" + strings.TrimPrefix(server.URL, "http")
}

type broadcastClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func dialBroadcastClient(t *testing.T, url string) *broadcastClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	return &broadcastClient{t: t, conn: conn}
}

func dialAuthedBroadcastClient(t *testing.T, url string, secret nostr.SecretKey) *broadcastClient {
	t.Helper()
	c := dialBroadcastClient(t, url)

	env, ok := c.next(5 * time.Second)
	challenge, isAuth := env.(*nostr.AuthEnvelope)
	if !ok || !isAuth || challenge.Challenge == nil {
		t.Fatalf("expected AUTH challenge, got %#v", env)
	}

	auth := nostr.Event{
		Kind:      nostr.KindClientAuthentication,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"relay", url}, {"challenge", *challenge.Challenge}},
	}
	if err := auth.Sign(secret); err != nil {
		t.Fatal(err)
	}
	msg, err := nostr.AuthEnvelope{Event: auth}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	c.write(msg)
	c.expectOK(auth.ID)

	return c
}

func (c *broadcastClient) write(msg []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 5*time.Second)
	defer cancel()

	if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// next reads one envelope. It returns false if nothing arrives within wait;
// the connection should not be used after that.
func (c *broadcastClient) next(wait time.Duration) (nostr.Envelope, bool) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), wait)
	defer cancel()

	_, data, err := c.conn.Read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false
		}
		c.t.Fatalf("read: %v", err)
	}
	env, err := nostr.ParseMessage(string(data))
	if err != nil {
		c.t.Fatalf("parse %s: %v", data, err)
	}
	return env, true
}

func (c *broadcastClient) expectOK(id nostr.ID) {
	c.t.Helper()
	for {
		env, ok := c.next(5 * time.Second)
		if !ok {
			c.t.Fatalf("no OK for event %s", id.Hex())
		}
		if result, isOK := env.(*nostr.OKEnvelope); isOK && result.EventID == id {
			if !result.OK {
				c.t.Fatalf("event %s rejected: %s", id.Hex(), result.Reason)
			}
			return
		}
	}
}

func (c *broadcastClient) publish(secret nostr.SecretKey, kind nostr.Kind, content, h string) {
	c.t.Helper()
	evt := nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", h}},
		Content:   content,
	}
	if err := evt.Sign(secret); err != nil {
		c.t.Fatal(err)
	}
	msg, err := nostr.EventEnvelope{Event: evt}.MarshalJSON()
	if err != nil {
		c.t.Fatal(err)
	}
	c.write(msg)
	c.expectOK(evt.ID)
}

// subscribe sends a REQ and collects stored events until EOSE, or returns the
// reason if the relay answers with CLOSED.
func (c *broadcastClient) subscribe(id, filter string) ([]nostr.Event, string) {
	c.t.Helper()
	c.write([]byte(fmt.Sprintf(`["REQ",%q,%s]`, id, filter)))

	var events []nostr.Event
	for {
		env, ok := c.next(5 * time.Second)
		if !ok {
			c.t.Fatalf("no EOSE or CLOSED for subscription %s", id)
		}
		switch env := env.(type) {
		case *nostr.EventEnvelope:
			if env.SubscriptionID != nil && *env.SubscriptionID == id {
				events = append(events, env.Event)
			}
		case *nostr.EOSEEnvelope:
			if string(*env) == id {
				return events, ""
			}
		case *nostr.ClosedEnvelope:
			if env.SubscriptionID == id {
				return events, env.Reason
			}
		}
	}
}

// waitEvent returns the next event delivered to subscription id, or nil if
// none arrives within wait. The connection should not be used afterwards.
func (c *broadcastClient) waitEvent(id string, wait time.Duration) *nostr.Event {
	c.t.Helper()
	deadline := time.Now().Add(wait)
	for {
		env, ok := c.next(time.Until(deadline))
		if !ok {
			return nil
		}
		if evt, isEvent := env.(*nostr.EventEnvelope); isEvent && evt.SubscriptionID != nil && *evt.SubscriptionID == id {
			return &evt.Event
		}
	}
}
