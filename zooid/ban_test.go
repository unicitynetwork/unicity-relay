package zooid

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

func TestInstance_PreventBroadcast_BannedListener(t *testing.T) {
	instance := createTestInstance()
	instance.Config.Policy.Open = true

	banned := nostr.Generate().Public()
	if err := instance.Management.BanPubkey(banned, "spam"); err != nil {
		t.Fatalf("BanPubkey: %v", err)
	}

	ws := &khatru.WebSocket{AuthedPublicKeys: []nostr.PubKey{banned}}
	if !instance.PreventBroadcast(ws, nostr.Filter{}, nostr.Event{Kind: nostr.KindTextNote}) {
		t.Error("PreventBroadcast delivered an event to a banned pubkey")
	}
}

// BanPubkey removes relay membership, which a relay with policy.open does not
// require, so the ban itself must stop the pubkey from reading and writing.
func TestBan_OpenRelay(t *testing.T) {
	instance, url := startBroadcastTestRelay(t)
	h := "public-" + strings.ToLower(RandomString(8))
	filter := fmt.Sprintf(`{"kinds":[9],"#h":[%q]}`, h)
	creatorKey, bannedKey := nostr.Generate(), nostr.Generate()
	now := nostr.Now()

	creator := dialAuthedBroadcastClient(t, url, creatorKey)
	creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"Public"}`, h))

	subscribed := dialAuthedBroadcastClient(t, url, bannedKey)
	subscribed.mustSubscribe("live", filter)

	if err := instance.Management.BanPubkey(bannedKey.Public(), "spam"); err != nil {
		t.Fatalf("BanPubkey: %v", err)
	}

	banned := dialAuthedBroadcastClient(t, url, bannedKey)
	if _, closed := banned.subscribe("after-ban", filter); !strings.Contains(closed, "banned") {
		t.Errorf("REQ from the banned pubkey closed with %q, want a ban rejection", closed)
	}
	if ok, reason := banned.publishResult(bannedKey, groupEvent(nostr.KindSimpleGroupChatMessage, now, "hello", h)); ok || !strings.Contains(reason, "banned") {
		t.Errorf("chat message from the banned pubkey: ok=%v reason=%q, want a ban rejection", ok, reason)
	}
	giftWrap := nostr.Event{
		Kind:      nostr.KindGiftWrap,
		CreatedAt: now,
		Tags:      nostr.Tags{{"p", instance.Config.GetOwner().Hex()}},
	}
	if ok, reason := banned.publishResult(bannedKey, giftWrap); ok || !strings.Contains(reason, "banned") {
		t.Errorf("gift wrap from the banned pubkey to a relay member: ok=%v reason=%q, want a ban rejection", ok, reason)
	}

	creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupChatMessage, now, "after the ban", h))
	if evt := subscribed.waitEvent("live", broadcastWait); evt != nil {
		t.Errorf("subscription opened before the ban received %q", evt.Content)
	}
}

// publishResult signs evt, sends it and returns the relay's OK verdict.
func (c *broadcastClient) publishResult(secret nostr.SecretKey, evt nostr.Event) (bool, string) {
	c.t.Helper()
	if err := evt.Sign(secret); err != nil {
		c.t.Fatal(err)
	}
	msg, err := nostr.EventEnvelope{Event: evt}.MarshalJSON()
	if err != nil {
		c.t.Fatal(err)
	}
	c.write(msg)
	for {
		env, ok := c.next(5 * time.Second)
		if !ok {
			c.t.Fatalf("no OK for event %s", evt.ID.Hex())
		}
		if result, isOK := env.(*nostr.OKEnvelope); isOK && result.EventID == evt.ID {
			return result.OK, result.Reason
		}
	}
}
