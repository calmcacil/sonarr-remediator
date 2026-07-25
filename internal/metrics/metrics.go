package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ImportsRecovered tracks successful automatic manual imports.
	ImportsRecovered = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sra_imports_recovered_total",
			Help: "Successful automatic manual imports.",
		},
		[]string{"confidence_bucket"},
	)

	// DownloadsRemoved tracks downloads removed by rule.
	DownloadsRemoved = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sra_downloads_removed_total",
			Help: "Downloads removed by rule.",
		},
		[]string{"reason"},
	)

	// RetriesTotal tracks import retries attempted.
	RetriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sra_retries_total",
			Help: "Import retries attempted.",
		},
		[]string{"outcome"},
	)

	// CleanupActionsTotal tracks cleanup actions performed.
	CleanupActionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sra_cleanup_actions_total",
			Help: "Cleanup actions performed.",
		},
		[]string{"action"},
	)

	// DecisionsEvaluated tracks safety rule evaluations.
	DecisionsEvaluated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sra_decisions_evaluated_total",
			Help: "Safety rule evaluations.",
		},
		[]string{"rule", "passed"},
	)

	// QueueItemsObserved is the current queue items count.
	QueueItemsObserved = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sra_queue_items_observed",
			Help: "Current queue items count.",
		},
	)

	// SuggestionsPending is pending review suggestions count.
	SuggestionsPending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sra_suggestions_pending",
			Help: "Pending review suggestions.",
		},
	)

	// SonarrUp is 1 if Sonarr reachable, 0 otherwise.
	SonarrUp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sra_sonarr_up",
			Help: "1 if Sonarr reachable, 0 otherwise.",
		},
	)

	// CycleDuration tracks duration per monitoring cycle.
	CycleDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sra_cycle_duration_seconds",
			Help:    "Duration per monitoring cycle.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"monitor"},
	)
)
