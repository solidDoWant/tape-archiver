package temporalclient

import (
	"io"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
)

// reportInterval bounds how stale SDK metrics can be on a scrape.
const reportInterval = time.Second

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// newMetricsHandler bridges SDK metrics onto reg via tally. The closer must be
// closed before exit to flush pending values.
func newMetricsHandler(reg prometheus.Registerer) (client.MetricsHandler, io.Closer) {
	if reg == nil {
		return client.MetricsNopHandler, noopCloser{}
	}

	reporter := tallyprom.NewReporter(tallyprom.Options{
		Registerer: reg,
		// Histograms aggregate across pods; tally's default summaries do not.
		DefaultTimerType: tallyprom.HistogramTimerType,
	})

	scope, closer := tally.NewRootScope(tally.ScopeOptions{
		CachedReporter:  reporter,
		Separator:       tallyprom.DefaultSeparator,
		SanitizeOptions: &contribtally.PrometheusSanitizeOptions,
	}, reportInterval)

	scope = newPromNamingScope(scope)

	return contribtally.NewMetricsHandler(scope), closer
}

// promNamingScope is contrib/tally's PrometheusNamingScope minus its
// unconditional _seconds suffix on histograms, which would mangle names like
// source_file_size_bytes.
type promNamingScope struct{ tally.Scope }

func newPromNamingScope(scope tally.Scope) tally.Scope { return &promNamingScope{scope} }

func (p *promNamingScope) Counter(name string) tally.Counter {
	if !strings.HasSuffix(name, "_total") {
		name += "_total"
	}

	return p.Scope.Counter(name)
}

func (p *promNamingScope) Timer(name string) tally.Timer {
	if !strings.HasSuffix(name, "_seconds") {
		name += "_seconds"
	}

	return p.Scope.Timer(name)
}

func (p *promNamingScope) Tagged(tags map[string]string) tally.Scope {
	return &promNamingScope{p.Scope.Tagged(tags)}
}

func (p *promNamingScope) SubScope(name string) tally.Scope {
	return &promNamingScope{p.Scope.SubScope(name)}
}
