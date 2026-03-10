package zooid

import (
	"encoding/json"
	"sync"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip29"
	"slices"
)

// NIP-29 group invite kind
const KindSimpleGroupCreateInvite nostr.Kind = 9009

// isWriteRestrictedGroupContent checks if group content contains write-restricted:true
func isWriteRestrictedGroupContent(content string) bool {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		return false
	}
	if wr, ok := data["write-restricted"].(bool); ok {
		return wr
	}
	return false
}

// isPrivateGroupContent checks if group creation content contains private:true
func isPrivateGroupContent(content string) bool {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		return false
	}
	if private, ok := data["private"].(bool); ok {
		return private
	}
	return false
}

// Utils

func GetGroupIDFromEvent(event nostr.Event) string {
	var tagName string

	if slices.Contains(nip29.MetadataEventKinds, event.Kind) {
		tagName = "d"
	} else {
		tagName = "h"
	}

	tag := event.Tags.Find(tagName)

	if tag != nil {
		return tag[1]
	}

	return ""
}

// Cache types

type groupMetaCache struct {
	event           nostr.Event
	found           bool
	private         bool
	hidden          bool
	closed          bool
	writeRestricted bool
}

type roleSet struct {
	mu    sync.RWMutex
	roles map[nostr.PubKey]map[string]struct{} // pubkey -> set of role names
}

type memberSet struct {
	mu      sync.RWMutex
	members map[nostr.PubKey]struct{}
}

// Struct definition

type GroupStore struct {
	Config     *Config
	Events     *EventStore
	Management *ManagementStore

	metadataCache   sync.Map // map[string]*groupMetaCache  (key = group h)
	membershipCache sync.Map // map[string]*memberSet        (key = group h)
	roleCache       sync.Map // map[string]*roleSet           (key = group h)
	creatorCache    sync.Map // map[string]nostr.PubKey       (key = group h)
	cachesWarmed    bool
}

func (g *GroupStore) WarmCaches() {
	// Load all group metadata
	metaFilter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupMetadata},
	}
	for event := range g.Events.QueryEvents(metaFilter, 0) {
		h := event.Tags.GetD()
		if h == "" {
			continue
		}
		g.metadataCache.Store(h, &groupMetaCache{
			event:           event,
			found:           true,
			private:         HasTag(event.Tags, "private"),
			hidden:          HasTag(event.Tags, "hidden"),
			closed:          HasTag(event.Tags, "closed"),
			writeRestricted: HasTag(event.Tags, "write-restricted"),
		})
	}

	// Load all group creators
	createFilter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupCreateGroup},
	}
	for event := range g.Events.QueryEvents(createFilter, 0) {
		h := GetGroupIDFromEvent(event)
		if h == "" {
			continue
		}
		g.creatorCache.Store(h, event.PubKey)
	}

	// Load all group memberships
	memberFilter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupPutUser, nostr.KindSimpleGroupRemoveUser},
	}
	// We need to process events oldest-first to replay the membership log correctly
	allEvents := slices.Collect(g.Events.QueryEvents(memberFilter, 0))
	for _, event := range Reversed(allEvents) {
		h := GetGroupIDFromEvent(event)
		if h == "" {
			continue
		}
		for tag := range event.Tags.FindAll("p") {
			pubkey, err := nostr.PubKeyFromHex(tag[1])
			if err != nil {
				continue
			}
			ms := g.getOrCreateMemberSet(h)
			ms.mu.Lock()
			if event.Kind == nostr.KindSimpleGroupPutUser {
				ms.members[pubkey] = struct{}{}
			} else {
				delete(ms.members, pubkey)
			}
			ms.mu.Unlock()

			// Track roles from put-user events (positions 2+ in the p tag)
			rs := g.getOrCreateRoleSet(h)
			rs.mu.Lock()
			if event.Kind == nostr.KindSimpleGroupPutUser {
				roles := make(map[string]struct{})
				for i := 2; i < len(tag); i++ {
					roles[tag[i]] = struct{}{}
				}
				// Always overwrite: a later put-user replaces previous roles
				rs.roles[pubkey] = roles
			} else {
				delete(rs.roles, pubkey)
			}
			rs.mu.Unlock()
		}
	}

	g.cachesWarmed = true
}

func (g *GroupStore) getOrCreateMemberSet(h string) *memberSet {
	if v, ok := g.membershipCache.Load(h); ok {
		return v.(*memberSet)
	}
	ms := &memberSet{members: make(map[nostr.PubKey]struct{})}
	actual, _ := g.membershipCache.LoadOrStore(h, ms)
	return actual.(*memberSet)
}

