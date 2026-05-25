#!/usr/bin/env bash
# detect_fc_leaks.sh - Comprehensive Firecracker leak detection and diagnostics
# 
# Usage: ./detect_fc_leaks.sh [ORCHESTRATOR_BASE_PATH]
# 
# This script performs a comprehensive audit of Firecracker-related resources,
# including processes, network namespaces, devices, and storage artifacts.
# 
# Requires: root privileges (sudo)

set -u

# Configuration
ORCHESTRATOR_BASE_PATH="${1:-.local-build/orchestrator}"
OUT="/tmp/fc-leak-report-$(date +%Y%m%dT%H%M%S).txt"
TEMP_DIR=$(python3 -c "import tempfile; print(tempfile.gettempdir())" 2>/dev/null || echo "/tmp")

# Colors for console output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Helper functions
sep() { 
    printf "\n==== %s ====" "$1" >> "$OUT"
    printf "\n\n" >> "$OUT"
    printf "${BLUE}==== %s ====${NC}\n" "$1"
}

log_info() {
    printf "${GREEN}✓${NC} %s\n" "$1"
    echo "✓ $1" >> "$OUT"
}

log_warn() {
    printf "${YELLOW}⚠${NC} %s\n" "$1"
    echo "⚠ $1" >> "$OUT"
}

log_error() {
    printf "${RED}✗${NC} %s\n" "$1"
    echo "✗ $1" >> "$OUT"
}

# Header
{
    echo "Firecracker Leak Detection Report"
    echo "=================================="
    echo "Timestamp: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    echo "Orchestrator base: $ORCHESTRATOR_BASE_PATH"
    echo "Temp directory: $TEMP_DIR"
    echo "Hostname: $(hostname)"
    echo "Kernel: $(uname -r)"
    echo ""
} > "$OUT"

printf "${BLUE}Firecracker Leak Detection Report${NC}\n"
printf "Timestamp: $(date -u +"%Y-%m-%dT%H:%M:%SZ")\n"
printf "Orchestrator base: $ORCHESTRATOR_BASE_PATH\n"
printf "Temp directory: $TEMP_DIR\n\n"

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    log_warn "Not running as root. Some checks may be incomplete. Run with: sudo $0"
fi

# helper
sep() { 
    printf "\n==== %s ====" "$1" >> "$OUT"
    printf "\n\n" >> "$OUT"
}

sep "Firecracker processes"
FC_COUNT=$(pgrep -c firecracker 2>/dev/null || echo 0)
if [ "$FC_COUNT" -gt 0 ]; then
    log_error "Found $FC_COUNT Firecracker process(es)"
    pgrep -af firecracker 2>/dev/null | tee -a "$OUT"
else
    log_info "No Firecracker processes found"
    echo "(none)" >> "$OUT"
fi
echo "" >> "$OUT"
echo "Count: $FC_COUNT" >> "$OUT"

sep "Firecracker process details (ps + state + /proc/<pid>/cmdline summary)"
if pids=$(pgrep -f firecracker 2>/dev/null); then
  for p in $pids; do
    printf "PID: %s\n" "$p" >> "$OUT"
    ps -o pid,ppid,stat,etime,cmd -p "$p" 2>/dev/null | sed -n '1,1p' >> "$OUT"
    ps -o pid,ppid,stat,etime,cmd -p "$p" 2>/dev/null | sed -n '2,2p' >> "$OUT"
    if [ -r "/proc/$p/fd" ]; then
      echo "Open files (first 20):" >> "$OUT"
      ls -l /proc/$p/fd 2>/dev/null | head -n 20 >> "$OUT"
    fi
    echo "" >> "$OUT"
  done
else
  echo "none" >> "$OUT"
fi

sep "Uninterruptible (D) state processes (possible stuck IO)"
ps -eo pid,ppid,stat,cmd | awk '$3 ~ /D/ {print}' | tee -a "$OUT" || true

sep "Network namespaces"
NETNS_COUNT=$(ip netns list 2>/dev/null | wc -l || echo 0)
if [ "$NETNS_COUNT" -gt 0 ]; then
    log_error "Found $NETNS_COUNT network namespace(s)"
    ip netns list 2>/dev/null | tee -a "$OUT"
else
    log_info "No orphaned network namespaces found"
    echo "(none)" >> "$OUT"
fi
echo "" >> "$OUT"
echo "Netns count: $NETNS_COUNT" >> "$OUT"

