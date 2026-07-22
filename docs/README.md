# Documentation

This directory contains the detailed guides and references for
`b2-share-broker`. Start with the root [README](../README.md) for the short
project overview and initial setup.

## Use B2 Share

- [User guide](user-guide.md) - upload from the browser or installed PWA,
  manage history, rename links, delete shares, and understand public-link
  behavior.
- [API reference](api.md) - authenticate with a browser session or bearer
  token and use every HTTP endpoint.

## Understand the System

- [Architecture](architecture.md) - runtime components, request routing,
  processing, content addressing, deduplication, and object lifecycle.
- [Configuration reference](configuration.md) - required environment
  variables, defaults, aliases, and validation rules.

## Deploy and Operate

- [Deployment guide](deployment.md) - configure OIDC, B2, PostgreSQL, Docker
  Compose, Helm, CloudNativePG, ingress, and NVIDIA NVENC.
- [Operations guide](operations.md) - migrations, health checks, backups,
  queue and media troubleshooting, B2 deletion, and live verification.
- [Helm chart reference](../chart/README.md) - chart installation, required
  Secrets, and all chart values.

## Develop

- [Development guide](development.md) - repository layout, local workflow,
  containerized Go checks, tests, and release artifacts.
- [Agent guidance](../AGENTS.md) - repository-specific constraints for coding
  agents.

## Historical Notes

Worklogs record completed efforts and production findings. They are useful
context, but the topic guides above are the source of truth for current
behavior.

- [2026-07-21 media pipeline hardening and embeds](worklogs/WORKLOG-2026-07-21-MEDIA-PIPELINE.md)
