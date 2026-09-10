# Study-only Task Plugin extensions

## Status and compatibility boundary

This document describes host behavior added on the
`study/rc36-phase1-20260909` line. It is not part of the stock
`v1.0.0-rc.36` Task Plugin API and is not a production deployment statement.

[`v1.md`](./v1.md), [`v1.d.ts`](./v1.d.ts), and
[`v1.schema.json`](./v1.schema.json) remain the authoritative pure rc.36
standard contract. The study host extensions below require a gateway containing
at least commit `0d980853c8a913c82679e6fac19e14a9daee043e`. A custom plugin may
still declare `apiVersion: 1` while using these extensions, but it is not
guaranteed to load or run on a stock rc.36 gateway.

## Deferred task submission

The study host accepts `SubmitIntent.execution: "deferred"`. This is a
host scheduling extension, not a new standard v1 execution mode.

For a deferred intent, the host reserves billing and persists a credential-free
task plus normalized request snapshot before any upstream call. A system-task
worker later claims the row and performs the upstream submission. It reloads
the user, API token, channel, base URL, and current channel credential at claim
time; credentials are not copied into `TaskPrivateData`.

Deferred execution does not accept request-scoped multipart bodies. Multipart
`FileReference.ref` values identify files owned by the current HTTP request and
are not durable across worker claims, and the host does not promote them
automatically. Work that must outlive the request needs a durable, host-issued
reference supported by that workflow. The Files API provides such persistence
for Batch input; it is not a general deferred-plugin file store.

## Files and Batch host APIs

The study host provides local OpenAI-compatible Files and Batch resources under
`/v1/files` and `/v1/batches`. Batch items re-enter normal gateway
authentication, routing, plugin, usage, logging, and billing. These APIs are
host services, not JavaScript globals or additions to the pure Task Plugin v1
schema. See [`../openai-files-batch.md`](../openai-files-batch.md) for their
endpoint and persistence contract.

## Encrypted stream recovery

Encrypted stream recovery and deferred Task Plugin execution are related only
as host-managed durability features. Stream recovery persists an encrypted
streaming request in its recovery store and tracks a `StreamExecution`; it does
not make Task Plugin multipart references durable and does not store secrets in
`TaskPluginSnapshot` or the deferred task request snapshot. Task polling and
settlement continue to use the task row and current channel credential.

## Study channel types

The study host reserves these additional channel type values:

| Value | Name | Purpose |
| --- | --- | --- |
| `62` | `VolcEngine3D` | VolcEngine 3D task channel |
| `63` | `Dummy` | Host test/dummy channel |

Plugins or configurations that depend on these values require the study host;
stock rc.36 does not provide this compatibility guarantee.

## Generations, upgrades, and rollback

A request pins the in-memory plugin generation selected when that request
starts. A persisted task records the producer plugin key, semantic version,
API version, and node-local generation as credential-free provenance. Those
fields are evidence and audit data; they are not a durable copy of plugin
source, and a generation number cannot be used to restore code after restart
or on another node.

Background dispatch and polling resolve the currently active plugin by the
persisted platform/plugin key. Therefore every activated upgrade or rollback
must continue to parse persisted request state and upstream responses for all
in-flight tasks created by versions that may still exist. Keep the key stable,
do not disable or delete its only compatible implementation while tasks are
in flight, and retain old-version fixtures for the full maximum task lifetime.
A rollback changes the active implementation for later dispatch and polling;
it does not rewrite the task's recorded producer version or generation.

Before changing the active version:

1. Replay built-in/vendor fixtures and any stored custom-plugin fixtures on the
   candidate version.
2. Exercise persisted tasks from each still-live producer version against the
   candidate polling and settlement path.
3. Confirm the candidate can read legacy rows that have no execution snapshot,
   plugin state, or poll-failure fields.
4. Roll back only to a version with the same backward-reading guarantees.

T0 measured the production uploaded-plugin override count as `0`. Consequently
there is no production override replay sample for this study, and none is
fabricated here. Compatibility evidence uses embedded vendor sources through
the existing fixture API plus synthetic, credential-free persisted task rows.
This evidence does not assert that the study extensions have been deployed to
production.
