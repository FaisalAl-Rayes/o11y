package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractClusterName tests the cluster name extraction logic.
func TestExtractClusterName(t *testing.T) {
	tests := []struct {
		name      string
		urlString string
		wantName  string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "valid private cluster URL with port",
			urlString: "https://api.stone-stage-p01.hpmt.p1.openshiftapps.com:6443/",
			wantName:  "stone-stage-p01",
			wantErr:   false,
		},
		{
			name:      "valid public cluster URL with port",
			urlString: "https://api.stone-stg-rh01.l2vh.p1.openshiftapps.com:6443/",
			wantName:  "stone-stg-rh01",
			wantErr:   false,
		},
		{
			name:      "valid cluster URL without port",
			urlString: "https://api.my-cluster.example.com/",
			wantName:  "my-cluster",
			wantErr:   false,
		},
		{
			name:      "URL with hyphens in cluster name",
			urlString: "https://api.my-super-long-cluster-name.sub.domain.co.uk:1234/",
			wantName:  "my-super-long-cluster-name",
			wantErr:   false,
		},
		{
			name:      "invalid URL - no 'api.' prefix",
			urlString: "https://stone-stage-p01.hpmt.p1.openshiftapps.com:6443/",
			wantName:  "",
			wantErr:   true,
			errSubstr: "does not conform to the expected format",
		},
		{
			name:      "invalid URL - http instead of https",
			urlString: "http://api.stone-stage-p01.hpmt.p1.openshiftapps.com:6443/",
			wantName:  "",
			wantErr:   true,
			errSubstr: "does not conform to the expected format",
		},
		{
			name:      "invalid URL - malformed",
			urlString: "://api.cluster.com",
			wantName:  "",
			wantErr:   true,
			errSubstr: "does not conform to the expected format",
		},
		{
			name:      "empty URL string",
			urlString: "",
			wantName:  "",
			wantErr:   true,
			errSubstr: "does not conform to the expected format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, err := extractClusterName(tt.urlString)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.Contains(t, err.Error(), tt.errSubstr, "Error message mismatch")
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantName, gotName, "Extracted cluster name mismatch")
			}
		})
	}
}

