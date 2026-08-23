#!/bin/sh
set -eu

BUSYBOX="{{ .BusyBox }}"
RESULT_PATH="{{ .ResultPath }}"

echo "Starting provisioning script"

# Configure DNS before any network use (align with e2b_val: static resolv.conf so apt/system work).
# Remove symlink if present (e.g. systemd-resolved); write static nameservers.
if [ -L /etc/resolv.conf ]; then
    rm -f /etc/resolv.conf
fi
cat > /etc/resolv.conf <<EOF
nameserver 8.8.8.8
nameserver 114.114.114.114
EOF
# Prevent systemd-resolved from taking over resolv.conf
if [ -f /etc/systemd/resolved.conf ]; then
    if ! grep -q "^DNSStubListener=" /etc/systemd/resolved.conf 2>/dev/null; then
        if grep -q "^\[Resolve\]" /etc/systemd/resolved.conf; then
            sed -i '/^\[Resolve\]/a DNSStubListener=no' /etc/systemd/resolved.conf 2>/dev/null || true
        else
            echo -e "\n[Resolve]\nDNSStubListener=no" >> /etc/systemd/resolved.conf
        fi
    else
        sed -i 's/^DNSStubListener=.*/DNSStubListener=no/' /etc/systemd/resolved.conf 2>/dev/null || true
    fi
fi
mkdir -p /etc/systemd/resolved.conf.d/
cat > /etc/systemd/resolved.conf.d/dns.conf <<EOF
[Resolve]
DNS=8.8.8.8 114.114.114.114
FallbackDNS=
Domains=
DNSSEC=no
EOF

echo "Making configuration immutable"
$BUSYBOX chattr +i /etc/resolv.conf

# Detect package manager: apk (Alpine) vs apt (Debian/Ubuntu)
if command -v apk >/dev/null 2>&1; then
    PKG_MANAGER="apk"
elif command -v apt-get >/dev/null 2>&1 || command -v dpkg-query >/dev/null 2>&1; then
    PKG_MANAGER="apt"
else
    echo "E: No supported package manager found (apk or apt-get)"
    exit 1
fi
echo "Detected package manager: $PKG_MANAGER"

if [ "$PKG_MANAGER" = "apk" ]; then
    # Alpine Linux: use apk and OpenRC-compatible packages
    PACKAGES="openrc openssh-server sudo chrony socat curl ca-certificates fuse3 iptables git nfs-utils less nftables iputils jq bash util-linux shadow"
    echo "Checking presence of the following packages: $PACKAGES"

    MISSING=""
    for pkg in $PACKAGES; do
        if ! apk info -e "$pkg" >/dev/null 2>&1; then
            echo "Package $pkg is missing, will install it."
            MISSING="$MISSING $pkg"
        fi
    done

    if [ -n "$MISSING" ]; then
        echo "Missing packages detected, installing:$MISSING"

        # Use Aliyun mirror for Alpine
        ALPINE_VER=$(cat /etc/alpine-release 2>/dev/null | cut -d. -f1,2)
        if [ -n "$ALPINE_VER" ]; then
            echo "Detected Alpine $ALPINE_VER, using Aliyun mirror"
            cat > /etc/apk/repositories <<EOF
https://mirrors.aliyun.com/alpine/v${ALPINE_VER}/main
https://mirrors.aliyun.com/alpine/v${ALPINE_VER}/community
EOF
        fi

        apk update || {
            echo "E: apk update failed (no outbound internet from build VM). On the HOST: enable ip_forward, NAT/MASQUERADE for 169.254.0.0/30, or configure HTTP_PROXY for the build."
            exit 1
        }
        apk add --no-cache $MISSING
    else
        echo "All required packages are already installed."
    fi

    # Register envd as an OpenRC service for the template runtime sandbox.
    # On Alpine, /sbin/init -> openrc-init; OpenRC reads /etc/runlevels/default/
    # but knows nothing about systemd units, so envd.service would never start.
    echo "Setting up envd as OpenRC service"
    cat > /etc/init.d/envd <<'OPENRC_EOF'
#!/sbin/openrc-run
description="e2b envd daemon"
command="/usr/bin/envd"
command_background=yes
pidfile="/run/envd.pid"
output_log="/var/log/envd.log"
error_log="/var/log/envd.log"
OPENRC_EOF
    chmod +x /etc/init.d/envd
    mkdir -p /etc/runlevels/default
    ln -sf /etc/init.d/envd /etc/runlevels/default/envd

    # Replace the provisioning inittab with an Alpine runtime inittab.
    # The provisioning inittab (used by the first VM boot) has fsfreeze which
    # freezes the filesystem before envd can start on the second VM boot.
    # BusyBox init reads /etc/inittab; this new version mounts essential
    # filesystems and respawns envd instead of running the provision sequence.
    echo "Setting up Alpine runtime inittab"
    cat > /etc/inittab << 'INITTAB_EOF'
