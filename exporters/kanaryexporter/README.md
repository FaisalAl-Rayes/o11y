## Kanary Exporter

This `kanaryexporter` (implemented in `kanaryexporter.go`) is a Prometheus exporter designed to monitor Kanary signal values derived from a PostgreSQL database. It exposes two primary metrics: `kanary_signal` and `kanary_interruption`.

### `kanary_signal` Metric

The `kanary_signal` metric indicates the health status of a target cluster based on recent error readings from load tests. It is a `prometheus.GaugeVec` with labels `cluster` (the name of the monitored cluster) and `status` ("up" or "down").

**How `kanary_signal` Works:**

* The exporter accepts the PostgreSQL database connection URL via the `CONNECTION_STRING` environment variable.
* Upon starting, it connects to the specified database.
* Periodically (every 5 minutes by default), it iterates through a predefined list of `targetClusters`.
* For each cluster, it constructs and executes a SQL query against the database. This query retrieves the last `requiredRecentReadings` (defaulting to 3) of `__results_measurements_KPI_errors` for entries matching `.repo_type = 'nodejs-devfile-sample'` or where `.repo_type` is not present.
* The retrieved KPI error counts are then processed:
    * If **all** of the last `requiredRecentReadings` error counts are strictly greater than 0, the `kanary_signal` for that cluster is set to `0.0` (status: "down").
    * If **at least one** of the last `requiredRecentReadings` error counts is 0 (or less), the `kanary_signal` for that cluster is set to `1.0` (status: "up").
* The `kanary_signal` is only updated if there are no interruptions during data fetching and processing for that cluster.

### `kanary_interruption` Metric

The `kanary_interruption` metric is a `prometheus.GaugeVec` with labels `cluster` and `reason`. It acts as a binary indicator (0 or 1) to signal issues encountered during the data fetching or processing pipeline for the `kanary_signal`.

**How `kanary_interruption` Works:**

* **Value `1` (Interrupted):**
    * Set if the cluster URL extraction from the `targetClusters` list fails.
        * `cluster` label: `"unknown_cluster_format"`
        * `reason` label: `"data_processing_error"`
    * Set if `getKpiErrorReadings` encounters an issue fetching or processing data for a specific cluster. The `reason` label will indicate the type of error:
        * `"db_error"`: If there's a database query failure, an error scanning rows, an error during row iteration, or if an insufficient number of data points (less than `requiredRecentReadings`) are returned.
        * `"data_processing_error"`: If a retrieved `kpi_error_value` is NULL, an empty string, or fails to parse as an integer.
* **Value `0` (Not Interrupted):**
    * Set if data for `requiredRecentReadings` is successfully fetched and parsed for a cluster.
        * `reason` label: `"no_interruption"`
* **Impact:** When `kanary_interruption` is `1` for a given cluster, the `kanary_signal` for that cluster is **not updated** and retains its previous value. This prevents stale or incorrect signals from being reported when the underlying data is unreliable.

**General Exporter Behavior:**

* The exporter uses globally defined `prometheus.GaugeVec` instances (`kanarySignalMetric`, `kanaryInterruptionMetric`) to store these values. These metrics are registered with the default Prometheus registry.
* Metrics are exposed via the standard `promhttp.Handler()` on the `/metrics` endpoint (port 8000 by default).
* The exporter does not implement a custom `prometheus.Collector` interface directly but relies on the standard Prometheus client library mechanisms.

The test case file (`kanaryexporter_test.go`) provides comprehensive unit and integration tests for the exporter's functionality. This includes testing the cluster name extraction, the core logic of fetching and processing data from a mocked database, metric value transformations, and the correct exposure of metrics via an HTTP test server.

This exporter serves as an example of how to create a Go application that polls an external data source (a database, in this case), processes the data to derive meaningful signals, and exposes these as Prometheus metrics. 

The o11y team provides this Kanary exporter and its configuration as a reference:

* [Kanary Exporter code](https://github.com/redhat-appstudio/o11y/tree/main/exporters/kanaryexporter.go)
* [Kanary Exporter and Service Monitor Kubernetes Resources](https://github.com/redhat-appstudio/o11y/tree/main/config/exporters/monitoring/kanary/base)