package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	tableName = "data"
	// dbURLEnvVar is the environment variable name for the database connection string.
	dbURLEnvVar = "CONNECTION_STRING"
	// requiredRecentReadings is the number of recent KPI error readings to consider for the kanary_signal.
	requiredRecentReadings = 3
)

var (
	kanarySignalMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kanary_signal",
			// Help string updated to reflect the new logic: UP if at least one error reading <= 0, DOWN if all > 0.
			Help: fmt.Sprintf("Kanary signal: 1 if at least one of last %d error readings is 0 or less, 0 if all last %d error readings are greater than 0. Only updated when %d valid data points are available.", requiredRecentReadings, requiredRecentReadings, requiredRecentReadings),
		},
		[]string{"cluster", "status"}, // status will be "up" or "down"
	)

	kanaryInterruptionMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kanary_interruption",
			Help: fmt.Sprintf("Binary indicator of an interruption in processing for Kanary signal (1 if interrupted, 0 otherwise). An interruption prevents kanary_signal from being updated. The 'reason' label provides details on the interruption type."),
		},
		[]string{"cluster", "reason"},
	)

	// clusterNameRegex extracts the cluster name from a URL.
	// Expected format: "https://api.<cluster_name>.domain.com:port/"
	clusterNameRegex = regexp.MustCompile(`https:\/\/api\.([a-z0-9-]+)\.[a-z0-9-.]+(:[0-9]+)?\/?`)

	// targetClusters is the list of cluster API URLs to monitor.
	targetClusters = []string{
		// Private Clusters
		"https://api.stone-stage-p01.hpmt.p1.openshiftapps.com:6443/",
		"https://api.stone-prod-p01.wcfb.p1.openshiftapps.com:6443/",
		"https://api.stone-prod-p02.hjvn.p1.openshiftapps.com:6443/",
		// Public Clusters
		"https://api.stone-stg-rh01.l2vh.p1.openshiftapps.com:6443/",
		"https://api.stone-prd-rh01.pg1f.p1.openshiftapps.com:6443/",
	}
)

// extractClusterName extracts the cluster name from a URL string.
func extractClusterName(urlString string) (string, error) {
	matches := clusterNameRegex.FindStringSubmatch(urlString)
	if len(matches) > 1 {
		return matches[1], nil
	}
	return "", fmt.Errorf("URL '%s' does not conform to the expected format 'https://api.<cluster_name>.domain.com:port/'", urlString)
}

// getKpiErrorReadings fetches and validates the last 'requiredRecentReadings' KPI error counts for a given cluster.
// It returns a slice of int64 values if successful, an internal status string for the interruption reason, and an error if any issue occurs.
func getKpiErrorReadings(db *sql.DB, clusterURL string, clusterName string) ([]int64, string, error) {
	// Query fetches KPI error values, filtering by member cluster and optionally by repo_type.
	query := fmt.Sprintf(`
		SELECT
			label_values->>'__results_measurements_KPI_errors' AS kpi_error_value
		FROM
			%s
		WHERE
			label_values->>'.metadata.env.MEMBER_CLUSTER' = $1
			AND ( label_values->>'.repo_type' = 'nodejs-devfile-sample' OR NOT (label_values ? '.repo_type') )
		ORDER BY
			EXTRACT(epoch FROM start) DESC
		LIMIT %d;
	`, tableName, requiredRecentReadings)

	rows, err := db.Query(query, clusterURL)
	if err != nil {
		return nil, "db_error", fmt.Errorf("database query failed for cluster %s: %w", clusterName, err)
	}
	defer rows.Close()

	var parsedErrorReadings []int64
	var rawValuesForLog []string

	for rows.Next() {
		var kpiErrorValueStr sql.NullString
		if err := rows.Scan(&kpiErrorValueStr); err != nil {
			// Error during row scan is considered a database error.
			return nil, "db_error", fmt.Errorf("failed to scan row for cluster %s: %w", clusterName, err)
		}

		if !kpiErrorValueStr.Valid || kpiErrorValueStr.String == "" {
			// NULL or empty values are data processing issues.
			return nil, "data_processing_error", fmt.Errorf("found NULL or empty kpi_error_value in one of the last %d rows for cluster %s. Raw values so far: %v", requiredRecentReadings, clusterName, rawValuesForLog)
		}
		rawValuesForLog = append(rawValuesForLog, kpiErrorValueStr.String)

		kpiErrorCount, err := strconv.ParseInt(kpiErrorValueStr.String, 10, 64)
		if err != nil {
			// Failure to parse the error count is a data processing issue.
			return nil, "data_processing_error", fmt.Errorf("failed to parse kpi_error_value '%s' as integer for cluster %s: %w", kpiErrorValueStr.String, clusterName, err)
		}
		parsedErrorReadings = append(parsedErrorReadings, kpiErrorCount)
	}

	if err := rows.Err(); err != nil {
		// Errors encountered during iteration (e.g., network issues during streaming) are database errors.
		return nil, "db_error", fmt.Errorf("error during row iteration for cluster %s: %w", clusterName, err)
	}

	if len(parsedErrorReadings) < requiredRecentReadings {
		// Not enough data points is considered a database/query issue (could be legitimate if data is sparse, but treated as an error for signal stability).
		return nil, "db_error", fmt.Errorf("expected %d data points for cluster %s, but query returned %d. Raw values: %v", requiredRecentReadings, clusterName, len(parsedErrorReadings), rawValuesForLog)
	}

	return parsedErrorReadings, "data_ok", nil
}

