package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DBQueryDuration measures how long our database queries take.
// We use a label 'operation' to distinguish between 'read_dashboard' and 'refresh_mv'.
var DBQueryDuration = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "db_query_duration_seconds",
		Help: "Duration of database queries in seconds",
		// Buckets tailored for fast reads and potentially slower background refreshes
		Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
	},
	[]string{"operation"},
)