func (g *GroupStore) getOrCreateRoleSet(h string) *roleSet {
	if v, ok := g.roleCache.Load(h); ok {
		return v.(*roleSet)
	}
	rs := &roleSet{roles: make(map[nostr.PubKey]map[string]struct{})}
	actual, _ := g.roleCache.LoadOrStore(h, rs)
	return actual.(*roleSet)
}

// Metadata

func (g *GroupStore) GetMetadata(h string) (nostr.Event, bool) {
	if g.cachesWarmed {
		if v, ok := g.metadataCache.Load(h); ok {
			cached := v.(*groupMetaCache)
			return cached.event, cached.found
		}
		return nostr.Event{}, false
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupMetadata},
		Tags: nostr.TagMap{
			"d": []string{h},
		},
	}

	for event := range g.Events.QueryEvents(filter, 1) {
		return event, true
	}

	return nostr.Event{}, false
}

func (g *GroupStore) UpdateMetadata(event nostr.Event) error {
	tags := nostr.Tags{}
	var h string

	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "h" {
			h = tag[1]
			tags = append(tags, nostr.Tag{"d", tag[1]})
		} else {
			tags = append(tags, tag)
		}
	}

	// Parse content JSON and add appropriate visibility tags
	var contentData map[string]interface{}
	if err := json.Unmarshal([]byte(event.Content), &contentData); err == nil {
		if private, ok := contentData["private"].(bool); ok && private {
			tags = append(tags, nostr.Tag{"private"})
		}
		if closed, ok := contentData["closed"].(bool); ok && closed {
			tags = append(tags, nostr.Tag{"closed"})
		}
		if hidden, ok := contentData["hidden"].(bool); ok && hidden {
			tags = append(tags, nostr.Tag{"hidden"})
		}
		if wr, ok := contentData["write-restricted"].(bool); ok && wr {
			tags = append(tags, nostr.Tag{"write-restricted"})
		}
	}

	metadataEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupMetadata,
		CreatedAt: event.CreatedAt,
		Tags:      tags,
		Content:   event.Content, // Include metadata JSON (name, about, picture, etc.)
	}

	if err := g.Events.SignAndStoreEvent(&metadataEvent, true); err != nil {
		return err
	}

	if h != "" {
		g.metadataCache.Store(h, &groupMetaCache{
			event:           metadataEvent,
			found:           true,
			private:         HasTag(tags, "private"),
			hidden:          HasTag(tags, "hidden"),
			closed:          HasTag(tags, "closed"),
			writeRestricted: HasTag(tags, "write-restricted"),
		})
	}

	return nil
}

// Deletion

func (g *GroupStore) DeleteGroup(h string) {
	filters := []nostr.Filter{
		{
			Kinds: nip29.MetadataEventKinds,
			Tags: nostr.TagMap{
				"d": []string{h},
			},
		},
		{
			Tags: nostr.TagMap{
				"h": []string{h},
			},
		},
	}

	for _, filter := range filters {
		// Collect IDs first to avoid holding the DB connection during deletion
		var toDelete []nostr.ID
		for event := range g.Events.QueryEvents(filter, 0) {
			if event.Kind != nostr.KindSimpleGroupDeleteGroup {
				toDelete = append(toDelete, event.ID)
			}
		}
		for _, id := range toDelete {
			g.Events.DeleteEvent(id)
		}
	}

	g.metadataCache.Delete(h)
	g.membershipCache.Delete(h)
	g.roleCache.Delete(h)
	g.creatorCache.Delete(h)
}

// Admins

func (g *GroupStore) IsAdmin(h string, pubkey nostr.PubKey) bool {
	return g.Management.IsAdmin(pubkey)
}

func (g *GroupStore) GetAdmins(h string) []nostr.PubKey {
	// For private groups without relay admin access, only the creator is admin
	if h != "_" && g.IsPrivateGroup(h) && !g.Config.Groups.PrivateRelayAdminAccess {
		creator := g.GetGroupCreator(h)
		if creator != (nostr.PubKey{}) {
			return []nostr.PubKey{creator}
		}
		return []nostr.PubKey{}
	}
	return g.Management.GetAdmins()
}

func (g *GroupStore) UpdateAdminsList(h string) error {
	tags := nostr.Tags{
		nostr.Tag{"-"},
		nostr.Tag{"d", h},
	}

	for _, pubkey := range g.GetAdmins(h) {
		tags = append(tags, nostr.Tag{"p", pubkey.Hex()})
	}

	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupAdmins,
		CreatedAt: nostr.Now(),
		Tags:      tags,
	}

	return g.Events.SignAndStoreEvent(&event, true)
}