// TestGetKpiErrorReadings tests the logic of fetching and parsing KPI error readings.
func TestGetKpiErrorReadings(t *testing.T) {
	queryRegexPattern := fmt.Sprintf(`^\s*SELECT\s+label_values->>'__results_measurements_KPI_errors' AS kpi_error_value\s+FROM\s+%s\s+WHERE\s+label_values->>'\.metadata\.env\.MEMBER_CLUSTER' = \$1\s+AND\s+\(\s*label_values->>'\.repo_type'\s*=\s*'nodejs-devfile-sample'\s+OR\s+NOT\s*\(label_values\s*\?\s*'\.repo_type'\)\s*\)\s+ORDER BY\s+EXTRACT\(epoch FROM start\) DESC\s+LIMIT %d;\s*$`, regexp.QuoteMeta(tableName), requiredRecentReadings)
	clusterURL := "https://api.test-cluster.example.com:6443/"
	clusterName := "test-cluster"

	tests := []struct {
		name                  string
		mockSetup             func(mock sqlmock.Sqlmock)
		expectedErrorReadings []int64
		expectedStatusMsg     string
		expectError           bool
		errSubstr             string
	}{
		{
			name: "success - 3 valid error readings, all zero",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).
					AddRow("0").AddRow("0").AddRow("0")
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedErrorReadings: []int64{0, 0, 0},
			expectedStatusMsg:     "data_ok",
			expectError:           false,
		},
		{
			name: "success - 3 valid error readings, some non-zero",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).
					AddRow("0").AddRow("1").AddRow("5")
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedErrorReadings: []int64{0, 1, 5},
			expectedStatusMsg:     "data_ok",
			expectError:           false,
		},
		{
			name: "db query error",
			mockSetup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnError(fmt.Errorf("db connection lost"))
			},
			expectedStatusMsg: "db_error",
			expectError:       true,
			errSubstr:         "database query failed",
		},
		{
			name: "row iteration error",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).
					AddRow("0").
					RowError(0, fmt.Errorf("iteration scan issue"))
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedStatusMsg: "db_error",
			expectError:       true,
			errSubstr:         "error during row iteration for cluster test-cluster: iteration scan issue",
		},
		{
			name: "parse error for a value",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).
					AddRow("0").AddRow("not-an-integer").AddRow("2")
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedStatusMsg: "data_processing_error",
			expectError:       true,
			errSubstr:         "failed to parse kpi_error_value 'not-an-integer' as integer",
		},
		{
			name: "null value for an error reading",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).
					AddRow("0").AddRow(sql.NullString{Valid: false}).AddRow("2")
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedStatusMsg: "data_processing_error",
			expectError:       true,
			errSubstr:         "found NULL or empty kpi_error_value",
		},
		{
			name: "empty string for an error reading",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).
					AddRow("").AddRow("1").AddRow("0")
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedStatusMsg: "data_processing_error",
			expectError:       true,
			errSubstr:         "found NULL or empty kpi_error_value",
		},
		{
			name: "insufficient data - less than required rows",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow("0")
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedStatusMsg: "db_error",
			expectError:       true,
			errSubstr:         fmt.Sprintf("expected %d data points", requiredRecentReadings),
		},
		{
			name: "insufficient data - no rows",
			mockSetup: func(mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"})
				mock.ExpectQuery(queryRegexPattern).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedStatusMsg: "db_error",
			expectError:       true,
			errSubstr:         fmt.Sprintf("expected %d data points", requiredRecentReadings),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			require.NoError(t, err)
			defer db.Close()

			tt.mockSetup(mock)

			errorReadings, statusMsg, err := getKpiErrorReadings(db, clusterURL, clusterName)

			assert.Equal(t, tt.expectedStatusMsg, statusMsg)
			if tt.expectError {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.Contains(t, err.Error(), tt.errSubstr)
				}
				assert.Nil(t, errorReadings)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedErrorReadings, errorReadings)
			}
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// TestFetchAndExportMetrics tests the core logic of fetching data and setting Prometheus metrics.
func TestFetchAndExportMetrics(t *testing.T) {
	originalGlobalTargetClusters := targetClusters
	defer func() {
		targetClusters = originalGlobalTargetClusters
		kanarySignalMetric.Reset()
		kanaryInterruptionMetric.Reset()
	}()

	queryRegexPattern := fmt.Sprintf(
		`^\s*SELECT\s+label_values->>'__results_measurements_KPI_errors' AS kpi_error_value\s+FROM\s+%s\s+WHERE\s+label_values->>'\.metadata\.env\.MEMBER_CLUSTER' = \$1\s+AND\s+\(\s*label_values->>'\.repo_type'\s*=\s*'nodejs-devfile-sample'\s+OR\s+NOT\s*\(label_values\s*\?\s*'\.repo_type'\)\s*\)\s+ORDER BY\s+EXTRACT\(epoch FROM start\) DESC\s+LIMIT %d;\s*$`, regexp.QuoteMeta(tableName), requiredRecentReadings)

	type expectedSignal struct {
		cluster     string
		status      string
		value       float64
		shouldBeSet bool
	}
	type expectedInterruption struct {
		cluster string
		reason  string
		value   float64
	}

	tests := []struct {
		name                  string
		testTargetClusters    []string
		mockSetup             func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string)
		expectedSignals       []expectedSignal
		expectedInterruptions []expectedInterruption
	}{
		{
			name:               "single cluster, all error readings zero, signal UP",
			testTargetClusters: []string{"https://api.cluster-up-all-zero.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow("0").AddRow("0")
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-up-all-zero", status: "up", value: 1.0, shouldBeSet: true}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-up-all-zero", reason: "no_interruption", value: 0.0}},
		},
		{
			name:               "single cluster, at least one error reading zero, signal UP",
			testTargetClusters: []string{"https://api.cluster-up-one-zero.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("1").AddRow("0").AddRow("2") // Middle is 0
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-up-one-zero", status: "up", value: 1.0, shouldBeSet: true}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-up-one-zero", reason: "no_interruption", value: 0.0}},
		},
		{
			name:               "single cluster, all error readings > 0, signal DOWN",
			testTargetClusters: []string{"https://api.cluster-down-all-positive.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("1").AddRow("2").AddRow("3") // All > 0
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-down-all-positive", status: "down", value: 0.0, shouldBeSet: true}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-down-all-positive", reason: "no_interruption", value: 0.0}},
		},
		{
			name:               "single cluster, DB query error, signal NOT updated, interruption YES",
			testTargetClusters: []string{"https://api.cluster-db-error.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnError(fmt.Errorf("db connection failed"))
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-db-error", status: "up", value: 0.0, shouldBeSet: false}, {cluster: "cluster-db-error", status: "down", value: 0.0, shouldBeSet: false}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-db-error", reason: "db_error", value: 1.0}},
		},
		{
			name:               "single cluster, parse error, signal NOT updated, interruption YES",
			testTargetClusters: []string{"https://api.cluster-parse-error.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow("not-an-integer").AddRow("0")
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-parse-error", status: "up", value: 0.0, shouldBeSet: false}, {cluster: "cluster-parse-error", status: "down", value: 0.0, shouldBeSet: false}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-parse-error", reason: "data_processing_error", value: 1.0}},
		},
		{
			name:               "single cluster, insufficient data (2 rows), signal NOT updated, interruption YES",
			testTargetClusters: []string{"https://api.cluster-insufficient.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow("0")
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-insufficient", status: "up", value: 0.0, shouldBeSet: false}, {cluster: "cluster-insufficient", status: "down", value: 0.0, shouldBeSet: false}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-insufficient", reason: "db_error", value: 1.0}},
		},
		{
			name:               "single cluster, insufficient data (NULL value), signal NOT updated, interruption YES",
			testTargetClusters: []string{"https://api.cluster-null.example.com:6443/"},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow(sql.NullString{Valid: false}).AddRow("0")
				mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
			},
			expectedSignals:       []expectedSignal{{cluster: "cluster-null", status: "up", value: 0.0, shouldBeSet: false}, {cluster: "cluster-null", status: "down", value: 0.0, shouldBeSet: false}},
			expectedInterruptions: []expectedInterruption{{cluster: "cluster-null", reason: "data_processing_error", value: 1.0}},
		},
		{
			name:                  "invalid cluster URL format, interruption YES for unknown_cluster_format",
			testTargetClusters:    []string{"invalid-url-format"},
			mockSetup:             func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) { /* No DB interaction */ },
			expectedSignals:       []expectedSignal{},
			expectedInterruptions: []expectedInterruption{{cluster: "unknown_cluster_format", reason: "data_processing_error", value: 1.0}},
		},
		{
			name: "multiple clusters with mixed results",
			testTargetClusters: []string{
				"https://api.cluster-mix-up-allzero.example.com:6443/",    // UP (all 0)
				"https://api.cluster-mix-up-onezero.example.com:6443/",    // UP (one 0)
				"https://api.cluster-mix-down-allpos.example.com:6443/", // DOWN (all >0)
				"https://api.cluster-mix-db-error.example.com:6443/",      // DB Error
				"invalid-url",                                               // Invalid URL
				"https://api.cluster-mix-parse-error.example.com:6443/",   // Parse Error
			},
			mockSetup: func(mock sqlmock.Sqlmock, clusterURL string, clusterName string, queryRegex string) {
				switch clusterName {
				case "cluster-mix-up-allzero":
					rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow("0").AddRow("0")
					mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
				case "cluster-mix-up-onezero":
					rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("1").AddRow("0").AddRow("2")
					mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
				case "cluster-mix-down-allpos":
					rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("1").AddRow("2").AddRow("3")
					mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
				case "cluster-mix-db-error":
					mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnError(fmt.Errorf("specific db error for mix"))
				case "cluster-mix-parse-error":
					rows := sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("not-an-int")
					mock.ExpectQuery(queryRegex).WithArgs(clusterURL).WillReturnRows(rows)
				}
			},
			expectedSignals: []expectedSignal{
				{cluster: "cluster-mix-up-allzero", status: "up", value: 1.0, shouldBeSet: true},
				{cluster: "cluster-mix-up-onezero", status: "up", value: 1.0, shouldBeSet: true},
				{cluster: "cluster-mix-down-allpos", status: "down", value: 0.0, shouldBeSet: true},
				{cluster: "cluster-mix-db-error", status: "up", value: 0.0, shouldBeSet: false},
				{cluster: "cluster-mix-db-error", status: "down", value: 0.0, shouldBeSet: false},
				{cluster: "cluster-mix-parse-error", status: "up", value: 0.0, shouldBeSet: false},
				{cluster: "cluster-mix-parse-error", status: "down", value: 0.0, shouldBeSet: false},
			},
			expectedInterruptions: []expectedInterruption{
				{cluster: "cluster-mix-up-allzero", reason: "no_interruption", value: 0.0},
				{cluster: "cluster-mix-up-onezero", reason: "no_interruption", value: 0.0},
				{cluster: "cluster-mix-down-allpos", reason: "no_interruption", value: 0.0},
				{cluster: "cluster-mix-db-error", reason: "db_error", value: 1.0},
				{cluster: "unknown_cluster_format", reason: "data_processing_error", value: 1.0},
				{cluster: "cluster-mix-parse-error", reason: "data_processing_error", value: 1.0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			require.NoError(t, err)
			defer db.Close()

			targetClusters = tt.testTargetClusters
			kanarySignalMetric.Reset()
			kanaryInterruptionMetric.Reset()

			for _, currentClusterURL := range tt.testTargetClusters {
				extractedClusterName, extractErr := extractClusterName(currentClusterURL)
				if extractErr == nil {
					tt.mockSetup(mock, currentClusterURL, extractedClusterName, queryRegexPattern)
				}
			}

			fetchAndExportMetrics(db)

			for _, expected := range tt.expectedSignals {
				actualVal := testutil.ToFloat64(kanarySignalMetric.WithLabelValues(expected.cluster, expected.status))
				if expected.shouldBeSet {
					assert.Equal(t, expected.value, actualVal, "Signal metric mismatch for cluster %s, status %s", expected.cluster, expected.status)
				} else {
					assert.Equal(t, 0.0, actualVal, "Signal metric for cluster %s, status %s should be 0 (not updated or reset), but was %f", expected.cluster, expected.status, actualVal)
				}
			}

			for _, expected := range tt.expectedInterruptions {
				actualVal := testutil.ToFloat64(kanaryInterruptionMetric.WithLabelValues(expected.cluster, expected.reason))
				assert.Equal(t, expected.value, actualVal, "Interruption metric value mismatch for cluster %s, reason %s", expected.cluster, expected.reason)
			}

			assert.NoError(t, mock.ExpectationsWereMet(), "SQL mock expectations not met")
		})
	}
}

