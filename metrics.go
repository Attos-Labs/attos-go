package attos

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FormatVersion returns the file format version of the live mapping (1 or 2),
// or 0 if no file is loaded.
func (m *Map) FormatVersion() uint32 {
	if mp := m.cur.Load(); mp != nil {
		return mp.version
	}
	return 0
}

// Slots returns the number of slots in the live mapping (0 if none).
func (m *Map) Slots() uint64 {
	if mp := m.cur.Load(); mp != nil {
		if mp.version == 2 {
			return mp.totalKeys
		}
		return mp.slots
	}
	return 0
}

func promEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func promMetric(w io.Writer, name, kind, help string, v any) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, v)
}

// WritePrometheus writes the Prometheus text exposition (version 0.0.4) for the
// map: lookups, misses and hit ratio, hot swaps, file size and version, and a
// sampled lookup-latency histogram (1 in 256 lookups). Everything is read from
// counters the SDK keeps anyway, so scraping adds nothing to the lookup path.
func (m *Map) WritePrometheus(w io.Writer) { m.writeProm(w, "", "") }

func (m *Map) writeProm(w io.Writer, etag, stash string) {
	lookups := m.Lookups()
	misses := min(m.Misses(), lookups)
	promMetric(w, "attos_lookups_total", "counter", "Lookups served.", lookups)
	promMetric(w, "attos_lookup_misses_total", "counter", "Lookups that found no value.", misses)
	ratio := 1.0
	if lookups > 0 {
		ratio = float64(lookups-misses) / float64(lookups)
	}
	promMetric(w, "attos_lookup_hit_ratio", "gauge", "Share of lookups that found a value since process start.", ratio)
	promMetric(w, "attos_hot_swaps_total", "counter", "Completed hot swaps of the live file.", m.Swaps())
	promMetric(w, "attos_file_bytes", "gauge", "Size of the file in use.", m.Size())
	promMetric(w, "attos_file_keys", "gauge", "Slots in the file in use.", m.Slots())
	fmt.Fprintf(w, "# HELP attos_file_info Version of the file in use (format version, ETag, stash).\n# TYPE attos_file_info gauge\nattos_file_info{format_version=\"%d\",etag=\"%s\",stash=\"%s\"} 1\n",
		m.FormatVersion(), promEscape(etag), promEscape(stash))
	fmt.Fprint(w, "# HELP attos_lookup_duration_seconds Lookup latency, sampled 1 in 256 lookups.\n# TYPE attos_lookup_duration_seconds histogram\n")
	var acc uint64
	var sumNs float64
	var prev uint64
	for _, b := range m.LatencyBuckets() {
		acc += b.Count
		sumNs += float64(b.Count) * float64(prev+b.UpperNs) / 2
		prev = b.UpperNs
		fmt.Fprintf(w, "attos_lookup_duration_seconds_bucket{le=\"%g\"} %d\n", float64(b.UpperNs)/1e9, acc)
	}
	fmt.Fprintf(w, "attos_lookup_duration_seconds_bucket{le=\"+Inf\"} %d\nattos_lookup_duration_seconds_sum %g\nattos_lookup_duration_seconds_count %d\n", acc, sumNs/1e9, acc)
}

// WritePrometheus writes the map metrics plus sync state: the ETag and stash
// of the file in use, CRC failures, sync outcomes and telemetry delivery.
func (s *Synchronizer) WritePrometheus(w io.Writer) {
	s.m.writeProm(w, s.ETag(), s.DatasetName())
	promMetric(w, "attos_crc_errors_total", "counter", "Downloaded blobs rejected by the CRC32 check.", s.crcErrors.Load())
	promMetric(w, "attos_signature_errors_total", "counter", "Files rejected because their signature was missing or invalid.", s.sigErrors.Load())
	promMetric(w, "attos_sync_ok_total", "counter", "Sync attempts that succeeded (including not-modified).", s.syncOK.Load())
	promMetric(w, "attos_sync_errors_total", "counter", "Failed sync attempts.", s.syncFailed.Load())
	ts := s.TelemetryStats()
	promMetric(w, "attos_telemetry_reports_sent_total", "counter", "Telemetry reports delivered.", ts.Sent)
	promMetric(w, "attos_telemetry_reports_dropped_total", "counter", "Telemetry reports dropped.", ts.DroppedPending)
}

type promWriter interface{ WritePrometheus(io.Writer) }

// MetricsHandler serves the Prometheus exposition of a *Map or *Synchronizer,
// for example http.Handle("/metrics", attos.MetricsHandler(sync)).
func MetricsHandler(src promWriter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		src.WritePrometheus(w)
	})
}
