package zooid

import (
	"log"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/Masterminds/squirrel"
	"github.com/prometheus/client_golang/prometheus"
)

var collectMu sync.Mutex

// Chat message kinds for the messages_total metric (NIP-29 group chat)
var chatKinds = []nostr.Kind{9, 10}

var (
	groupsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_groups_total",
		Help: "Total number of groups",
	}, []string{"instance"})

	groupsPrivate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_groups_private",
		Help: "Number of private groups",
	}, []string{"instance"})

	groupsHidden = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_groups_hidden",
		Help: "Number of hidden groups",
	}, []string{"instance"})

	groupsClosed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_groups_closed",
		Help: "Number of closed groups",
	}, []string{"instance"})

	groupMembers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_group_members",
		Help: "Number of members per group",
	}, []string{"instance", "group"})

	groupMessages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_group_messages",
		Help: "Number of chat messages per group",
	}, []string{"instance", "group"})

	groupMembersTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_group_members_total",
		Help: "Distinct members across all groups (each pubkey counted once)",
	}, []string{"instance"})

	groupsTracked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_groups_tracked",
		Help: "Number of groups reported in per-group member metrics (capped at 1000)",
	}, []string{"instance"})

	relayMembersTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_relay_members_total",
		Help: "Total relay members",
	}, []string{"instance"})

	bannedPubkeysTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_banned_pubkeys_total",
		Help: "Total banned pubkeys",
	}, []string{"instance"})

	bannedEventsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_banned_events_total",
		Help: "Total banned events",
	}, []string{"instance"})

	eventsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_events_total",
		Help: "Estimated total events in database",
	}, []string{"instance"})

	messagesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_messages_total",
		Help: "Total chat messages in database",
	}, []string{"instance"})

	QueryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zooid_query_duration_seconds",
		Help:    "Duration of database queries (DB execution and row scanning)",
		Buckets: prometheus.DefBuckets,
	}, []string{"instance"})
)

func init() {
	prometheus.MustRegister(
		groupsTotal,
		groupsPrivate,
		groupsHidden,
		groupsClosed,
		groupMembers,
		groupMessages,
		groupMembersTotal,
		groupsTracked,
		relayMembersTotal,
		bannedPubkeysTotal,
		bannedEventsTotal,
		eventsTotal,
		messagesTotal,
		QueryDuration,
	)
}

// GetAllInstances returns a snapshot of all loaded instances.
func GetAllInstances() []*Instance {
	instancesMux.RLock()
	defer instancesMux.RUnlock()

	result := make([]*Instance, 0, len(instancesByName))
	for _, inst := range instancesByName {
		result = append(result, inst)
	}
	return result
}

// instanceLabel returns the instance label value for metrics, derived from config.
func instanceLabel(inst *Instance) string {
	return inst.Config.Schema
}

const maxTrackedGroups = 1000

// StartMetricsCollector launches background goroutines that update
// Prometheus metrics. Most metrics refresh every 30 seconds; per-group
// message counts run every 2 minutes to reduce CPU load.
func StartMetricsCollector() {
	go func() {
		collectMetrics()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			collectMetrics()
		}
	}()

	go func() {
		collectGroupMessages()

		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			collectGroupMessages()
		}
	}()
}

// activeInstances tracks which instance labels were seen in the last collection,
// so we can clean up metrics for unloaded instances.
var activeInstances = make(map[string]struct{})

func collectMetrics() {
	collectMu.Lock()
	defer collectMu.Unlock()

	instances := GetAllInstances()

	currentInstances := make(map[string]struct{}, len(instances))
	for _, inst := range instances {
		label := instanceLabel(inst)
		currentInstances[label] = struct{}{}
		collectCacheMetrics(inst)
		collectDBMetrics(inst)
	}

	// Delete metrics for instances that were unloaded since last cycle.
	for label := range activeInstances {
		if _, ok := currentInstances[label]; !ok {
			match := prometheus.Labels{"instance": label}
			groupsTotal.DeletePartialMatch(match)
			groupsPrivate.DeletePartialMatch(match)
			groupsHidden.DeletePartialMatch(match)
			groupsClosed.DeletePartialMatch(match)
			groupMembers.DeletePartialMatch(match)
			groupMembersTotal.DeletePartialMatch(match)
			groupsTracked.DeletePartialMatch(match)
			relayMembersTotal.DeletePartialMatch(match)
			bannedPubkeysTotal.DeletePartialMatch(match)
			bannedEventsTotal.DeletePartialMatch(match)
			eventsTotal.DeletePartialMatch(match)
			messagesTotal.DeletePartialMatch(match)
		}
	}

	activeInstances = currentInstances
}