// TestMetricsEndpoint verifies the full exposure of metrics via HTTP.
func TestMetricsEndpoint(t *testing.T) {
	originalGlobalTargetClusters := targetClusters
	originalRegistry := prometheus.DefaultRegisterer
	testRegistry := prometheus.NewRegistry()

	correctSignalHelp := fmt.Sprintf("Kanary signal: 1 if at least one of last %d error readings is 0 or less, 0 if all last %d error readings are greater than 0. Only updated when %d valid data points are available.", requiredRecentReadings, requiredRecentReadings, requiredRecentReadings)
	correctInterruptionHelp := fmt.Sprintf("Binary indicator of an interruption in processing for Kanary signal (1 if interrupted, 0 otherwise). An interruption prevents kanary_signal from being updated. The 'reason' label provides details on the interruption type.")

	localKanarySignalMetric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "kanary_signal", Help: correctSignalHelp},
		[]string{"cluster", "status"},
	)
	localKanaryInterruptionMetric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "kanary_interruption", Help: correctInterruptionHelp},
		[]string{"cluster", "reason"},
	)

	originalSignalMetric := kanarySignalMetric
	originalInterruptionMetric := kanaryInterruptionMetric
	kanarySignalMetric = localKanarySignalMetric
	kanaryInterruptionMetric = localKanaryInterruptionMetric

	testRegistry.MustRegister(kanarySignalMetric)
	testRegistry.MustRegister(kanaryInterruptionMetric)

	defer func() {
		targetClusters = originalGlobalTargetClusters
		prometheus.DefaultRegisterer = originalRegistry
		kanarySignalMetric = originalSignalMetric
		kanaryInterruptionMetric = originalInterruptionMetric
	}()

	queryRegexPattern := fmt.Sprintf(`^\s*SELECT\s+label_values->>'__results_measurements_KPI_errors' AS kpi_error_value\s+FROM\s+%s\s+WHERE\s+label_values->>'\.metadata\.env\.MEMBER_CLUSTER' = \$1\s+AND\s+\(\s*label_values->>'\.repo_type'\s*=\s*'nodejs-devfile-sample'\s+OR\s+NOT\s*\(label_values\s*\?\s*'\.repo_type'\)\s*\)\s+ORDER BY\s+EXTRACT\(epoch FROM start\) DESC\s+LIMIT %d;\s*$`, regexp.QuoteMeta(tableName), requiredRecentReadings)

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	targetClusters = []string{
		"https://api.endpoint-up-allzero.example.com:6443/",      // UP (all 0)
		"https://api.endpoint-up-onezero.example.com:6443/",      // UP (one 0)
		"https://api.endpoint-down-allpos.example.com:6443/", // DOWN (all >0)
		"invalid-for-endpoint",                                   // Results in data_processing_error interruption
		"https://api.endpoint-dberror.example.com:6443/",     // Results in db_error interruption
	}

	mock.ExpectQuery(queryRegexPattern).
		WithArgs("https://api.endpoint-up-allzero.example.com:6443/").
		WillReturnRows(sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("0").AddRow("0").AddRow("0"))

	mock.ExpectQuery(queryRegexPattern).
		WithArgs("https://api.endpoint-up-onezero.example.com:6443/").
		WillReturnRows(sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("1").AddRow("0").AddRow("2"))

	mock.ExpectQuery(queryRegexPattern).
		WithArgs("https://api.endpoint-down-allpos.example.com:6443/").
		WillReturnRows(sqlmock.NewRows([]string{"kpi_error_value"}).AddRow("1").AddRow("2").AddRow("3"))

	mock.ExpectQuery(queryRegexPattern).
		WithArgs("https://api.endpoint-dberror.example.com:6443/").
		WillReturnError(sql.ErrNoRows)

	kanarySignalMetric.Reset()
	kanaryInterruptionMetric.Reset()
	fetchAndExportMetrics(db)

	handler := promhttp.HandlerFor(testRegistry, promhttp.HandlerOpts{})
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	expectedSignalMetrics := fmt.Sprintf(`
		# HELP kanary_signal %s
		# TYPE kanary_signal gauge
		kanary_signal{cluster="endpoint-down-allpos",status="down"} 0
		kanary_signal{cluster="endpoint-up-allzero",status="up"} 1
		kanary_signal{cluster="endpoint-up-onezero",status="up"} 1
	`, correctSignalHelp)
	err = testutil.CollectAndCompare(kanarySignalMetric, strings.NewReader(expectedSignalMetrics), "kanary_signal")
	assert.NoError(t, err, "kanary_signal metrics output mismatch")

	expectedInterruptionMetrics := fmt.Sprintf(`
		# HELP kanary_interruption %s
		# TYPE kanary_interruption gauge
		kanary_interruption{cluster="endpoint-dberror",reason="db_error"} 1
		kanary_interruption{cluster="endpoint-down-allpos",reason="no_interruption"} 0
		kanary_interruption{cluster="endpoint-up-allzero",reason="no_interruption"} 0
		kanary_interruption{cluster="endpoint-up-onezero",reason="no_interruption"} 0
		kanary_interruption{cluster="unknown_cluster_format",reason="data_processing_error"} 1
	`, correctInterruptionHelp)
	err = testutil.CollectAndCompare(kanaryInterruptionMetric, strings.NewReader(expectedInterruptionMetrics), "kanary_interruption")
	assert.NoError(t, err, "kanary_interruption metrics output mismatch")

	assert.NoError(t, mock.ExpectationsWereMet(), "SQL mock expectations not met in TestMetricsEndpoint")
}
