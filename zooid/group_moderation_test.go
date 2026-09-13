package zooid

import (
	"fmt"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

// Clients catch up on removals and group deletions that happened while they
// were offline by querying kinds 9005, 9001 and 9008 for their groups when they
// reconnect. These tests make the change first and query afterwards.

func TestStoredModeration_RemovedUserCanReadOwnRemoval(t *testing.T) {
	_, url := startBroadcastTestRelay(t)
	h := "private-" + strings.ToLower(RandomString(8))
	creatorKey, removedKey, otherKey := nostr.Generate(), nostr.Generate(), nostr.Generate()
	removed, other := removedKey.Public(), otherKey.Public()
	now := nostr.Now()

	creator := dialAuthedBroadcastClient(t, url, creatorKey)
	creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"Secret","private":true}`, h))
	for _, pubkey := range []nostr.PubKey{removed, other} {
		creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupPutUser, now, "", h, nostr.Tag{"p", pubkey.Hex()}))
	}
	for _, pubkey := range []nostr.PubKey{removed, other} {
		creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupRemoveUser, now+1, "", h, nostr.Tag{"p", pubkey.Hex()}))
	}

	filter := fmt.Sprintf(`{"kinds":[9005,9001,9008],"#h":[%q]}`, h)
	events, closed := dialAuthedBroadcastClient(t, url, removedKey).subscribe("moderation", filter)
	if closed != "" {
		t.Fatalf("subscription closed: %s", closed)
	}
	if len(events) != 1 || events[0].Tags.FindWithValue("p", removed.Hex()) == nil {
		t.Fatalf("removed user got %d moderation events, want only the removal naming them", len(events))
	}
}

func TestStoredModeration_GroupDeletionAfterTheFact(t *testing.T) {
	_, url := startBroadcastTestRelay(t)
	creatorKey, memberKey, outsiderKey := nostr.Generate(), nostr.Generate(), nostr.Generate()
	now := nostr.Now()
	creator := dialAuthedBroadcastClient(t, url, creatorKey)

	createAndDelete := func(h, content string) {
		t.Helper()
		creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupCreateGroup, now, content, h))
		creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupPutUser, now, "", h, nostr.Tag{"p", memberKey.Public().Hex()}))
		creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
	}
	deletions := func(key nostr.SecretKey, h string) int {
		t.Helper()
		events, closed := dialAuthedBroadcastClient(t, url, key).subscribe("deleted", fmt.Sprintf(`{"kinds":[9008],"#h":[%q]}`, h))
		if closed != "" {
			t.Fatalf("subscription closed: %s", closed)
		}
		return len(events)
	}

	private := "private-" + strings.ToLower(RandomString(8))
	createAndDelete(private, `{"name":"Secret","private":true}`)
	if n := deletions(memberKey, private); n != 1 {
		t.Errorf("former member found %d deletion events for a deleted private group, want 1", n)
	}
	if n := deletions(outsiderKey, private); n != 1 {
		t.Errorf("non-member found %d deletion events for a deleted private group, want 1", n)
	}

	// A hidden group leaves nothing behind that would reveal it existed.
	hidden := "hidden-" + strings.ToLower(RandomString(8))
	createAndDelete(hidden, `{"name":"Hidden","private":true,"hidden":true}`)
	if n := deletions(memberKey, hidden); n != 0 {
		t.Errorf("former member found %d deletion events for a deleted hidden group, want 0", n)
	}
	if n := deletions(outsiderKey, hidden); n != 0 {
		t.Errorf("non-member found %d deletion events for a deleted hidden group, want 0", n)
	}
}
