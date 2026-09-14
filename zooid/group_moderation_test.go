package zooid

import (
	"fmt"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

// Clients catch up on removals that happened while they were offline by
// querying kinds 9005 and 9001 for their groups when they reconnect. This test
// makes the change first and queries afterwards.

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

	filter := fmt.Sprintf(`{"kinds":[9005,9001],"#h":[%q]}`, h)
	events, closed := dialAuthedBroadcastClient(t, url, removedKey).subscribe("moderation", filter)
	if closed != "" {
		t.Fatalf("subscription closed: %s", closed)
	}
	if len(events) != 1 || events[0].Tags.FindWithValue("p", removed.Hex()) == nil {
		t.Fatalf("removed user got %d moderation events, want only the removal naming them", len(events))
	}
}
