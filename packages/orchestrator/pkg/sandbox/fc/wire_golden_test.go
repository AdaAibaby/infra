//go:build linux

package fc

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc/cputemplate"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// wireRecorder is a Firecracker API stub that answers every request with 204
// and keeps the body it received, keyed by method and path.
type wireRecorder struct {
	mu     sync.Mutex
	bodies map[string]string
}

func newWireRecorder(t *testing.T) (*apiClient, *wireRecorder) {
	t.Helper()

	dir, err := os.MkdirTemp("", "fc") //nolint:usetesting // t.TempDir embeds the test name and overflows the unix sun_path limit
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := dir + "/fc.sock"
	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)

	rec := &wireRecorder{bodies: map[string]string{}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // no timeouts needed for a stub on a private socket
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies[r.Method+" "+r.URL.Path] = string(body)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	return newApiClient(socket), rec
}

func (r *wireRecorder) body(t *testing.T, key string) string {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	body, ok := r.bodies[key]
	require.True(t, ok, "no request %q; got %v", key, r.keysLocked())

	return body
}

func (r *wireRecorder) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.keysLocked()
}

func (r *wireRecorder) keysLocked() []string {
	keys := make([]string, 0, len(r.bodies))
	for k := range r.bodies {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return keys
}

// stubRootfs serves one path; nothing else of the provider is reached.
type stubRootfs struct {
	rootfs.Provider

	path string
}

func (s stubRootfs) Path() (string, error) { return s.path, nil }

// goldenProcess is a Process on the stub socket with the slot, paths and
// provider Create reads, and no Firecracker behind it.
func goldenProcess(t *testing.T, versions Config, slotIdx int) (*Process, *wireRecorder, *network.Slot) {
	t.Helper()

	client, rec := newWireRecorder(t)
	slot, err := network.NewSlot("golden", slotIdx, network.Config{}, network.NoopEgressProxy{})
	require.NoError(t, err)

	cacheDir := t.TempDir()
	files := (storage.CachePaths{}).NewSandboxFiles("sbx-golden")
	config := cfg.BuilderConfig{StorageConfig: storage.Config{SandboxCacheDir: cacheDir}}
	require.NoError(t, os.MkdirAll(filepath.Dir(files.SandboxCacheRootfsLinkPath(config.StorageConfig)), 0o755))

	return &Process{
		Versions:       versions,
		client:         client,
		slot:           slot,
		files:          files,
		config:         config,
		rootfsProvider: stubRootfs{path: filepath.Join(cacheDir, "rootfs.ext4")},
		kernelPath:     "/fc-kernels/" + versions.KernelVersion + "/vmlinux.bin",
		rootfsPath:     "/fc-vm/sbx-golden/" + versions.RootfsFileName(),
	}, rec, slot
}

// noRateLimit disables both buckets, so no rate_limiter is sent.
var noRateLimit = RateLimiterConfig{Ops: TokenBucketConfig{BucketSize: -1}, Bandwidth: TokenBucketConfig{BucketSize: -1}}

func machineBody(vcpus, memMB int64, extra string) string {
	// Firecracker refuses SMT on arm64, so setMachineConfig sends it only elsewhere.
	return fmt.Sprintf(`{"vcpu_count":%d,"mem_size_mib":%d,"smt":%t,"track_dirty_pages":false%s}`, vcpus, memMB, runtime.GOARCH != archARM64, extra)
}

// The exact requests a Linux sandbox's Create sends to Firecracker, produced
// by the code that decides them, pinned so the fields a guest profile can add
// (machine-config extensions, blk_size, an empty boot_args) stay out of the
// Linux bodies and the devices a profile can skip (entropy, balloon, CPU
// template) stay in: the fleet's Firecracker rejects a body with a field it
// does not know.
func TestLinuxCreateWireBodies(t *testing.T) {
	t.Parallel()

	p, rec, slot := goldenProcess(t, Config{KernelVersion: "vmlinux-6.1", FirecrackerVersion: "v1.14.1_431f1fc"}, 31)
	options := ProcessOptions{InitScriptPath: "/sbin/init", CPUTemplate: &cputemplate.Template{KvmCapabilities: []string{"!121"}}}

	require.NoError(t, p.configureGuestDevices(t.Context(), 2, 512, false, true, false, options, noRateLimit, noRateLimit))

	assert.Equal(t, []string{
		"PUT /balloon", "PUT /boot-source", "PUT /cpu-config", "PUT /drives/rootfs", "PUT /entropy", "PUT /machine-config", "PUT /mmds/config",
		"PUT /network-interfaces/" + slot.VpeerName(),
	}, rec.keys())

	ipv4 := fmt.Sprintf("%s::%s:%s:instance:%s:off:%s", slot.NamespaceIP(), slot.TapIPString(), slot.TapMaskString(), slot.VpeerName(), slot.TapName())
	kernelArgs := buildKernelArgs(ipv4, options).String()
	require.NotEmpty(t, kernelArgs)
	assert.JSONEq(t, fmt.Sprintf(`{"boot_args":%q,"kernel_image_path":"/fc-kernels/vmlinux-6.1/vmlinux.bin"}`, kernelArgs), rec.body(t, "PUT /boot-source"))
	assert.JSONEq(t, `{"drive_id":"rootfs","path_on_host":"/fc-vm/sbx-golden/rootfs.ext4","is_root_device":true}`, rec.body(t, "PUT /drives/rootfs"))
	assert.JSONEq(t, fmt.Sprintf(`{"iface_id":%q,"host_dev_name":%q,"guest_mac":%q}`, slot.VpeerName(), slot.TapName(), slot.TapMAC()), rec.body(t, "PUT /network-interfaces/"+slot.VpeerName()))
	assert.JSONEq(t, fmt.Sprintf(`{"version":"V2","network_interfaces":[%q]}`, slot.VpeerName()), rec.body(t, "PUT /mmds/config"))
	assert.JSONEq(t, machineBody(2, 512, ""), rec.body(t, "PUT /machine-config"))
	assert.JSONEq(t, `{"kvm_capabilities":["!121"]}`, rec.body(t, "PUT /cpu-config"))
	assert.JSONEq(t, `{"amount_mib":0,"deflate_on_oom":false,"free_page_reporting":true}`, rec.body(t, "PUT /balloon"))
	assert.JSONEq(t, `{"rate_limiter":{"bandwidth":{"one_time_burst":0,"refill_time":100,"size":1024}}}`, rec.body(t, "PUT /entropy"))
}

// The restore: a snapshot this runtime wrote loads with no field beyond the
// memory backend.
func TestLinuxResumeWireBody(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	close(ready)

	linux, rec, _ := goldenProcess(t, Config{KernelVersion: "vmlinux-6.1", FirecrackerVersion: "v1.14.1_431f1fc"}, 33)
	require.NoError(t, linux.client.loadSnapshot(t.Context(), "/tmp/uffd.sock", ready, stubSnapfile("/cache/snapfile"), false, false, "", "team-1"))
	assert.JSONEq(t, `{"snapshot_path":"/cache/snapfile","mem_backend":{"backend_path":"/tmp/uffd.sock","backend_type":"Uffd"}}`, rec.body(t, "PUT /snapshot/load"))
}
