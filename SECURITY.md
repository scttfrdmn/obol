# Security Policy

This software gates access to compute spend. A correctness bug in the money path is a security
issue. Report privately via GitHub Security Advisories ("Report a vulnerability") rather than a
public issue. Please include the smallest reproducing case and which of the five invariants you
believe is violated (exact integer conservation, gate atomicity, pure-`(state, command, now)`
transitions, WAL-holds-only-committed-transitions, or burst-as-permission). The invariants are
spelled out in full in `CLAUDE.md`.
