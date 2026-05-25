# Reconcile Sweep

Diagnostic tool for detecting Firecracker sandbox leaks and orphaned resources.

## Overview

This command scans the host for Firecracker-related artifacts and generates a diagnostic report. It helps identify:

- **Orphaned Firecracker processes** (via API sockets)
- **Leaked memory resources** (via uffd sockets)
- **Orphaned metrics FIFOs** (metrics collection endpoints)
- **Leaked storage** (rootfs COW files and links)
- **Orphaned iptables rules** (NAT REDIRECT rules for sandbox networking)

## Build

```bash
cd packages/orchestrator
go build -o reconcile ./cmd/reconcile
```

## Usage

### Basic Usage

```bash
# Generate report with auto-discovered paths
sudo ./reconcile

# Specify custom output path
sudo ./reconcile -out /var/log/fc-reconcile-$(date +%s).txt
```

### Flags

- `-out string` — Output report path (default: `/tmp/reconcile-report-<timestamp>.txt`)

### Example Output

**Healthy system (no leaks):**
```
Reconcile sweep report
Timestamp: 20260525T143022Z

Discovered paths:
  Temp directory: /tmp
  Orchestrator base: /orchestrator

==== Firecracker api sockets ====
(none found) /tmp/fc-*.sock

==== uffd sockets ====
(none found) /tmp/uffd-*.sock

==== metrics FIFOs ====
(none found) /tmp/fc-metrics-*

==== Rootfs COW files (/orchestrator/sandbox) ====
(none found) /orchestrator/sandbox/rootfs-*.cow

==== Rootfs links (/orchestrator/sandbox) ====
(none found) /orchestrator/sandbox/rootfs-*.link

==== iptables (nat table) ====
*nat
:PREROUTING ACCEPT [0:0]
:INPUT ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
COMMIT
```

**System with custom TMPDIR (e.g., /data0/tmp):**
```
Reconcile sweep report
Timestamp: 20260525T143022Z

Discovered paths:
  Temp directory: /data0/tmp
  Orchestrator base: /orchestrator

==== Firecracker api sockets ====
/data0/tmp/fc-sandbox-abc123.sock
/data0/tmp/fc-sandbox-def456.sock

==== uffd sockets ====
(none found) /data0/tmp/uffd-*.sock

==== metrics FIFOs ====
/data0/tmp/fc-metrics-sandbox-abc123.fifo
/data0/tmp/fc-metrics-sandbox-def456.fifo

==== Rootfs COW files (/orchestrator/sandbox) ====
(none found) /orchestrator/sandbox/rootfs-*.cow

==== Rootfs links (/orchestrator/sandbox) ====
(none found) /orchestrator/sandbox/rootfs-*.link

==== iptables (nat table) ====
*nat
:PREROUTING ACCEPT [0:0]
:INPUT ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
COMMIT
```

## Path Discovery

The tool automatically discovers artifact locations by scanning the orchestrator node:

### Socket Directories (fc-*.sock, uffd-*.sock)

Socket files are created by orchestrator in the temp directory. The discovery order is:

1. **`TMPDIR` environment variable** (highest priority) — If set, orchestrator uses this for socket files
2. **`os.TempDir()`** (default `/tmp` on Linux) — Standard Go temp directory
3. **Common alternative temp directories** — Scanned if they exist:
   - `/data0/tmp`, `/data1/tmp` (common on GCP orchestrator nodes)
   - `/mnt/tmp`, `/var/tmp` (alternative temp locations)

**Why multiple directories?** On orchestrator nodes, `TMPDIR` may be set to a custom location (e.g., `/data0/tmp` or `/data1/tmp`) to use faster local storage. The reconcile tool scans all possible locations to ensure comprehensive leak detection.

### Other Artifacts

| Artifact | Location |
|----------|----------|
| Metrics FIFOs | Same as socket directories (determined by `os.TempDir()`) |
| Rootfs COW files | `$ORCHESTRATOR_BASE_PATH/sandbox/rootfs-*.cow` |
| Rootfs links | `$ORCHESTRATOR_BASE_PATH/sandbox/rootfs-*.link` |

### Environment Variables

- `TMPDIR` — Override temp directory for socket files (orchestrator respects this)
- `ORCHESTRATOR_BASE_PATH` — Override orchestrator base directory (default: auto-discovered)

## Safety & Guarantees

- **Read-only**: The command does not modify iptables, network devices, or VMs
- **Privileges**: Reading the nat table requires root; run with `sudo` if necessary
- **No writes**: Only executes `iptables-save` (read-only command)
- **Testability**: The package exposes injectable `ExecCommand` for unit tests

## Operational Notes

### Do NOT Use as Automated Cleanup

This tool is **diagnostic only**. Do not run it as an automated cleanup tool. Any remediation (deleting sockets, removing iptables rules, killing processes) must be performed under an explicit, reviewed runbook.

### Interpreting Results

| Finding | Interpretation |
|---------|-----------------|
| FC sockets > 0 | Orphaned Firecracker processes |
| UFFD sockets > 0 | Leaked memory resources |
| Metrics FIFOs > 0 | Orphaned metrics collection |
| Rootfs COW > 0 | Leaked storage (disk space leak) |
| REDIRECT rules > 0 | Orphaned network rules |

### Incident Response Workflow

1. **Run reconcile** to generate diagnostic report
2. **Analyze findings** to identify leak patterns
3. **Correlate with logs** (Grafana Loki, orchestrator logs)
4. **Develop remediation runbook** (manual or automated)
5. **Test remediation** in staging environment
6. **Execute remediation** with explicit approval
7. **Verify cleanup** by running reconcile again

## Integration with detect_fc_leaks.sh

For comprehensive leak detection, use alongside `detect_fc_leaks.sh`:

```bash
# Quick reconcile sweep
sudo ./reconcile -out /tmp/reconcile.txt

# Comprehensive leak audit (includes process details, netns, etc.)
bash ../scripts/detect_fc_leaks.sh /orchestrator
```

## Troubleshooting

### Permission Denied

```bash
# iptables-save requires root
sudo ./reconcile -out /tmp/report.txt
```

### Paths Not Discovered

```bash
# Set environment variable explicitly
export ORCHESTRATOR_BASE_PATH=/custom/path
sudo ./reconcile -out /tmp/report.txt
```

### No Artifacts Found (Expected)

If all sections show "(none found)", the system is clean:

```
✓ No orphaned Firecracker processes
✓ No leaked memory resources
✓ No orphaned metrics
✓ No leaked storage
✓ No orphaned network rules
```


