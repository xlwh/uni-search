package admin

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricEntry is one metric in the JSON response.
type metricEntry struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// serveMetricsJSON reads all gathered metrics from the default Prometheus
// registry and returns them as a flat JSON array.
func (s *AdminServer) serveMetricsJSON(w http.ResponseWriter, r *http.Request) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to gather metrics: "+err.Error())
		return
	}

	entries := make([]metricEntry, 0, 128)
	for _, fam := range families {
		name := fam.GetName()
		typ := fam.GetType().String()

		for _, m := range fam.GetMetric() {
			entry := metricEntry{
				Name:  name,
				Type:  typ,
				Value: getMetricValue(m, typ),
			}
			if ls := m.GetLabel(); len(ls) > 0 {
				entry.Labels = make(map[string]string, len(ls))
				for _, l := range ls {
					entry.Labels[l.GetName()] = l.GetValue()
				}
			}
			entries = append(entries, entry)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"metrics": entries,
	})
}

// getMetricValue extracts the numeric value from a Prometheus Metric
// proto, handling counter, gauge, histogram, and summary types.
func getMetricValue(m *dto.Metric, typ string) float64 {
	switch typ {
	case "COUNTER":
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
	case "GAUGE":
		if g := m.GetGauge(); g != nil {
			return g.GetValue()
		}
	case "HISTOGRAM":
		if h := m.GetHistogram(); h != nil {
			return h.GetSampleSum()
		}
	case "SUMMARY":
		if s := m.GetSummary(); s != nil {
			return s.GetSampleSum()
		}
	case "UNTYPED":
		if u := m.GetUntyped(); u != nil {
			return u.GetValue()
		}
	}
	return 0
}

// ensure strings import is used (for future query param parsing)
var _ = strings.TrimSpace
