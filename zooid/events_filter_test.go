package zooid

import (
	"encoding/json"
	"testing"

	"fiatjaf.com/nostr"
)

// A filter field that is present but empty, or a tag key no event is indexed
// under, constrains the query to nothing. Dropping it from the query would
// return every event instead.
func TestEventStore_QueryEvents_EmptyFilterValuesMatchNothing(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "indexed",
		Tags:      nostr.Tags{{"t", "nostr"}, {"title", "special"}},
	}
	event.Sign(nostr.Generate())
	if err := store.SaveEvent(event); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}

	tests := []struct {
		filter string
		want   int
	}{
		{`{}`, 1},
		{`{"#t":["nostr"]}`, 1},
		{`{"ids":[]}`, 0},
		{`{"authors":[]}`, 0},
		{`{"kinds":[]}`, 0},
		{`{"#t":[]}`, 0},
		{`{"#t":["nostr"],"#title":["special"]}`, 0},
	}

	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			var filter nostr.Filter
			if err := json.Unmarshal([]byte(tt.filter), &filter); err != nil {
				t.Fatalf("unmarshal %s: %v", tt.filter, err)
			}

			got := 0
			for range store.QueryEvents(filter, 0) {
				got++
			}
			if got != tt.want {
				t.Errorf("QueryEvents(%s) returned %d events, want %d", tt.filter, got, tt.want)
			}

			count, err := store.CountEvents(filter)
			if err != nil {
				t.Fatalf("CountEvents(%s): %v", tt.filter, err)
			}
			if int(count) != tt.want {
				t.Errorf("CountEvents(%s) = %d, want %d", tt.filter, count, tt.want)
			}
		})
	}
}
