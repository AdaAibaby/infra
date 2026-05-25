package reconcile

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// RunReconcile scans common Firecracker artifact locations and iptables
// and writes a plain-text report to stdout and to /tmp/reconcile-report-<ts>.txt.
// injectable vars to make the package testable
var (
	SocketDirs      = []string{} // auto-discovered if empty
	MetricsDir      = ""          // auto-discovered if empty
	ExecCommand     = exec.Command
	OrchestratorDir = ""           // auto-discovered if empty
)

func RunReconcile(outPath string) error {
	ts := time.Now().UTC().Format("20060102T150405Z")
	if outPath == "" {
		outPath = fmt.Sprintf("/tmp/reconcile-report-%s.txt", ts)
	}

	// Auto-discover paths if not set
	socketDirs := SocketDirs
	if len(socketDirs) == 0 {
		socketDirs = discoverSocketDirs()
	}

	metricsDir := MetricsDir
	if metricsDir == "" {
		metricsDir = os.TempDir()
	}

	orchestratorDir := OrchestratorDir
	if orchestratorDir == "" {
		orchestratorDir = discoverOrchestratorDir()
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Reconcile sweep report\nTimestamp: %s\n\n", ts)

	// Report discovered paths
	fmt.Fprintf(&buf, "Discovered paths:\n")
	fmt.Fprintf(&buf, "  Temp directory: %s\n", os.TempDir())
	fmt.Fprintf(&buf, "  Orchestrator base: %s\n", orchestratorDir)
	fmt.Fprintf(&buf, "\n")

	// scan socket dirs
	fmt.Fprintln(&buf, "==== Firecracker api sockets ====")
	for _, d := range socketDirs {
		scanAndWrite(&buf, d, "fc-*.sock")
	}

	fmt.Fprintln(&buf, "\n==== uffd sockets ====")
	for _, d := range socketDirs {
		scanAndWrite(&buf, d, "uffd-*.sock")
	}

	fmt.Fprintln(&buf, "\n==== metrics FIFOs ====")
	scanAndWrite(&buf, metricsDir, "fc-metrics-*")

	// scan orchestrator cache
	if orchestratorDir != "" {
		sandboxCacheDir := filepath.Join(orchestratorDir, "sandbox")
		if _, err := os.Stat(sandboxCacheDir); err == nil {
			fmt.Fprintf(&buf, "\n==== Rootfs COW files (%s) ====\n", sandboxCacheDir)
			scanAndWrite(&buf, sandboxCacheDir, "rootfs-*.cow")

			fmt.Fprintf(&buf, "\n==== Rootfs links (%s) ====\n", sandboxCacheDir)
			scanAndWrite(&buf, sandboxCacheDir, "rootfs-*.link")
		}
	}

	fmt.Fprintln(&buf, "\n==== iptables (nat table) ====")
	if out, err := ExecCommand("iptables-save", "-t", "nat").CombinedOutput(); err != nil {
		fmt.Fprintf(&buf, "iptables-save error: %v\n", err)
	} else {
		buf.Write(out)
	}

	// write report
	if err := ioutil.WriteFile(outPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	// also echo to stdout
	_, _ = os.Stdout.Write(buf.Bytes())
	fmt.Fprintf(os.Stdout, "\nReport written to: %s\n", outPath)
	return nil
}

// discoverSocketDirs returns socket directories where fc-*.sock files may exist.
// fc-*.sock files are created by orchestrator in os.TempDir() (determined by TMPDIR env var).
// This function discovers all possible socket directories by:
// 1. Checking TMPDIR environment variable (highest priority)
// 2. Checking os.TempDir() (default /tmp on Linux)
// 3. Scanning common alternative temp directories if they exist
func discoverSocketDirs() []string {
	dirs := make(map[string]bool) // Use map to deduplicate

	// 1. Check TMPDIR environment variable (orchestrator respects this)
	if tmpdir := os.Getenv("TMPDIR"); tmpdir != "" {
		if _, err := os.Stat(tmpdir); err == nil {
			dirs[tmpdir] = true
		}
	}

	// 2. Check os.TempDir() (default /tmp on Linux)
	tempDir := os.TempDir()
	dirs[tempDir] = true

	// 3. Check common alternative temp directories on orchestrator nodes
	// These may be used if TMPDIR is set to a custom location
	commonTempDirs := []string{
		"/tmp",
		"/data0/tmp",
		"/data1/tmp",
		"/mnt/tmp",
		"/var/tmp",
	}

	for _, dir := range commonTempDirs {
		if _, err := os.Stat(dir); err == nil {
			dirs[dir] = true
		}
	}

	// Convert map to slice, maintaining order (TMPDIR first, then os.TempDir, then others)
	result := []string{}
	if tmpdir := os.Getenv("TMPDIR"); tmpdir != "" && dirs[tmpdir] {
		result = append(result, tmpdir)
		delete(dirs, tmpdir)
	}
	if dirs[tempDir] {
		result = append(result, tempDir)
		delete(dirs, tempDir)
	}
	for _, dir := range commonTempDirs {
		if dirs[dir] {
			result = append(result, dir)
		}
	}

	return result
}

// discoverOrchestratorDir returns the orchestrator base directory
func discoverOrchestratorDir() string {
	// Check environment variable first
	if dir := os.Getenv("ORCHESTRATOR_BASE_PATH"); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}

	// Check common local paths
	commonPaths := []string{
		".local-build/orchestrator",
		"/orchestrator",
		"/mnt/orchestrator",
	}

	for _, path := range commonPaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

func scanAndWrite(buf *bytes.Buffer, dir, pattern string) {
	if dir == "" {
		return
	}

	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		fmt.Fprintf(buf, "scan %s/%s error: %v\n", dir, pattern, err)
		return
	}
	if len(files) == 0 {
		fmt.Fprintf(buf, "(none found) %s/%s\n", dir, pattern)
		return
	}
	for _, f := range files {
		fi, err := os.Lstat(f)
		if err != nil {
			fmt.Fprintf(buf, "%s (stat error: %v)\n", f, err)
			continue
		}
		fmt.Fprintf(buf, "%s  %s\n", f, fi.Mode().String())
	}
}