::sysinit:/bin/sh -c 'mount -t proc proc /proc 2>/dev/null; mount -t sysfs sysfs /sys 2>/dev/null; mount -t devtmpfs devtmpfs /dev 2>/dev/null; mkdir -p /dev/pts && mount -t devpts devpts /dev/pts 2>/dev/null; [ -c /dev/ptmx ] || mknod /dev/ptmx c 5 2 2>/dev/null; true'
::respawn:/usr/bin/envd
::ctrlaltdel:/sbin/reboot
INITTAB_EOF
    # The e2b-injected /usr/bin/busybox is glibc-dynamically-linked and cannot
    # run on Alpine (musl libc has no /lib64/ld-linux-x86-64.so.2). Override it
    # with Alpine's native statically-linked busybox so SyncChangesToDisk works.
    echo "Linking Alpine native busybox to /usr/bin/busybox"
    ln -sf /bin/busybox /usr/bin/busybox
    # envd OOM wrapper hardcodes /usr/bin/ionice and /usr/bin/nice;
    # on Alpine these are busybox applets at /bin/ - create symlinks.
    ln -sf /bin/ionice /usr/bin/ionice
    ln -sf /bin/nice /usr/bin/nice
    # Create the sudo group (Alpine uses wheel by default, but e2b runs
    # usermod -aG sudo user). shadow package provides usermod.
    addgroup sudo 2>/dev/null || true
    echo "%sudo ALL=(ALL:ALL) NOPASSWD: ALL" >> /etc/sudoers

else
    # Debian/Ubuntu: use apt-get
    # Helper function to check if a package is installed
    is_package_installed() {
        dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "install ok installed"
    }

    PACKAGES="systemd systemd-sysv openssh-server sudo chrony socat curl ca-certificates fuse3 iptables git nfs-common less nftables iputils-ping jq"
    echo "Checking presence of the following packages: $PACKAGES"

    MISSING=""
    for pkg in $PACKAGES; do
        if ! is_package_installed "$pkg"; then
            echo "Package $pkg is missing, will install it."
            MISSING="$MISSING $pkg"
        fi
    done

    if [ -n "$MISSING" ]; then
        echo "Missing packages detected, installing:$MISSING"

        # Use Aliyun mirror when archive.ubuntu.com is unreachable (e.g. China, restricted network).
        if [ -f /etc/os-release ]; then
            . /etc/os-release
            DISTRO_ID="$ID"
            CODENAME="${VERSION_CODENAME:-}"
            if [ -z "$CODENAME" ] && command -v lsb_release >/dev/null 2>&1; then
                CODENAME=$(lsb_release -cs 2>/dev/null || true)
            fi
            if [ -n "$CODENAME" ]; then
                case "$DISTRO_ID" in
                    ubuntu)
                        echo "Detected Ubuntu, using Aliyun mirror for $CODENAME"
                        cat > /etc/apt/sources.list <<EOF
deb http://mirrors.aliyun.com/ubuntu/ $CODENAME main restricted universe multiverse
deb http://mirrors.aliyun.com/ubuntu/ $CODENAME-updates main restricted universe multiverse
deb http://mirrors.aliyun.com/ubuntu/ $CODENAME-backports main restricted universe multiverse
deb http://mirrors.aliyun.com/ubuntu/ $CODENAME-security main restricted universe multiverse
EOF
                        ;;
                    debian)
                        echo "Detected Debian, using Aliyun mirror for $CODENAME"
                        cat > /etc/apt/sources.list <<EOF
deb http://mirrors.aliyun.com/debian/ $CODENAME main contrib non-free non-free-firmware
deb http://mirrors.aliyun.com/debian/ $CODENAME-updates main contrib non-free non-free-firmware
deb http://mirrors.aliyun.com/debian/ $CODENAME-backports main contrib non-free non-free-firmware
deb http://mirrors.aliyun.com/debian-security/ $CODENAME-security main contrib non-free non-free-firmware
EOF
                        ;;
                    *)
                        echo "Keeping default apt sources for $DISTRO_ID"
                        ;;
                esac
            else
                echo "Could not determine distribution codename; keeping default apt sources for $DISTRO_ID"
            fi
        fi

        apt-get -q update || {
            echo "E: apt-get update failed (no outbound internet from build VM). On the HOST: enable ip_forward, NAT/MASQUERADE for 169.254.0.0/30, or configure HTTP_PROXY for the build."
            exit 1
        }
        DEBIAN_FRONTEND=noninteractive DEBCONF_NOWARNINGS=yes apt-get -qq -o=Dpkg::Use-Pty=0 install -y --no-install-recommends $MISSING
        # After installing systemd, resolv.conf may have become a symlink again; restore static DNS.
        if [ -L /etc/resolv.conf ]; then
            $BUSYBOX chattr -i /etc/resolv.conf 2>/dev/null || true
            rm -f /etc/resolv.conf
            cat > /etc/resolv.conf <<EOF
