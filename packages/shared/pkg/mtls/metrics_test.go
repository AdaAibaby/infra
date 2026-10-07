package mtls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"

	"github.com/e2b-dev/infra/packages/shared/pkg/mtls/mtlstest"
)

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)

	return hex.EncodeToString(sum[:])
}

func TestMetricsReportReloadsExpiryAndRoots(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, serverDNS)
	assert.Equal(t, int64(1), mustPoint(t, fx.reader, MetricReloads, attribute.String(AttrOutcome, ReloadLoaded)))
	assert.Equal(t, fx.leaf.Cert.NotAfter.Unix(), mustPoint(t, fx.reader, MetricNotAfter, attribute.String(AttrKind, KindLeaf)))
	assert.Equal(t, fx.leaf.Cert.NotAfter.Unix(), mustPoint(t, fx.reader, MetricNotAfter, attribute.String(AttrKind, KindChain)), "the leaf expires before its intermediate")
	assert.Equal(t, int64(1), mustPoint(t, fx.reader, MetricRootsLoaded, attribute.String(AttrFingerprint, fingerprint(fx.root.Cert.Raw))))

	require.NoError(t, fx.files.Reload(t.Context()))
	assert.Equal(t, int64(1), mustPoint(t, fx.reader, MetricReloads, attribute.String(AttrOutcome, ReloadUnchanged)))

	// corruptPEMBlock and concat come from files_test.go.
	mtlstest.Write(t, fx.dir.CAFile, concat(fx.root.PEM, []byte(corruptPEMBlock)))
	require.Error(t, fx.files.Reload(t.Context()))
	assert.Equal(t, int64(1), mustPoint(t, fx.reader, MetricReloads, attribute.String(AttrOutcome, ReloadFailed)))

	replacement := newFixture(t, serverDNS)
	fx.dir.Rewrite(t, fx.leaf, replacement.root.BundlePEM())
	require.NoError(t, fx.files.Reload(t.Context()))
	assert.Equal(t, int64(0), mustPoint(t, fx.reader, MetricRootsLoaded, attribute.String(AttrFingerprint, fingerprint(fx.root.Cert.Raw))), "a root that left the bundle reads zero")
	assert.Equal(t, int64(1), mustPoint(t, fx.reader, MetricRootsLoaded, attribute.String(AttrFingerprint, fingerprint(replacement.root.Cert.Raw))))
	assert.Equal(t, int64(2), mustPoint(t, fx.reader, MetricReloads, attribute.String(AttrOutcome, ReloadLoaded)))
}

func TestNilMetricsRecordNothing(t *testing.T) {
	t.Parallel()

	var m *Metrics
	m.handshake(t.Context(), "l", OutcomeTLS)
	m.clientHandshake(t.Context(), "h", OutcomeTLS)
	m.refused(t.Context(), "l", ReasonNotAllowed, KindGRPC)
	m.wouldReject(t.Context(), "l", ReasonNotAllowed, KindGRPC)
	m.plaintextRequest(t.Context(), "l", KindHTTP)
	m.plaintextConn(t.Context(), "l", 1)
	m.connection(t.Context(), "l", clientID)
	m.recordMode(t.Context(), "l", ModeOff, SourceFallback)
	m.recordClientMode(t.Context(), "h", ClientOff, SourceFallback)
	m.reload(t.Context(), ReloadLoaded)
	m.bundleLoaded(t.Context(), nil, nil)
}

// slowGauge is an Int64Gauge whose first write stalls, so a second flip of
// the same key can run while the first is still writing.
type slowGauge struct {
	embedded.Int64Gauge

	mu      sync.Mutex
	writes  int
	reading map[attribute.Distinct]int64
}

func (g *slowGauge) Record(_ context.Context, value int64, opts ...metric.RecordOption) {
	g.mu.Lock()
	g.writes++
	first := g.writes == 1
	g.mu.Unlock()
	if first {
		time.Sleep(50 * time.Millisecond)
	}

	set := metric.NewRecordConfig(opts).Attributes()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reading[set.Equivalent()] = value
}

func (g *slowGauge) Enabled(context.Context) bool { return true }

func TestMetricsFlipLeavesOneSeriesReadingOne(t *testing.T) {
	t.Parallel()

	m := &Metrics{lastMode: map[string]attribute.Set{}}
	gauge := &slowGauge{reading: map[attribute.Distinct]int64{}}
	off := attribute.NewSet(attribute.String(AttrListener, "edge"), attribute.String(AttrMode, ModeOff.String()))
	required := attribute.NewSet(attribute.String(AttrListener, "edge"), attribute.String(AttrMode, ModeRequired.String()))

	var wg sync.WaitGroup
	wg.Go(func() { m.flip(t.Context(), gauge, m.lastMode, "edge", off) })
	time.Sleep(10 * time.Millisecond) // the first flip is now inside its stalled write
	wg.Go(func() { m.flip(t.Context(), gauge, m.lastMode, "edge", required) })
	wg.Wait()

	ones := 0
	for _, v := range gauge.reading {
		if v == 1 {
			ones++
		}
	}
	assert.Equal(t, 1, ones, "one series reads 1")
	last := m.lastMode["edge"]
	assert.Equal(t, int64(1), gauge.reading[last.Equivalent()], "and it is the mode recorded last")
	assert.Equal(t, 3, gauge.writes, "a repeat of the mode in force records nothing")
	m.flip(t.Context(), gauge, m.lastMode, "edge", required)
	assert.Equal(t, 3, gauge.writes)
}
