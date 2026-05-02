package zooid

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/prometheus/client_golang/prometheus"
)

var retentionMu sync.Mutex

const retentionDeleteBatchSize = 10000

var (
	retentionDeletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "zooid_retention_deleted_total",
		Help: "Total chat messages deleted by retention policy",
	}, []string{"instance"})

	retentionRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zooid_retention_run_duration_seconds",
		Help:    "Duration of each retention cleanup run",
		Buckets: prometheus.DefBuckets,
	}, []string{"instance"})
)

func init() {
	prometheus.MustRegister(retentionDeletedTotal, retentionRunDuration)
}

// StartRetentionCleaner launches a background goroutine that periodically
// deletes expired chat messages (kinds 9, 10) based on per-group retention
// policies defined in the TOML config. ctx is the service root context;
// when it cancels (SIGTERM), the cleaner exits and any in-flight DELETE
// aborts via the per-batch derived context.
func StartRetentionCleaner(ctx context.Context) {
	go func() {
		cleanExpiredMessages(ctx)

		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanExpiredMessages(ctx)
			}
		}
	}()
}

// activeRetentionInstances tracks which instance labels were seen in the last
// cleanup cycle, so we can clean up metrics for unloaded instances.
var activeRetentionInstances = make(map[string]struct{})

func cleanExpiredMessages(ctx context.Context) {
	retentionMu.Lock()
	defer retentionMu.Unlock()

	instances := GetAllInstances()

	currentInstances := make(map[string]struct{}, len(instances))
	for _, inst := range instances {
		if !inst.Config.Groups.Enabled || !inst.Config.HasRetention() {
			continue
		}

		label := instanceLabel(inst)
		currentInstances[label] = struct{}{}

		start := time.Now()
		var totalDeleted int64

		inst.Groups.metadataCache.Range(func(key, _ any) bool {
			groupID := key.(string)
			retention := inst.Config.GetRetention(groupID)
			if retention <= 0 {
				return true
			}

			cutoff := time.Now().Unix() - int64(retention/time.Second)
			deleted := deleteExpiredGroupMessages(ctx, inst, groupID, cutoff)
			if deleted > 0 {
				totalDeleted += deleted
				log.Printf("retention: deleted %d messages from group %q (instance %s)", deleted, groupID, inst.Config.Schema)
			}
			return true
		})

		retentionRunDuration.With(prometheus.Labels{"instance": label}).Observe(time.Since(start).Seconds())
		if totalDeleted > 0 {
			retentionDeletedTotal.With(prometheus.Labels{"instance": label}).Add(float64(totalDeleted))
		}
	}

	// Clean up metrics for instances that were unloaded or lost retention config.
	for label := range activeRetentionInstances {
		if _, ok := currentInstances[label]; !ok {
			match := prometheus.Labels{"instance": label}
			retentionDeletedTotal.DeletePartialMatch(match)
			retentionRunDuration.DeletePartialMatch(match)
		}
	}
	activeRetentionInstances = currentInstances
}

// deleteOneRetentionBatch runs one bounded DELETE batch. Pulled out so the
// per-iteration ctx can use `defer cancel()` and survive any future early
// returns added inside the batch logic. ctx is the service root passed
// down from the cleaner — derives a per-batch dbOpTimeout from it.
func deleteOneRetentionBatch(ctx context.Context, inst *Instance, groupID string, cutoff int64) (rowsAffected int64, more bool, err error) {
	eventsTable := inst.Events.Schema.Prefix("events")
	tagsTable := inst.Events.Schema.Prefix("event_tags")

	subquery := sb.Select("DISTINCT e.id").
		From(eventsTable + " e").
		Join(tagsTable + " t ON t.event_id = e.id").
		Where(squirrel.Eq{"t.key": "h"}).
		Where(squirrel.Eq{"t.value": groupID}).
		Where(squirrel.Eq{"e.kind": []int{9, 10}}).
		Where(squirrel.Lt{"e.created_at": cutoff}).
		Limit(retentionDeleteBatchSize)

	subSQL, subArgs, err := subquery.ToSql()
	if err != nil {
		return 0, false, fmt.Errorf("build subquery: %w", err)
	}

	// DELETE FROM events WHERE id IN (subquery)
	// CASCADE on event_tags foreign key handles tag cleanup.
	deleteSQL := "DELETE FROM " + eventsTable + " WHERE id IN (" + subSQL + ")"

	subctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	result, err := GetDb().ExecContext(subctx, deleteSQL, subArgs...)
	if err != nil {
		return 0, false, fmt.Errorf("exec delete: %w", err)
	}

	rowsAffected, err = result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("rows affected: %w", err)
	}
	return rowsAffected, rowsAffected >= retentionDeleteBatchSize, nil
}

func deleteExpiredGroupMessages(ctx context.Context, inst *Instance, groupID string, cutoff int64) int64 {
	var totalDeleted int64
	for {
		rows, more, err := deleteOneRetentionBatch(ctx, inst, groupID, cutoff)
		if err != nil {
			log.Printf("retention: %s for group %q", err, groupID)
			return totalDeleted
		}
		totalDeleted += rows
		if !more {
			break
		}
	}
	return totalDeleted
}
