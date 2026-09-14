package zooid

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

// Two deletions of the same group can both pass CheckWrite before either is
// applied. The one handled second finds the group already gone and must not
// leave its kind 9008 behind: CanRead would expose it for a hidden group, and
// for any group it would be a second deletion notice.
func TestOnEventSaved_DuplicateGroupDeletion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "hidden group", content: `{"private":true,"hidden":true}`, want: 0},
		{name: "visible group", content: `{"private":true}`, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := createTestInstance()
			creator := nostr.Generate()
			h := "group-" + strings.ToLower(RandomString(8))
			now := nostr.Now()

			saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now, tt.content, h))
			saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
			// The second deletion is stored after the first has been applied.
			saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+2, "", h))

			if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupDeleteGroup, h); n != tt.want {
				t.Errorf("stored deletion events = %d, want %d", n, tt.want)
			}
		})
	}
}

// Recreating a deleted group must drop the old deletion, or clients catching up
// on kind 9008 would treat the new group as deleted.
func TestOnEventSaved_RecreatedGroupDropsOldDeletion(t *testing.T) {
	instance := createTestInstance()
	creator := nostr.Generate()
	h := "group-" + strings.ToLower(RandomString(8))
	now := nostr.Now()

	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"First"}`, h))
	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
	if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupDeleteGroup, h); n != 1 {
		t.Fatalf("stored deletion events after deleting = %d, want 1", n)
	}

	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now+2, `{"name":"Second"}`, h))
	if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupDeleteGroup, h); n != 0 {
		t.Errorf("stored deletion events after recreating = %d, want 0", n)
	}
}

// A deletion accepted while a group exists must not be applied to a group
// created under the same ID after that group was deleted.
func TestOnEventSaved_StaleDeletionSparesRecreatedGroup(t *testing.T) {
	instance := createTestInstance()
	creator := nostr.Generate()
	h := "group-" + strings.ToLower(RandomString(8))
	now := nostr.Now()

	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"First"}`, h))

	// Both deletions are accepted, as OnEvent accepts them, while the first
	// group exists.
	first := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
	second := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+2, "", h))
	for _, evt := range []nostr.Event{first, second} {
		if reason := instance.Groups.CheckDeletion(evt); reason != "" {
			t.Fatalf("CheckDeletion rejected a deletion: %s", reason)
		}
	}

	storeAndHandle(t, instance, first)
	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now+3, `{"name":"Second"}`, h))
	storeAndHandle(t, instance, second)

	if _, found := instance.Groups.GetMetadata(h); !found {
		t.Error("a deletion accepted for the first group deleted the recreated group")
	}
	if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupDeleteGroup, h); n != 0 {
		t.Errorf("stored deletion events = %d, want 0", n)
	}
}

// Two creations of the same group ID can both pass CheckWrite before either is
// applied. The one applied second must not take over the group, and must not
// start a new incarnation that would make a deletion accepted for the group
// stale.
func TestOnEventSaved_DuplicateGroupCreation(t *testing.T) {
	instance := createTestInstance()
	firstKey, secondKey := nostr.Generate(), nostr.Generate()
	h := "group-" + strings.ToLower(RandomString(8))
	now := nostr.Now()

	first := signedEvent(t, firstKey, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"First"}`, h))
	second := signedEvent(t, secondKey, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"Second"}`, h))
	for _, evt := range []nostr.Event{first, second} {
		if reason := instance.Groups.CheckWrite(evt); reason != "" {
			t.Fatalf("CheckWrite rejected a creation: %s", reason)
		}
	}

	storeAndHandle(t, instance, first)
	deletion := signedEvent(t, firstKey, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
	if reason := instance.Groups.CheckDeletion(deletion); reason != "" {
		t.Fatalf("CheckDeletion rejected the deletion: %s", reason)
	}
	storeAndHandle(t, instance, second)

	if creator := instance.Groups.GetGroupCreator(h); creator != firstKey.Public() {
		t.Errorf("group creator = %s, want the first creator %s", creator.Hex(), firstKey.Public().Hex())
	}
	if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupCreateGroup, h); n != 1 {
		t.Errorf("stored creation events = %d, want 1", n)
	}

	storeAndHandle(t, instance, deletion)
	if _, found := instance.Groups.GetMetadata(h); found {
		t.Error("the deletion accepted for the group was not applied")
	}
}

// OnEventSaved broadcasts a group creation once it has applied it, and
// PreventBroadcast withholds khatru's own broadcast of the same event.
func TestBroadcast_GroupCreationOnce(t *testing.T) {
	_, url := startBroadcastTestRelay(t)
	h := "group-" + strings.ToLower(RandomString(8))
	creatorKey, outsiderKey := nostr.Generate(), nostr.Generate()

	outsider := dialAuthedBroadcastClient(t, url, outsiderKey)
	outsider.mustSubscribe("live", fmt.Sprintf(`{"kinds":[9007],"#h":[%q]}`, h))

	creator := dialAuthedBroadcastClient(t, url, creatorKey)
	creator.publishEvent(creatorKey, groupEvent(nostr.KindSimpleGroupCreateGroup, nostr.Now(), `{"name":"Open"}`, h))

	if evt := outsider.waitEvent("live", broadcastWait); evt == nil {
		t.Fatal("non-member did not receive the creation of a public group")
	}
	if evt := outsider.waitEvent("live", broadcastWait); evt != nil {
		t.Error("non-member received the creation of a public group twice")
	}
}

func signedEvent(t *testing.T, secret nostr.SecretKey, evt nostr.Event) nostr.Event {
	t.Helper()
	if err := evt.Sign(secret); err != nil {
		t.Fatal(err)
	}
	return evt
}

// storeAndHandle stores evt and runs OnEventSaved, as khatru does for an
// accepted event.
func storeAndHandle(t *testing.T, instance *Instance, evt nostr.Event) {
	t.Helper()
	if err := instance.Events.StoreEvent(evt); err != nil {
		t.Fatalf("StoreEvent: %v", err)
	}
	instance.OnEventSaved(context.Background(), evt)
}

func saveAndHandle(t *testing.T, instance *Instance, secret nostr.SecretKey, evt nostr.Event) {
	t.Helper()
	storeAndHandle(t, instance, signedEvent(t, secret, evt))
}

func storedGroupEvents(t *testing.T, instance *Instance, kind nostr.Kind, h string) int {
	t.Helper()
	filter := nostr.Filter{
		Kinds: []nostr.Kind{kind},
		Tags:  nostr.TagMap{"h": []string{h}},
	}
	n := 0
	for range instance.Events.QueryEvents(filter, 0) {
		n++
	}
	return n
}
