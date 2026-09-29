//go:build linux

package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
)

func TestSandboxMemory(t *testing.T) {
	t.Parallel()

	m := &sandbox.Metrics{MemTotal: 4 << 30, MemUsed: 1 << 30}

	tests := []struct {
		name         string
		envdVersion  string
		wantTotal    int64
		wantUsed     int64
		wantReported bool
		wantErr      bool
	}{
		{name: "first envd reporting bytes", envdVersion: "0.2.4", wantTotal: 4 << 30, wantUsed: 1 << 30, wantReported: true},
		{name: "current envd", envdVersion: "0.9.0", wantTotal: 4 << 30, wantUsed: 1 << 30, wantReported: true},
		{name: "envd reporting only MiB", envdVersion: "0.2.3"},
		{name: "unparsable version", envdVersion: "not-a-version", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			total, used, reported, err := sandboxMemory(tt.envdVersion, m)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tt.wantReported, reported)
			assert.Equal(t, tt.wantTotal, total)
			assert.Equal(t, tt.wantUsed, used)
		})
	}
}
