# Obol documentation

Start with what you're trying to do.

### 🔍 Evaluating Obol — is this for us?
- [**Concepts**](concepts.md) — how it works, from one worked example: reserve →
  admit → settle → refund, balances, costing, windows, burst, and **how Obol
  relates to Slurm accounting / QOS / fair-share**.
- [**Architecture**](SEAM_DESIGN.md) — *why* it's a sidecar daemon: the
  controller-lock constraint, the three-tier latency model, the `admin_comment`
  correlation token, partition policy, and failure behavior.
- [Project overview & maturity](../README.md) — what's built, the compatibility
  policy, and the feature list.

### 🚀 Trying Obol — see it run
- [Quickstart](../README.md#quickstart) — config → daemon → shim → `sbatch`.
- [`test/docker/`](../test/docker/) — a complete, working single-node Slurm +
  Obol deployment you can build and run; the reference install.

### 🛠️ Deploying Obol — get it into production
- [**Installation**](installation.md) — binaries, running the daemon, the flag
  reference, a systemd unit, socket permissions, verify.
- [**Configuration**](configuration.md) — the full `obold.json` schema: accounts,
  rates, windows, burst, node-type pricing, admins, validation.
- [**Slurm integration**](INTEGRATION.md) — wiring the shim + prolog/jobcomp, and
  the test tiers (unit / Docker / multi-generation / ParallelCluster) with what
  each proves.

### ⚙️ Operating Obol — run it safely
- [**Operations & recovery**](operations.md) — durability & backup/restore,
  recovery after failure, the orphan janitors (`obol reconcile`), fail-open vs.
  fail-closed, monitoring, upgrades/rollback, and the admin model.
- [**CLI reference**](cli-reference.md) — every `obol` verb, its flags, and exit
  codes.

### 🤝 Contributing
- [Contributing guide](../CONTRIBUTING.md) — workflow, tests, and the design
  invariants that must never regress.
- [Changelog](../CHANGELOG.md) — what changed, per release.

---

## The whole map

| Doc | What it answers |
|-----|-----------------|
| [`concepts.md`](concepts.md) | What is Obol and how does the money model work? |
| [`SEAM_DESIGN.md`](SEAM_DESIGN.md) | Why is the architecture shaped this way? |
| [`installation.md`](installation.md) | How do I get it running? |
| [`configuration.md`](configuration.md) | What goes in the config file? |
| [`INTEGRATION.md`](INTEGRATION.md) | How does it attach to Slurm, and what's tested? |
| [`operations.md`](operations.md) | How do I run it safely — backup, recovery, upgrades? |
| [`cli-reference.md`](cli-reference.md) | What does each `obol` command do? |