sep "For each namespace: PIDs inside and top cmds"
for ns in $(ip netns list 2>/dev/null | awk '{print $1}' 2>/dev/null || true); do
  echo "Namespace: $ns" >> "$OUT"
  ip netns pids "$ns" 2>/dev/null | tee -a "$OUT" || echo "(no pids or no permission)" >> "$OUT"
  for pid in $(ip netns pids "$ns" 2>/dev/null || true); do
    ps -o pid,ppid,stat,etime,cmd -p "$pid" 2>/dev/null | sed -n '2p' >> "$OUT"
  done
  echo "" >> "$OUT"
done

sep "Network devices (veth/tap/vpeer) and counts"
VETH_COUNT=$(ip link show 2>/dev/null | grep -E 'veth|tap|vpeer' -c || echo 0)
if [ "$VETH_COUNT" -gt 0 ]; then
    log_error "Found $VETH_COUNT orphaned network device(s)"
    ip link show 2>/dev/null | grep -E 'veth|tap|vpeer' -n | tee -a "$OUT"
else
    log_info "No orphaned network devices found"
    echo "(none)" >> "$OUT"
fi
echo "" >> "$OUT"
echo "veth/tap count: $VETH_COUNT" >> "$OUT"

sep "iptables rules mentioning veth/tap/REDIRECT"
if command -v iptables-save >/dev/null 2>&1; then
    IPTABLES_COUNT=$(iptables-save 2>/dev/null | grep -E 'veth|tap|vpeer|REDIRECT' -c || echo 0)
    if [ "$IPTABLES_COUNT" -gt 0 ]; then
        log_error "Found $IPTABLES_COUNT orphaned iptables rule(s)"
        iptables-save 2>/dev/null | grep -E 'veth|tap|vpeer|REDIRECT' -n | tee -a "$OUT"
    else
        log_info "No orphaned iptables rules found"
        echo "(none)" >> "$OUT"
    fi
else
    log_warn "iptables-save not available"
    echo "iptables-save not available" >> "$OUT"
fi

sep "Firecracker artifacts in temp directory"
FC_SOCKETS=$(find "$TEMP_DIR" -maxdepth 1 -name 'fc-*.sock' 2>/dev/null | wc -l || echo 0)
UFFD_SOCKETS=$(find "$TEMP_DIR" -maxdepth 1 -name 'uffd-*.sock' 2>/dev/null | wc -l || echo 0)
METRICS_FIFOS=$(find "$TEMP_DIR" -maxdepth 1 -name 'fc-metrics-*' 2>/dev/null | wc -l || echo 0)

if [ "$FC_SOCKETS" -gt 0 ] || [ "$UFFD_SOCKETS" -gt 0 ] || [ "$METRICS_FIFOS" -gt 0 ]; then
    log_error "Found orphaned Firecracker artifacts in $TEMP_DIR"
    find "$TEMP_DIR" -maxdepth 1 \( -name 'fc-*.sock' -o -name 'uffd-*.sock' -o -name 'fc-metrics-*' \) -ls 2>/dev/null | tee -a "$OUT"
else
    log_info "No orphaned Firecracker artifacts in $TEMP_DIR"
    echo "(none)" >> "$OUT"
fi
echo "" >> "$OUT"
echo "FC sockets: $FC_SOCKETS, UFFD sockets: $UFFD_SOCKETS, Metrics FIFOs: $METRICS_FIFOS" >> "$OUT"

sep "Rootfs cache artifacts in orchestrator"
if [ -d "$ORCHESTRATOR_BASE_PATH/sandbox" ]; then
    ROOTFS_COW=$(find "$ORCHESTRATOR_BASE_PATH/sandbox" -maxdepth 1 -name 'rootfs-*.cow' 2>/dev/null | wc -l || echo 0)
    ROOTFS_LINK=$(find "$ORCHESTRATOR_BASE_PATH/sandbox" -maxdepth 1 -name 'rootfs-*.link' 2>/dev/null | wc -l || echo 0)
    
    if [ "$ROOTFS_COW" -gt 0 ] || [ "$ROOTFS_LINK" -gt 0 ]; then
        log_error "Found orphaned rootfs artifacts in $ORCHESTRATOR_BASE_PATH/sandbox"
        find "$ORCHESTRATOR_BASE_PATH/sandbox" -maxdepth 1 \( -name 'rootfs-*.cow' -o -name 'rootfs-*.link' \) -ls 2>/dev/null | tee -a "$OUT"
    else
        log_info "No orphaned rootfs artifacts found"
        echo "(none)" >> "$OUT"
    fi
    echo "" >> "$OUT"
    echo "Rootfs COW: $ROOTFS_COW, Rootfs links: $ROOTFS_LINK" >> "$OUT"
else
    log_warn "Orchestrator sandbox directory not found: $ORCHESTRATOR_BASE_PATH/sandbox"
    echo "Directory not found: $ORCHESTRATOR_BASE_PATH/sandbox" >> "$OUT"
