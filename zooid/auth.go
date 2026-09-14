package zooid

import (
	"context"
	"reflect"
	"sync"
	"unsafe"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

// authLockOffset locates khatru.WebSocket's unexported authLock, the mutex
// khatru's AUTH handler holds while it changes AuthedPublicKeys. It is resolved
// when the package initializes, so a khatru change that renames or retypes the
// field stops the relay from starting instead of breaking broadcasts.
var authLockOffset = func() uintptr {
	field, ok := reflect.TypeFor[khatru.WebSocket]().FieldByName("authLock")
	if !ok || field.Type != reflect.TypeFor[sync.Mutex]() {
		panic("zooid: khatru.WebSocket.authLock is missing or not a sync.Mutex")
	}
	return field.Offset
}()

// lastAuthedPubkey returns the pubkey khatru.GetAuthed returns for ws, the last
// one to authenticate, but reads AuthedPublicKeys under authLock. An
// unsynchronized read while an AUTH message appends to the slice can pair the
// new length with the old backing array and index past its end.
func lastAuthedPubkey(ws *khatru.WebSocket) (nostr.PubKey, bool) {
	lock := (*sync.Mutex)(unsafe.Add(unsafe.Pointer(ws), authLockOffset))
	lock.Lock()
	defer lock.Unlock()

	if len(ws.AuthedPublicKeys) == 0 {
		return nostr.ZeroPK, false
	}
	return ws.AuthedPublicKeys[len(ws.AuthedPublicKeys)-1], true
}

// getAuthed is khatru.GetAuthed with a websocket connection's pubkey read
// through lastAuthedPubkey. NIP-86 requests arrive over HTTP without a
// connection, so khatru resolves those.
func getAuthed(ctx context.Context) (nostr.PubKey, bool) {
	if ws := khatru.GetConnection(ctx); ws != nil {
		return lastAuthedPubkey(ws)
	}
	return khatru.GetAuthed(ctx)
}
