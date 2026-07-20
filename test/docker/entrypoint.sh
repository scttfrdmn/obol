#!/bin/bash
# entrypoint.sh — boot a single-node Slurm + obold inside the container, then
# idle. The test harness (test/docker/slurm_test.go) execs into the running
# container to submit jobs and assert on budget state.
# Not set -e: service daemons and probes return nonzero in normal operation
# (a dbd not-yet-up, an idempotent sacctmgr add); we handle failures explicitly
# and must not let a transient nonzero abort the whole boot.
set -uo pipefail

log() { echo "[entrypoint] $*"; }

# --- munge (auth) ---
log "starting munge"
# munged needs its runtime socket dir (Slurm looks for /run/munge/munge.socket.2).
install -d -o munge -g munge -m 0755 /run/munge /var/run/munge
/usr/sbin/create-munge-key -f >/dev/null 2>&1 || true
chown munge:munge /etc/munge/munge.key 2>/dev/null || true
chmod 400 /etc/munge/munge.key 2>/dev/null || true
runuser -u munge -- /usr/sbin/munged --force --num-threads=10
for i in $(seq 1 20); do
  [ -S /run/munge/munge.socket.2 ] && break
  sleep 0.25
done

# --- mariadb (slurmdbd store) ---
log "starting mariadb"
mkdir -p /var/lib/mysql /run/mariadb
chown -R mysql:mysql /var/lib/mysql /run/mariadb
if [ ! -d /var/lib/mysql/mysql ]; then
  mariadb-install-db --user=mysql --datadir=/var/lib/mysql >/dev/null 2>&1
fi
runuser -u mysql -- /usr/libexec/mariadbd --datadir=/var/lib/mysql --socket=/var/lib/mysql/mysql.sock >/var/log/mariadb.log 2>&1 &
for i in $(seq 1 30); do
  mysqladmin --socket=/var/lib/mysql/mysql.sock ping >/dev/null 2>&1 && break
  sleep 0.5
done
mysql --socket=/var/lib/mysql/mysql.sock <<'SQL'
CREATE DATABASE IF NOT EXISTS slurm_acct_db;
CREATE USER IF NOT EXISTS 'slurm'@'localhost' IDENTIFIED BY 'slurm';
GRANT ALL ON slurm_acct_db.* TO 'slurm'@'localhost';
FLUSH PRIVILEGES;
SQL
# slurmdbd's mysql plugin connects at the compiled-in default socket
# (/var/lib/mysql/mysql.sock), which is exactly where mariadbd listens above.

# --- slurmdbd ---
log "starting slurmdbd"
runuser -u slurm -- /usr/sbin/slurmdbd
# Wait until slurmdbd is actually accepting connections on its port, else
# slurmctld fatals ("we have no TRES from it") on an empty DB.
for i in $(seq 1 40); do
  if (exec 3<>/dev/tcp/localhost/6819) 2>/dev/null; then exec 3>&- 3<&-; break; fi
  sleep 0.5
done

# Register the accounting cluster BEFORE slurmctld starts, so the DB has TRES.
# We set up a small multi-tenant fixture: two groups (Slurm accounts), each with
# two users, plus the root operator. The obold MVP is single-budget (per-account
# budget resolution is the hierarchy work, #17/#18), so all of these gate against
# the one budget for now; the fixture exercises real multi-user/multi-account
# identity plumbing and lets the multi-tenant test assert conservation across the
# mix. When hierarchy lands, these accounts become distinct budgets.
log "registering cluster + multi-tenant accounts/users"
sacctmgr -i add cluster obol-test >/dev/null 2>&1 || true

# Two groups.
sacctmgr -i add account lab_smith Description="Smith Lab" Organization=obol >/dev/null 2>&1 || true
sacctmgr -i add account lab_jones Description="Jones Lab" Organization=obol >/dev/null 2>&1 || true
# Keep a plain "lab" account too for the simple single-tenant tests.
sacctmgr -i add account lab Description="default" Organization=obol >/dev/null 2>&1 || true

# Create OS users and attach them to accounts (two users per group).
for u in alice bob carol dave; do
  id -u "$u" >/dev/null 2>&1 || useradd -m "$u" 2>/dev/null || true
done
sacctmgr -i add user alice Account=lab_smith DefaultAccount=lab_smith >/dev/null 2>&1 || true
sacctmgr -i add user bob   Account=lab_smith DefaultAccount=lab_smith >/dev/null 2>&1 || true
sacctmgr -i add user carol Account=lab_jones DefaultAccount=lab_jones >/dev/null 2>&1 || true
sacctmgr -i add user dave  Account=lab_jones DefaultAccount=lab_jones >/dev/null 2>&1 || true
sacctmgr -i add user root  Account=lab DefaultAccount=lab >/dev/null 2>&1 || true

# --- obold (must be up before slurmctld so the first gate can reach it) ---
log "starting obold"
/usr/local/bin/obold -socket /run/obol/obold.sock -state-dir /var/lib/obol \
  -create -balance "${OBOL_BALANCE:-100000}" -rate "${OBOL_RATE:-1}" -sync=false \
  >/var/log/obold.log 2>&1 &
for i in $(seq 1 20); do
  [ -S /run/obol/obold.sock ] && break
  sleep 0.25
done
chmod 666 /run/obol/obold.sock 2>/dev/null || true

# --- slurmctld + slurmd ---
log "starting slurmctld"
/usr/sbin/slurmctld

# slurmd (22.05) initializes the cgroup/v2 plugin and needs the systemd scope
# directory to exist even with IgnoreSystemd=yes. Inside the container's private
# cgroup namespace the cgroup2 fs is writable, so pre-create it; without this
# slurmd fails to initialize and the node never leaves UNKNOWN.
mkdir -p /sys/fs/cgroup/system.slice/slurmstepd.scope 2>/dev/null || true
log "starting slurmd"
/usr/sbin/slurmd

# Wait for the controller to answer before declaring ready.
for i in $(seq 1 40); do
  sinfo >/dev/null 2>&1 && break
  sleep 0.5
done

log "ready"
# Keep the container alive; the harness drives it via docker exec.
touch /run/obol/ready
tail -f /var/log/slurm/slurmctld.log