// Membership

func (g *GroupStore) AddMember(h string, pubkey nostr.PubKey) error {
	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupPutUser,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			nostr.Tag{"p", pubkey.Hex()},
			nostr.Tag{"h", h},
		},
	}

	if err := g.Events.SignAndStoreEvent(&event, true); err != nil {
		return err
	}

	ms := g.getOrCreateMemberSet(h)
	ms.mu.Lock()
	ms.members[pubkey] = struct{}{}
	ms.mu.Unlock()

	return nil
}

func (g *GroupStore) RemoveMember(h string, pubkey nostr.PubKey) error {
	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupRemoveUser,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			nostr.Tag{"p", pubkey.Hex()},
			nostr.Tag{"h", h},
		},
	}

	if err := g.Events.SignAndStoreEvent(&event, true); err != nil {
		return err
	}

	if v, ok := g.membershipCache.Load(h); ok {
		ms := v.(*memberSet)
		ms.mu.Lock()
		delete(ms.members, pubkey)
		ms.mu.Unlock()
	}

	return nil
}

func (g *GroupStore) IsMember(h string, pubkey nostr.PubKey) bool {
	if g.cachesWarmed {
		if v, ok := g.membershipCache.Load(h); ok {
			ms := v.(*memberSet)
			ms.mu.RLock()
			_, found := ms.members[pubkey]
			ms.mu.RUnlock()
			return found
		}
		return false
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupPutUser, nostr.KindSimpleGroupRemoveUser},
		Tags: nostr.TagMap{
			"p": []string{pubkey.Hex()},
			"h": []string{h},
		},
	}

	for event := range g.Events.QueryEvents(filter, 1) {
		if event.Kind == nostr.KindSimpleGroupPutUser {
			return true
		}

		if event.Kind == nostr.KindSimpleGroupRemoveUser {
			return false
		}
	}

	return false
}

func (g *GroupStore) GetMembers(h string) []nostr.PubKey {
	if g.cachesWarmed {
		if v, ok := g.membershipCache.Load(h); ok {
			ms := v.(*memberSet)
			ms.mu.RLock()
			result := make([]nostr.PubKey, 0, len(ms.members))
			for pk := range ms.members {
				result = append(result, pk)
			}
			ms.mu.RUnlock()
			return result
		}
		return []nostr.PubKey{}
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupPutUser, nostr.KindSimpleGroupRemoveUser},
		Tags: nostr.TagMap{
			"h": []string{h},
		},
	}

	members := make(map[nostr.PubKey]struct{})

	for _, event := range Reversed(slices.Collect(g.Events.QueryEvents(filter, 0))) {
		for tag := range event.Tags.FindAll("p") {
			if pubkey, err := nostr.PubKeyFromHex(tag[1]); err == nil {
				if event.Kind == nostr.KindSimpleGroupPutUser {
					members[pubkey] = struct{}{}
				} else {
					delete(members, pubkey)
				}
			}
		}
	}

	return Keys(members)
}

func (g *GroupStore) UpdateMembersList(h string) error {
	tags := nostr.Tags{
		nostr.Tag{"-"},
		nostr.Tag{"d", h},
	}

	for _, pubkey := range g.GetMembers(h) {
		pTag := nostr.Tag{"p", pubkey.Hex()}
		// Append roles if any exist in the role cache
		if v, ok := g.roleCache.Load(h); ok {
			rs := v.(*roleSet)
			rs.mu.RLock()
			if roles, exists := rs.roles[pubkey]; exists {
				for role := range roles {
					pTag = append(pTag, role)
				}
			}
			rs.mu.RUnlock()
		}
		tags = append(tags, pTag)
	}

	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupMembers,
		CreatedAt: nostr.Now(),
		Tags:      tags,
	}

	return g.Events.SignAndStoreEvent(&event, true)
}

// Invite Codes

// ValidateInviteCode checks if an invite code is valid for a group
func (g *GroupStore) ValidateInviteCode(h string, code string) bool {
	if code == "" {
		return false
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{KindSimpleGroupCreateInvite},
		Tags: nostr.TagMap{
			"h": []string{h},
		},
	}

	for event := range g.Events.QueryEvents(filter, 0) {
		codeTag := event.Tags.Find("code")
		if codeTag != nil && len(codeTag) >= 2 && codeTag[1] == code {
			return true
		}
	}

	return false
}

// GetInviteCodeFromEvent extracts the invite code from an event's tags
func GetInviteCodeFromEvent(event nostr.Event) string {
	tag := event.Tags.Find("code")
	if tag != nil && len(tag) >= 2 {
		return tag[1]
	}
	return ""
}