// fetchAndExportMetrics orchestrates fetching data and updating Prometheus metrics for all target clusters.
func fetchAndExportMetrics(db *sql.DB) {
	for _, clusterURL := range targetClusters {
		clusterName, err := extractClusterName(clusterURL)
		reasonForInterruption := ""

		if err != nil {
			log.Printf("CRITICAL: Error extracting cluster name from URL '%s': %v. This URL will be skipped for metrics.", clusterURL, err)
			clusterName = "unknown_cluster_format"
			reasonForInterruption = "data_processing_error"
			kanaryInterruptionMetric.WithLabelValues(clusterName, reasonForInterruption).Set(1)
			continue // Skip to the next cluster.
		}

		kpiErrorReadings, internalStatusMsg, err := getKpiErrorReadings(db, clusterURL, clusterName)

		if err != nil {
			// An interruption occurred (DB error, parse error, insufficient data, etc.).
			reasonForInterruption = internalStatusMsg
			log.Printf("Interruption for cluster '%s' (URL: %s): %s. Error details: %v", clusterName, clusterURL, reasonForInterruption, err)
			kanaryInterruptionMetric.WithLabelValues(clusterName, reasonForInterruption).Set(1)
			// Do NOT update kanarySignalMetric in case of an interruption; it retains its previous value.
		} else {
			// Successfully retrieved and parsed data; no interruption.
			reasonForInterruption = "no_interruption"
			kanaryInterruptionMetric.WithLabelValues(clusterName, reasonForInterruption).Set(0)

			// Determine signal status: UP if at least one error reading is <= 0, DOWN if all are > 0.
			allReadingsStrictlyPositive := true
			if len(kpiErrorReadings) == 0 {
				// This case should ideally be caught by getKpiErrorReadings returning an error.
				// If it somehow occurs, treat as not all readings being strictly positive.
				allReadingsStrictlyPositive = false
			} else {
				for _, errorCount := range kpiErrorReadings {
					if errorCount <= 0 {
						allReadingsStrictlyPositive = false
						break
					}
				}
			}

			if allReadingsStrictlyPositive {
				// All recent error readings are > 0, so the signal is DOWN.
				kanarySignalMetric.WithLabelValues(clusterName, "down").Set(0)
				log.Printf("KO: Kanary signal for cluster '%s' is DOWN (all last %d error readings > 0): %v. Interruption status: %s", clusterName, requiredRecentReadings, kpiErrorReadings, reasonForInterruption)
			} else {
				// At least one recent error reading is <= 0, so the signal is UP.
				kanarySignalMetric.WithLabelValues(clusterName, "up").Set(1)
				log.Printf("OK: Kanary signal for cluster '%s' is UP (at least one of last %d error readings is 0 or less): %v. Interruption status: %s", clusterName, requiredRecentReadings, kpiErrorReadings, reasonForInterruption)
			}
		}
	}
}

func main() {
	databaseURL := os.Getenv(dbURLEnvVar)
	if databaseURL == "" {
		log.Fatalf("FATAL: Environment variable %s is not set or is empty. Example: export %s=\"postgres://user:pass@host:port/db?sslmode=disable\"", dbURLEnvVar, dbURLEnvVar)
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("FATAL: Error connecting to the database using DSN from %s: %v", dbURLEnvVar, err)
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		log.Fatalf("FATAL: Error pinging database: %v", err)
	}
	log.Println("Successfully connected to the database.")

	prometheus.MustRegister(kanarySignalMetric)
	prometheus.MustRegister(kanaryInterruptionMetric)

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Println("Prometheus exporter starting on :8000/metrics ...")
		if err := http.ListenAndServe(":8000", nil); err != nil {
			log.Fatalf("FATAL: Error starting Prometheus HTTP server: %v", err)
		}
	}()

	log.Println("Performing initial metrics fetch...")
	fetchAndExportMetrics(db)
	log.Println("Initial metrics fetch complete.")

	// Periodically fetch metrics. The interval could be made configurable.
	scrapeInterval := 300 * time.Second
	log.Printf("Starting periodic metrics fetch every %v.", scrapeInterval)
	ticker := time.NewTicker(scrapeInterval)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("Fetching and exporting metrics...")
		fetchAndExportMetrics(db)
		log.Println("Metrics fetch complete.")
	}
}