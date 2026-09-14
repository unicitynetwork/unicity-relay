package zooid

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

// Two deletions of the same group can both pass OnEvent's checks before either
// is applied. The one handled second finds the group already gone and must not
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
			first := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
			second := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+2, "", h))
			acceptGroupEvent(t, instance, first)
			acceptGroupEvent(t, instance, second)

			storeAndHandle(t, instance, first)
			// The second deletion is stored after the first has been applied.
			storeAndHandle(t, instance, second)

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
	acceptGroupEvent(t, instance, first)
	acceptGroupEvent(t, instance, second)

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
	acceptGroupEvent(t, instance, first)
	acceptGroupEvent(t, instance, second)

	storeAndHandle(t, instance, first)
	deletion := signedEvent(t, firstKey, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
	acceptGroupEvent(t, instance, deletion)
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

// A client can send the same event again, over another connection or as a
// retry, before the relay has applied it, so both copies pass OnEvent. Only one
// copy may be applied, and the other must not remove the event it stored.
func TestOnEventSaved_DuplicateDelivery(t *testing.T) {
	t.Run("creation", func(t *testing.T) {
		instance := createTestInstance()
		creator := nostr.Generate()
		h := "group-" + strings.ToLower(RandomString(8))

		creation := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, nostr.Now(), `{"name":"Group"}`, h))
		acceptGroupEvent(t, instance, creation)
		acceptGroupEvent(t, instance, creation)
		storeAndHandle(t, instance, creation)
		storeAndHandle(t, instance, creation)

		// WarmCaches restores the group's creator from this event on startup.
		if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupCreateGroup, h); n != 1 {
			t.Errorf("stored creation events = %d, want 1", n)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		instance := createTestInstance()
		creator := nostr.Generate()
		h := "group-" + strings.ToLower(RandomString(8))
		now := nostr.Now()

		saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now, `{"name":"Group"}`, h))
		deletion := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
		acceptGroupEvent(t, instance, deletion)
		acceptGroupEvent(t, instance, deletion)
		storeAndHandle(t, instance, deletion)
		storeAndHandle(t, instance, deletion)

		if _, found := instance.Groups.GetMetadata(h); found {
			t.Error("the group was not deleted")
		}
		// Clients catching up on kind 9008 learn of the deletion from this event.
		if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupDeleteGroup, h); n != 1 {
			t.Errorf("stored deletion events = %d, want 1", n)
		}
	})
}

// Deleting a hidden group also deletes its kind 9008, so a second copy of the
// deletion, accepted alongside the first, is stored again instead of being
// reported as a duplicate. If the group was recreated in the meantime, that copy
// must not delete the new group.
func TestOnEventSaved_DuplicateDeletionSparesRecreatedGroup(t *testing.T) {
	instance := createTestInstance()
	creator := nostr.Generate()
	h := "group-" + strings.ToLower(RandomString(8))
	now := nostr.Now()
	hidden := `{"private":true,"hidden":true}`

	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now, hidden, h))
	deletion := signedEvent(t, creator, groupEvent(nostr.KindSimpleGroupDeleteGroup, now+1, "", h))
	acceptGroupEvent(t, instance, deletion)
	acceptGroupEvent(t, instance, deletion)

	storeAndHandle(t, instance, deletion)
	saveAndHandle(t, instance, creator, groupEvent(nostr.KindSimpleGroupCreateGroup, now+2, hidden, h))
	storeAndHandle(t, instance, deletion)

	if _, found := instance.Groups.GetMetadata(h); !found {
		t.Error("a second copy of the first group's deletion deleted the recreated group")
	}
	if n := storedGroupEvents(t, instance, nostr.KindSimpleGroupDeleteGroup, h); n != 0 {
		t.Errorf("stored deletion events = %d, want 0", n)
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

// acceptGroupEvent runs the group check OnEvent runs on evt, and fails the test
// if the check rejects it.
func acceptGroupEvent(t *testing.T, instance *Instance, evt nostr.Event) {
	t.Helper()
	check := instance.Groups.CheckWrite
	if evt.Kind == nostr.KindSimpleGroupDeleteGroup {
		check = instance.Groups.CheckDeletion
	}
	if reason := check(evt); reason != "" {
		t.Fatalf("kind %d event rejected: %s", evt.Kind, reason)
	}
}

// storeAndHandle hands evt, already accepted, to khatru's handling of an
// accepted event: khatru stores it through Instance.StoreEvent and runs
// OnEventSaved, unless the store reports the event as a duplicate.
func storeAndHandle(t *testing.T, instance *Instance, evt nostr.Event) {
	t.Helper()
	instance.Relay.StoreEvent = instance.StoreEvent
	instance.Relay.OnEventSaved = instance.OnEventSaved
	if _, err := instance.Relay.AddEvent(context.Background(), evt); err != nil {
		t.Fatalf("AddEvent: %v", err)
	}
}

func saveAndHandle(t *testing.T, instance *Instance, secret nostr.SecretKey, evt nostr.Event) {
	t.Helper()
	evt = signedEvent(t, secret, evt)
	acceptGroupEvent(t, instance, evt)
	storeAndHandle(t, instance, evt)
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