// Private group helpers

func (g *GroupStore) IsPrivateGroup(h string) bool {
	if g.cachesWarmed {
		if v, ok := g.metadataCache.Load(h); ok {
			return v.(*groupMetaCache).private
		}
		return false
	}

	meta, found := g.GetMetadata(h)
	if !found {
		return false
	}
	return HasTag(meta.Tags, "private")
}

func (g *GroupStore) GetGroupCreator(h string) nostr.PubKey {
	if g.cachesWarmed {
		if v, ok := g.creatorCache.Load(h); ok {
			return v.(nostr.PubKey)
		}
		return nostr.PubKey{}
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupCreateGroup},
		Tags:  nostr.TagMap{"h": []string{h}},
	}
	for event := range g.Events.QueryEvents(filter, 1) {
		return event.PubKey
	}
	return nostr.PubKey{}
}

func (g *GroupStore) IsGroupCreator(h string, pubkey nostr.PubKey) bool {
	return g.GetGroupCreator(h) == pubkey
}

// Write restriction helpers

func (g *GroupStore) IsWriteRestricted(h string) bool {
	if g.cachesWarmed {
		if v, ok := g.metadataCache.Load(h); ok {
			return v.(*groupMetaCache).writeRestricted
		}
		return false
	}

	meta, found := g.GetMetadata(h)
	if !found {
		return false
	}
	return HasTag(meta.Tags, "write-restricted")
}

func (g *GroupStore) HasRole(h string, pubkey nostr.PubKey, role string) bool {
	if v, ok := g.roleCache.Load(h); ok {
		rs := v.(*roleSet)
		rs.mu.RLock()
		defer rs.mu.RUnlock()
		if roles, exists := rs.roles[pubkey]; exists {
			_, has := roles[role]
			return has
		}
	}
	return false
}

// CanWrite checks if a user can post content to a write-restricted group.
// Returns true if the group is not write-restricted, or if the user is an
// admin, group creator, or has the "writer" role.
func (g *GroupStore) CanWrite(h string, pubkey nostr.PubKey) bool {
	if !g.IsWriteRestricted(h) {
		return true
	}
	if g.Config.CanManage(pubkey) || g.IsGroupCreator(h, pubkey) {
		return true
	}
	return g.HasRole(h, pubkey, "writer")
}

// SetMemberRoles updates the role cache for a member in a group.
// The roles slice replaces any previous roles for this pubkey.
func (g *GroupStore) SetMemberRoles(h string, pubkey nostr.PubKey, roles []string) {
	rs := g.getOrCreateRoleSet(h)
	rs.mu.Lock()
	roleMap := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		roleMap[r] = struct{}{}
	}
	rs.roles[pubkey] = roleMap
	rs.mu.Unlock()
}

// ClearMemberRoles removes all roles for a member in a group.
func (g *GroupStore) ClearMemberRoles(h string, pubkey nostr.PubKey) {
	if v, ok := g.roleCache.Load(h); ok {
		rs := v.(*roleSet)
		rs.mu.Lock()
		delete(rs.roles, pubkey)
		rs.mu.Unlock()
	}
}

// Other stuff

func (g *GroupStore) HasAccess(h string, pubkey nostr.PubKey) bool {
	// For private groups without relay admin access, only members and creator have access
	if g.IsPrivateGroup(h) && !g.Config.Groups.PrivateRelayAdminAccess {
		return g.IsMember(h, pubkey) || g.IsGroupCreator(h, pubkey)
	}
	return g.Config.CanManage(pubkey) || g.IsAdmin(h, pubkey) || g.IsMember(h, pubkey)
}

func (g *GroupStore) IsGroupEvent(event nostr.Event) bool {
	if slices.Contains(nip29.MetadataEventKinds, event.Kind) {
		return true
	}

	if slices.Contains(nip29.ModerationEventKinds, event.Kind) {
		return true
	}

	joinKinds := []nostr.Kind{
		nostr.KindSimpleGroupJoinRequest,
		nostr.KindSimpleGroupLeaveRequest,
	}

	if slices.Contains(joinKinds, event.Kind) {
		return true
	}

	return GetGroupIDFromEvent(event) != ""
}