func collectCacheMetrics(inst *Instance) {
	instLabel := instanceLabel(inst)
	label := prometheus.Labels{"instance": instLabel}

	// Group counts from metadataCache
	var total, private, hidden, closed float64
	inst.Groups.metadataCache.Range(func(_, value any) bool {
		meta := value.(*groupMetaCache)
		total++
		if meta.private {
			private++
		}
		if meta.hidden {
			hidden++
		}
		if meta.closed {
			closed++
		}
		return true
	})
	groupsTotal.With(label).Set(total)
	groupsPrivate.With(label).Set(private)
	groupsHidden.With(label).Set(hidden)
	groupsClosed.With(label).Set(closed)

	// Per-group member counts from membershipCache.
	// Delete only this instance's stale per-group series.
	groupMembers.DeletePartialMatch(prometheus.Labels{"instance": instLabel})

	distinctMembers := make(map[nostr.PubKey]struct{})
	var tracked int
	inst.Groups.membershipCache.Range(func(key, value any) bool {
		ms := value.(*memberSet)
		ms.mu.RLock()
		count := float64(len(ms.members))
		for pk := range ms.members {
			distinctMembers[pk] = struct{}{}
		}
		ms.mu.RUnlock()

		// Skip private/hidden groups to avoid leaking their IDs via /metrics
		if tracked < maxTrackedGroups {
			h := key.(string)
			if v, ok := inst.Groups.metadataCache.Load(h); ok {
				meta := v.(*groupMetaCache)
				if meta.private || meta.hidden {
					return true
				}
			}
			groupMembers.With(prometheus.Labels{
				"instance": instLabel,
				"group":    h,
			}).Set(count)
			tracked++
		}
		return true
	})

	groupMembersTotal.With(label).Set(float64(len(distinctMembers)))
	groupsTracked.With(label).Set(float64(tracked))

	// Relay members
	var relayCount float64
	inst.Management.relayMembers.Range(func(_, _ any) bool {
		relayCount++
		return true
	})
	relayMembersTotal.With(label).Set(relayCount)

	// Banned pubkeys
	var bannedPKCount float64
	inst.Management.bannedPubkeys.Range(func(_, _ any) bool {
		bannedPKCount++
		return true
	})
	bannedPubkeysTotal.With(label).Set(bannedPKCount)

	// Banned events
	var bannedEvCount float64
	inst.Management.bannedEvents.Range(func(_, _ any) bool {
		bannedEvCount++
		return true
	})
	bannedEventsTotal.With(label).Set(bannedEvCount)
}

func collectDBMetrics(inst *Instance) {
	instLabel := instanceLabel(inst)
	label := prometheus.Labels{"instance": instLabel}
	eventsTable := inst.Events.Schema.Prefix("events")

	// Use Postgres reltuples estimate — no sequential scan, instant.
	// GREATEST handles -1 (never-analyzed tables). PostgreSQL lowercases
	// unquoted identifiers, so match against lowercase.
	var eventsEst float64
	err := GetDb().QueryRow(
		"SELECT GREATEST(COALESCE(reltuples, 0), 0) FROM pg_class WHERE relname = $1",
		strings.ToLower(eventsTable),
	).Scan(&eventsEst)
	if err != nil {
		log.Printf("metrics: failed to estimate events: %v", err)
	} else {
		eventsTotal.With(label).Set(eventsEst)
	}

	// Chat message count — use COUNT with kind filter (hits the kind index).
	msgCount, err := inst.Events.CountEvents(nostr.Filter{Kinds: chatKinds})
	if err != nil {
		log.Printf("metrics: failed to count messages: %v", err)
	} else {
		messagesTotal.With(label).Set(float64(msgCount))
	}

}

var (
	groupMessagesMu             sync.Mutex
	activeGroupMessageInstances = make(map[string]struct{})
)

func collectGroupMessages() {
	groupMessagesMu.Lock()
	defer groupMessagesMu.Unlock()

	instances := GetAllInstances()

	currentInstances := make(map[string]struct{}, len(instances))
	for _, inst := range instances {
		instLabel := instanceLabel(inst)
		currentInstances[instLabel] = struct{}{}
		groupMessages.DeletePartialMatch(prometheus.Labels{"instance": instLabel})
		collectGroupMessageCounts(inst, instLabel)
	}

	for label := range activeGroupMessageInstances {
		if _, ok := currentInstances[label]; !ok {
			groupMessages.DeletePartialMatch(prometheus.Labels{"instance": label})
		}
	}
	activeGroupMessageInstances = currentInstances
}

func collectGroupMessageCounts(inst *Instance, instLabel string) {
	// Collect visible group IDs (not private, not hidden), capped at maxTrackedGroups.
	visibleGroups := make([]string, 0, maxTrackedGroups)
	inst.Groups.metadataCache.Range(func(key, value any) bool {
		if len(visibleGroups) >= maxTrackedGroups {
			return false
		}
		meta := value.(*groupMetaCache)
		if meta.private || meta.hidden {
			return true
		}
		visibleGroups = append(visibleGroups, key.(string))
		return true
	})

	if len(visibleGroups) == 0 {
		return
	}

	// Build: SELECT t.value, COUNT(*) FROM {event_tags} t
	//        JOIN {events} e ON e.id = t.event_id
	//        WHERE t.key = 'h' AND t.value IN (...) AND e.kind IN (9, 10)
	//        GROUP BY t.value
	eventsTable := inst.Events.Schema.Prefix("events")
	tagsTable := inst.Events.Schema.Prefix("event_tags")

	kindArgs := make([]interface{}, len(chatKinds))
	for i, k := range chatKinds {
		kindArgs[i] = int(k)
	}
	groupArgs := make([]interface{}, len(visibleGroups))
	for i, g := range visibleGroups {
		groupArgs[i] = g
	}

	qb := sb.Select("t.value", "COUNT(*)").
		From(tagsTable + " t").
		Join(eventsTable + " e ON e.id = t.event_id").
		Where(squirrel.Eq{"t.key": "h"}).
		Where(squirrel.Eq{"t.value": groupArgs}).
		Where(squirrel.Eq{"e.kind": kindArgs}).
		GroupBy("t.value")

	rows, err := qb.RunWith(GetDb()).Query()
	if err != nil {
		log.Printf("metrics: failed to query group message counts: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var group string
		var count int64
		if err := rows.Scan(&group, &count); err != nil {
			continue
		}
		groupMessages.With(prometheus.Labels{
			"instance": instLabel,
			"group":    group,
		}).Set(float64(count))
	}
}
