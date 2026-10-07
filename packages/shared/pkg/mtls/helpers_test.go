package mtls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/mtls/mtlstest"
)

// Example identities. The trust domain and names are fixtures, not deployments.
const (
	trustDomain = "cluster-a.example.internal"
	serverID    = "spiffe://cluster-a.example.internal/ns/platform/sa/server"
	clientID    = "spiffe://cluster-a.example.internal/ns/platform/sa/client"
	strangerID  = "spiffe://cluster-a.example.internal/ns/platform/sa/stranger"
	otherDomain = "spiffe://cluster-b.example.internal/ns/platform/sa/client"
	serverDNS   = "server.platform.svc.cluster.local"
)

// fixture is one identity: a root, an intermediate, a leaf, its files on
// disk and the Files that loaded them, plus the logger and metric reader
// every component built on the fixture shares.
type fixture struct {
	root    *mtlstest.CA
	inter   *mtlstest.CA
	leaf    *mtlstest.Leaf
	dir     mtlstest.Dir
	files   *Files
	log     logger.Logger
	logs    *observer.ObservedLogs
	metrics *Metrics
	reader  *sdkmetric.ManualReader
}

// newFixture mints serverID under a fresh root and intermediate and loads its
// files with a reload interval long enough that only explicit reloads happen.
func newFixture(t *testing.T, dnsNames ...string) *fixture {
	t.Helper()

	root := mtlstest.NewRootCA(t, "root")
	inter := root.Intermediate(t, "intermediate")
	leaf := inter.Leaf(t, mtlstest.LeafSpec{SPIFFEID: serverID, DNSNames: dnsNames})
	dir := mtlstest.WriteFiles(t, leaf, root.BundlePEM())
	log, logs := testLogger(t)
	metrics, reader := testMetrics(t)
	files := NewFiles(t.Context(),
		FileConfig{CertFile: dir.CertFile, KeyFile: dir.KeyFile, CAFile: dir.CAFile},
		WithReloadInterval(time.Hour), WithFilesLogger(log), WithFilesMetrics(metrics))
	require.NotNil(t, files.Bundle())

	return &fixture{root: root, inter: inter, leaf: leaf, dir: dir, files: files, log: log, logs: logs, metrics: metrics, reader: reader}
}

// peerLeaf mints another identity under the fixture's intermediate, so the
// fixture's roots verify it.
func (fx *fixture) peerLeaf(t *testing.T, id string, dnsNames ...string) *mtlstest.Leaf {
	t.Helper()

	return fx.inter.Leaf(t, mtlstest.LeafSpec{SPIFFEID: id, DNSNames: dnsNames})
}

// testLogger returns a logger whose entries the test can inspect, so tests
// never replace the global logger and can run in parallel.
func testLogger(t *testing.T) (logger.Logger, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zap.DebugLevel)

	return logger.NewTracedLoggerFromCore(core), logs
}

func logMessages(logs *observer.ObservedLogs) []string {
	entries := logs.All()
	messages := make([]string, 0, len(entries))
	for _, entry := range entries {
		messages = append(messages, entry.Message)
	}

	return messages
}

// testMetrics builds the package instruments on a provider the test reads
// directly, so metric assertions never touch the global provider.
func testMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.WithoutCancel(t.Context())) })

	metrics, err := NewMetrics(provider)
	require.NoError(t, err)

	return metrics, reader
}

// pointValue collects and returns the int64 data point of name whose
// attribute set is exactly attrs. Sums and gauges both qualify.
func pointValue(t *testing.T, reader *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) (int64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	want := attribute.NewSet(attrs...)

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					if dp.Attributes.Equals(&want) {
						return dp.Value, true
					}
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					if dp.Attributes.Equals(&want) {
						return dp.Value, true
					}
				}
			}
		}
	}

	return 0, false
}

// mustPoint is pointValue that fails the test when the series is absent.
func mustPoint(t *testing.T, reader *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()

	value, ok := pointValue(t, reader, name, attrs...)
	require.True(t, ok, "metric %s with %v is absent", name, attrs)

	return value
}
