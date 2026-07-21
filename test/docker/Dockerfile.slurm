# Single-node Slurm for the obol integration tier. Runs munge + slurmctld +
# slurmd + slurmdbd on one localhost node with the obol GATE seam installed, so
# the real job_submit.lua -> obold -> SETTLE path is exercised against an actual
# slurmctld (docs/SEAM_DESIGN.md).
#
# Slurm version: EPEL ships 22.05 for Rocky 9. That is deliberately fine here —
# 22.05 is burstlab Gen 1 and the generation whose admin_comment writability is
# the first unconfirmed blocker (SEAM_DESIGN.md §10 / §13 gap #1), so this tier
# validates exactly that. Newer generations (23.11/24.05) come from source builds
# under issue #16.
#
# The obold + obol binaries are built on the host and COPYed in (see the test
# harness / build script), so this image does not need the Go toolchain.

FROM rockylinux:9

# Slurm + munge + lua + luasocket from EPEL/CRB. mariadb for slurmdbd's store.
RUN dnf install -y -q epel-release 'dnf-command(config-manager)' \
 && dnf config-manager --set-enabled crb \
 && dnf install -y -q \
      slurm slurm-slurmctld slurm-slurmd slurm-slurmdbd slurm-perlapi \
      munge lua lua-socket \
      mariadb-server mariadb \
      procps-ng iproute hostname sudo shadow-utils \
 && dnf clean all

# Create the slurm service user (the RPMs don't always create it), then the
# state/log/run dirs. munge/mysql users come from their RPMs.
RUN getent group slurm  >/dev/null || groupadd -r slurm \
 && getent passwd slurm >/dev/null || useradd -r -g slurm -d /var/lib/slurm -s /sbin/nologin slurm \
 && install -d -o slurm -g slurm /var/spool/slurmctld /var/spool/slurmd \
      /var/log/slurm /run/slurm \
 && install -d -o munge -g munge /var/log/munge /var/lib/munge /run/munge \
 && install -d -o slurm -g slurm /run/obol /var/lib/obol \
 && install -d /etc/slurm/lua

# Config + entrypoint + the obol seam.
COPY test/docker/slurm.conf      /etc/slurm/slurm.conf
COPY test/docker/slurmdbd.conf   /etc/slurm/slurmdbd.conf
COPY test/docker/cgroup.conf     /etc/slurm/cgroup.conf
COPY test/docker/obold-config.json /etc/obol/obold-config.json
COPY test/docker/entrypoint.sh   /usr/local/bin/entrypoint.sh
COPY seam/lua/obol_wire.lua      /etc/slurm/lua/obol_wire.lua
COPY seam/lua/obol_transport.lua /etc/slurm/lua/obol_transport.lua
COPY seam/lua/job_submit.lua     /etc/slurm/job_submit.lua
COPY seam/slurm/obol-prolog.sh   /usr/local/bin/obol-prolog.sh
COPY seam/slurm/obol-epilog.sh   /usr/local/bin/obol-epilog.sh
COPY seam/slurm/obol-jobcomp.sh  /usr/local/bin/obol-jobcomp.sh

# The obol binaries, built on the host into test/docker/bin/ before docker build.
COPY test/docker/bin/obold /usr/local/bin/obold
COPY test/docker/bin/obol  /usr/local/bin/obol

RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/obol-prolog.sh \
      /usr/local/bin/obol-epilog.sh /usr/local/bin/obol-jobcomp.sh \
      /usr/local/bin/obold /usr/local/bin/obol \
 && chown slurm:slurm /etc/slurm/slurmdbd.conf && chmod 600 /etc/slurm/slurmdbd.conf

# slurm.conf points JobSubmitPlugins at /etc/slurm/job_submit.lua; the lua module
# path must include /etc/slurm/lua. The shim sets these via LUA_PATH in the env.
ENV LUA_PATH="/etc/slurm/lua/?.lua;;" \
    OBOL_SOCKET="/run/obol/obold.sock" \
    OBOL_BIN="/usr/local/bin/obol"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