fi

sep "Unix domain sockets open by processes (ss -x)"
if command -v ss >/dev/null 2>&1; then
    UNIX_SOCKETS=$(ss -x -a -n -p 2>/dev/null | grep -E 'firecracker|fc|nbd|uffd' -c || echo 0)
    if [ "$UNIX_SOCKETS" -gt 0 ]; then
        log_error "Found $UNIX_SOCKETS matching unix socket(s)"
        ss -x -a -n -p 2>/dev/null | grep -E 'firecracker|fc|nbd|uffd' -n | tee -a "$OUT"
    else
        log_info "No matching unix sockets found"
        echo "(none)" >> "$OUT"
    fi
else
    log_warn "ss command not installed"
    echo "ss not installed" >> "$OUT"
fi

sep "NBD-related files/sockets"
if [ -d "$ORCHESTRATOR_BASE_PATH" ]; then
    NBD_COUNT=$(find "$ORCHESTRATOR_BASE_PATH" -xdev \( -type s -o -name '*nbd*' \) 2>/dev/null | wc -l || echo 0)
    if [ "$NBD_COUNT" -gt 0 ]; then
        log_error "Found $NBD_COUNT NBD-related artifact(s)"
        find "$ORCHESTRATOR_BASE_PATH" -xdev \( -type s -o -name '*nbd*' \) -ls 2>/dev/null | tee -a "$OUT"
    else
        log_info "No NBD artifacts found"
        echo "(none)" >> "$OUT"
    fi
else
    log_warn "Orchestrator directory not found: $ORCHESTRATOR_BASE_PATH"
    echo "Directory not found: $ORCHESTRATOR_BASE_PATH" >> "$OUT"
fi

sep "Summary counts"
D_STATE=$(ps -eo stat 2>/dev/null | grep -c 'D' || echo 0)

{
    echo "Summary of Findings:"
    echo "==================="
    echo "Firecracker processes: $FC_COUNT"
    echo "Network namespaces: $NETNS_COUNT"
    echo "Orphaned network devices: $VETH_COUNT"
    echo "Uninterruptible (D) state processes: $D_STATE"
    echo ""
    echo "Firecracker artifacts:"
    echo "  FC sockets: $FC_SOCKETS"
    echo "  UFFD sockets: $UFFD_SOCKETS"
    echo "  Metrics FIFOs: $METRICS_FIFOS"
    echo "  Rootfs COW files: ${ROOTFS_COW:-0}"
    echo "  Rootfs links: ${ROOTFS_LINK:-0}"
    echo ""
} | tee -a "$OUT"

# Print summary to console
printf "\n${BLUE}=== Summary ===${NC}\n"
printf "Firecracker processes: $FC_COUNT\n"
printf "Network namespaces: $NETNS_COUNT\n"
printf "Orphaned network devices: $VETH_COUNT\n"
printf "Uninterruptible (D) state processes: $D_STATE\n"
printf "\nFirecracker artifacts:\n"
printf "  FC sockets: $FC_SOCKETS\n"
printf "  UFFD sockets: $UFFD_SOCKETS\n"
printf "  Metrics FIFOs: $METRICS_FIFOS\n"
printf "  Rootfs COW files: ${ROOTFS_COW:-0}\n"
printf "  Rootfs links: ${ROOTFS_LINK:-0}\n"

# Determine overall health
TOTAL_LEAKS=$((FC_COUNT + NETNS_COUNT + VETH_COUNT + FC_SOCKETS + UFFD_SOCKETS + METRICS_FIFOS + ${ROOTFS_COW:-0} + ${ROOTFS_LINK:-0}))

{
    echo ""
    echo "Tips:"
    echo "- Run as root (sudo) for full visibility (netns, iptables, lsof info)."
    echo "- If you find netns with PIDs inside, inspect those PIDs and their /proc/<pid>/fd to see held files."
    echo "- Use 'reconcile' command for quick iptables and socket scan."
    echo "- For remediation, see: packages/orchestrator/cmd/reconcile/README.md"
    echo ""
} | tee -a "$OUT"

printf "\n${BLUE}=== Report ===${NC}\n"
printf "Report generated: ${GREEN}$OUT${NC}\n"
printf "Total leaks detected: "
if [ "$TOTAL_LEAKS" -eq 0 ]; then
    printf "${GREEN}0 (system is clean)${NC}\n"
else
    printf "${RED}$TOTAL_LEAKS${NC}\n"
fi

printf "\n${BLUE}Preview (first 100 lines):${NC}\n"
printf "----------------------------------------\n"
sed -n '1,100p' "$OUT"
printf "----------------------------------------\n"
printf "\nFull report: cat $OUT\n"
