package zooid

import (
	"log"
	"time"

	"fiatjaf.com/nostr"
	"github.com/prometheus/client_golang/prometheus"
)

const metricsInstance = "g-relay"

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

	groupMembersTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_group_members_total",
		Help: "Total members across all groups",
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
		Help: "Total events in database",
	}, []string{"instance"})

	messagesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zooid_messages_total",
		Help: "Total chat messages in database",
	}, []string{"instance"})

	QueryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zooid_query_duration_seconds",
		Help:    "Duration of database queries",
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

const maxTrackedGroups = 1000

// StartMetricsCollector launches a background goroutine that updates
// Prometheus metrics every 30 seconds.
func StartMetricsCollector() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		// Collect once immediately at startup
		collectMetrics()

		for range ticker.C {
			collectMetrics()
		}
	}()
}

func collectMetrics() {
	instances := GetAllInstances()

	for _, inst := range instances {
		collectCacheMetrics(inst)
		collectDBMetrics(inst)
	}
}

func collectCacheMetrics(inst *Instance) {
	label := prometheus.Labels{"instance": metricsInstance}

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

	// Per-group member counts from membershipCache
	// Reset per-group gauges to avoid stale entries from deleted groups
	groupMembers.Reset()

	var totalMembers float64
	var tracked int
	inst.Groups.membershipCache.Range(func(key, value any) bool {
		ms := value.(*memberSet)
		ms.mu.RLock()
		count := float64(len(ms.members))
		ms.mu.RUnlock()

		totalMembers += count

		if tracked < maxTrackedGroups {
			h := key.(string)
			groupMembers.With(prometheus.Labels{
				"instance": metricsInstance,
				"group":    h,
			}).Set(count)
			tracked++
		}
		return true
	})

	groupMembersTotal.With(label).Set(totalMembers)
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
	label := prometheus.Labels{"instance": metricsInstance}

	// Total events
	count, err := inst.Events.CountEvents(nostr.Filter{})
	if err != nil {
		log.Printf("metrics: failed to count events: %v", err)
	} else {
		eventsTotal.With(label).Set(float64(count))
	}

	// Total chat messages
	msgCount, err := inst.Events.CountEvents(nostr.Filter{Kinds: chatKinds})
	if err != nil {
		log.Printf("metrics: failed to count messages: %v", err)
	} else {
		messagesTotal.With(label).Set(float64(msgCount))
	}
}
