# New API Sub2API Sync

Chrome MV3 companion for the New API upstream orchestrator.

## Build

```bash
npm run build
```

Load `dist/` as an unpacked extension, open its options page, enter the New API
URL and the one-time pairing code created by a Root administrator.

The extension reads Sub2API login tokens only inside the matching site origin.
Tokens and cookies are never sent to New API. Existing API keys are represented
by SHA-256 fingerprints. A newly created managed key is sent once through the
enrollment result endpoint and is never included in later snapshots.