func (g *GroupStore) CanRead(pubkey nostr.PubKey, event nostr.Event) bool {
	if !g.Config.Groups.Enabled {
		return false
	}

	h := GetGroupIDFromEvent(event)

	// Relay-level events (h="_") are always readable
	// This includes the relay admin list (GROUP_ADMINS with d="_")
	if h == "_" {
		return true
	}

	meta, found := g.GetMetadata(h)

	if !found {
		return false
	}

	if HasTag(meta.Tags, "hidden") && !g.HasAccess(h, pubkey) {
		return false
	}

	if event.Kind == nostr.KindSimpleGroupMetadata {
		return true
	}

	if event.Kind == nostr.KindSimpleGroupDeleteGroup {
		return true
	}

	// For private groups, require membership
	if HasTag(meta.Tags, "private") && !g.HasAccess(h, pubkey) {
		return false
	}

	// For public groups with open policy, allow all authenticated users to read
	if g.Config.Policy.Open && !HasTag(meta.Tags, "private") {
		return true
	}

	// Otherwise require group membership
	return g.HasAccess(h, pubkey)
}

func (g *GroupStore) CheckWrite(event nostr.Event) string {
	if !g.Config.Groups.Enabled {
		return "invalid: groups are not enabled"
	}

	if slices.Contains(nip29.MetadataEventKinds, event.Kind) {
		return "invalid: group metadata cannot be set directly"
	}

	h := GetGroupIDFromEvent(event)
	meta, found := g.GetMetadata(h)

	if event.Kind == nostr.KindSimpleGroupCreateGroup {
		if found {
			return "invalid: that group already exists"
		}
		// If admin_create_only is set, only admins can create groups
		if g.Config.Groups.AdminCreateOnly && !g.Config.CanManage(event.PubKey) {
			return "restricted: only admins can create groups"
		}
		// If private_admin_only is set, check if group is private
		if g.Config.Groups.PrivateAdminOnly && !g.Config.CanManage(event.PubKey) {
			if isPrivateGroupContent(event.Content) {
				return "restricted: only admins can create private groups"
			}
		}
		// Write-restricted groups can only be created by relay admins
		if isWriteRestrictedGroupContent(event.Content) && !g.Config.CanManage(event.PubKey) {
			return "restricted: only admins can create write-restricted groups"
		}
		// Group creation check passed, don't apply general ModerationEventKinds check
		return ""
	} else if !found {
		return "invalid: group not found"
	}

	if slices.Contains(nip29.ModerationEventKinds, event.Kind) {
		if g.IsPrivateGroup(h) && !g.Config.Groups.PrivateRelayAdminAccess {
			// For private groups without relay admin access, only the creator can moderate
			if !g.IsGroupCreator(h, event.PubKey) {
				return "restricted: only the group creator can manage private groups"
			}
		} else if !g.Config.CanManage(event.PubKey) && !g.IsGroupCreator(h, event.PubKey) {
			return "restricted: you are not authorized to manage groups"
		}
		// Only relay admins can set write-restricted on a group via metadata edit
		if event.Kind == nostr.KindSimpleGroupEditMetadata &&
			isWriteRestrictedGroupContent(event.Content) && !g.Config.CanManage(event.PubKey) {
			return "restricted: only admins can set write-restricted on groups"
		}
	}

	// Handle join requests - check invite code for private/hidden groups
	if event.Kind == nostr.KindSimpleGroupJoinRequest {
		if g.IsMember(h, event.PubKey) {
			return "duplicate: already a member"
		}

		isPrivate := HasTag(meta.Tags, "private")
		isHidden := HasTag(meta.Tags, "hidden")

		// For private or hidden groups, require a valid invite code
		if isPrivate || isHidden {
			inviteCode := GetInviteCodeFromEvent(event)
			if !g.ValidateInviteCode(h, inviteCode) {
				if isHidden {
					// Don't reveal that the group exists
					return "invalid: group not found"
				}
				return "restricted: valid invite code required to join this group"
			}
		}

		return ""
	}

	// For non-join requests, hidden groups require access
	if HasTag(meta.Tags, "hidden") && !g.HasAccess(h, event.PubKey) {
		return "invalid: group not found"
	}

	if event.Kind == nostr.KindSimpleGroupLeaveRequest {
		if !g.IsMember(h, event.PubKey) {
			return "duplicate: not currently a member"
		} else {
			return ""
		}
	}

	if HasTag(meta.Tags, "closed") && !g.HasAccess(h, event.PubKey) {
		return "restricted: you are not a member of that group"
	}

	// Write-restricted check: only users with "writer" role, admins, or creator can post
	if HasTag(meta.Tags, "write-restricted") && !g.CanWrite(h, event.PubKey) {
		return "restricted: this group only allows designated writers to post"
	}

	return ""
}

// Middleware

func (g *GroupStore) Enable(instance *Instance) {
	instance.Relay.Info.SupportedNIPs = append(instance.Relay.Info.SupportedNIPs, 29)
}