nameserver 8.8.8.8
nameserver 114.114.114.114
EOF
        fi
    else
        echo "All required packages are already installed."
    fi
fi

# Set /dev/fuse permissions to 666 for non-root access
if [ "$PKG_MANAGER" = "apt" ]; then
    # Use systemd-tmpfiles to set permissions at boot
    mkdir -p /etc/tmpfiles.d
    echo 'z /dev/fuse 0666 root root -' > /etc/tmpfiles.d/fuse.conf
fi

echo "Setting up shell"
echo "export SHELL='/bin/bash'" >/etc/profile.d/shell.sh
echo "export PS1='\w \$ '" >/etc/profile.d/prompt.sh
echo "export PS1='\w \$ '" >>"/etc/profile"
echo "export PS1='\w \$ '" >>"/root/.bashrc"

echo "Use .bashrc and .profile"
echo "if [ -f ~/.bashrc ]; then source ~/.bashrc; fi; if [ -f ~/.profile ]; then source ~/.profile; fi" >>/etc/profile

echo "Remove root password"
passwd -d root

echo "Setting up chrony"
mkdir -p /etc/chrony
cat <<EOF >/etc/chrony/chrony.conf
refclock PHC /dev/ptp0 poll 2 dpoll 2
# Step (jump) the clock instead of slewing when the offset exceeds 1s, but only
# for the first 3 updates after chronyd starts. chronyd restarts on every cold
# boot/reboot, so this corrects a large boot-time offset fast (TLS needs a
# correct clock) without risking a backward jump under a running workload.
# Needed because chrony-wait is masked, so boot no longer blocks on first sync.
makestep 1.0 3
EOF

# Add a proxy config, as some environments expects it there (e.g. timemaster in Node Dockerimage)
echo "include /etc/chrony/chrony.conf" >/etc/chrony.conf

echo "Setting up SSH"
mkdir -p /etc/ssh
cat <<EOF >>/etc/ssh/sshd_config
PermitRootLogin yes
PermitEmptyPasswords yes
PasswordAuthentication yes
EOF

echo "Increasing inotify watch limit"
echo 'fs.inotify.max_user_watches=65536' | tee -a /etc/sysctl.conf

# Disable kcompactd background page migration. With 2 MiB host-side hugepage
# backing of guest RAM, every migration dirties a destination hugepage from
# the host UFFD's perspective and lands in the next memfile diff, with no
# corresponding workload benefit between snapshots. We trigger compaction
# explicitly pre-pause instead.
echo "Disabling proactive memory compaction"
echo 'vm.compaction_proactiveness=0' | tee -a /etc/sysctl.conf

if command -v systemctl >/dev/null 2>&1; then
    echo "Don't wait for ttyS0 (serial console kernel logs)"
    systemctl mask serial-getty@ttyS0.service

    echo "Disable network online wait"
    systemctl mask systemd-networkd-wait-online.service

    echo "Disable system first boot wizard"
    systemctl mask systemd-firstboot.service

    echo "Disable chrony-wait"
    systemctl mask chrony-wait.service

    echo "Disable slow boot units not needed in the sandbox"
    systemctl mask systemd-binfmt.service
    systemctl mask e2scrub_reap.service
else
    echo "Skipping systemctl service masking (not a systemd-based distro)"
fi

# Clean machine-id from Docker
rm -rf /etc/machine-id

echo "Linking init"
if [ -f /lib/systemd/systemd ]; then
    ln -sf /lib/systemd/systemd /usr/sbin/init
elif [ -f /sbin/openrc-init ]; then
    ln -sf /sbin/openrc-init /sbin/init
    ln -sf /sbin/openrc-init /usr/sbin/init
elif [ -f /sbin/init ]; then
    echo "Using existing /sbin/init"
else
    echo "Warning: no init system found"
fi

echo "Unlocking immutable configuration"
$BUSYBOX chattr -i /etc/resolv.conf

echo "Finished provisioning script"

# Delete itself
rm -rf /etc/init.d/rcS
rm -rf /usr/local/bin/provision.sh

# Report successful provisioning
printf "0" > "$RESULT_PATH"