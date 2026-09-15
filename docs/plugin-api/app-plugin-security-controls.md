# App Plugin B1.5 Security Controls

## Status And Scope

Reviewed 2026-09-15 against canonical `phase1-integrated.md` sections 5.2-6.6,
10.3-10.4, and 11. Baseline:
`46f3a3d54c65d9079e11b41225275c51257a519c`.

This document records security requirements and decisions, not a compliance
claim. B1.5 initially used the approved exact-12 file scope, including the
narrow `service/security_verification.go` extension. The 429 spec-review fix
adds the parent-approved 13th path `router/app_plugin_router_test.go` and 14th
path `router/api-router.go`; the correction itself touches only four files,
listed below. The parent observed exact-14
canonical compile-symbol RED before production code. Exact-14 native race GREEN
and the added contention, credential-proof, and migration tests have run.
The baseline-11 regression gate and real three-database matrix passed as recorded
below, including targeted revalidation of the last code changes. Independent
specification review, focused 429 re-review, and subsequent quality review
approved the implementation. No deployment has been performed. The verified
source commit is recorded externally after canonical `commit_task` succeeds.

## Reviewed Sources

The [official ASVS project page](https://owasp.org/www-project-application-security-verification-standard/)
identifies **5.0.0** as the latest stable release. Requirement references below
use the immutable `v5.0.0` tag, not the development branch.

The following living Cheat Sheet Series pages were read on 2026-09-15. They do
not publish an ASVS-style semantic version; the review date identifies the
consultation, not a frozen upstream revision.

- [Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html):
  sensitive account separation, TLS, reauthentication for sensitive operations,
  safe failure messages, throttling, and authentication event logging.
- [Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html):
  opaque identifiers, CSPRNG entropy, backend validation, expiration, logout,
  privilege changes, cookie boundaries, and exclusion of session secrets from logs.
- [CSRF Prevention](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html):
  existing protection reuse, exact Origin validation, rejecting simple content
  types, custom headers, and the limits of SameSite.
- [OAuth 2.0](https://cheatsheetseries.owasp.org/cheatsheets/OAuth2_Cheat_Sheet.html):
  exact registered redirects, S256 PKCE, transaction/session-bound state and
  nonce, confidential backchannel authentication, and resource/action restriction.

B1.5 is a dedicated App Plugin protocol, not an implementation or certification
of the complete OAuth/OIDC specifications.

## Applicable ASVS Controls

| Versioned requirement | Application to B1.5 | Verification status |
| --- | --- | --- |
| v5.0.0-6.1.1, 6.3.1 | Document and enforce throttling for authorize, exchange, and credential operations without malicious account lockout. | Global/IP/user 429 contract race tests pass; deployment limits unverified |
| v5.0.0-6.1.3, 6.3.4 | Document dashboard-session, PAT, and AppService boundaries; neither surface may bypass restrictions. | Local race tests pass |
| v5.0.0-7.1.1, 7.1.3 | Document upstream and App session lifetimes and federated logout/version invalidation. | Local race tests pass; downstream lifetime contract below |
| v5.0.0-7.2.1, 7.2.2, 7.2.3 | Validate sessions in the backend; use newly issued opaque identifiers with at least 128 bits of entropy. | 256-bit CSPRNG IDs; local race tests pass |
| v5.0.0-7.3.1, 7.3.2 | Enforce documented inactivity and maximum-lifetime decisions server-side. | Upstream bound tested; downstream idle and absolute-lifetime risk remain |
| v5.0.0-7.4.1, 7.4.2 | Reject sessions after logout, expiry, user disable/delete, or version change. | Authoritative DB invalidation tests pass |
| v5.0.0-7.5.3 | Further authentication before highly sensitive credential issuance/rotation. | Approved installation/action-bound scopes; ceremony, mismatch and stale-session tests pass |
| v5.0.0-7.6.1, 7.6.2 | Coordinate relying-party lifetime with upstream authentication and require explicit launch interaction. | Server tests pass; browser interaction is downstream work |
| v5.0.0-8.1.1, 8.1.2 | Document function, object, and field authorization. | Boundaries specified below |
| v5.0.0-8.2.1, 8.2.2, 8.2.3 | Root-only credentials; exact installation/app/subject ownership; no caller-owned entitlements or redirects. | Local race tests pass |
| v5.0.0-8.3.1, 8.3.2, 8.3.3 | Trusted, current authorization from the originating user, never intermediary service privilege. | Current DB role/user override and immutable host-policy intersection tested |
| v5.0.0-3.4.2, 3.5.1, 3.5.2, 3.5.3 | Exact trusted origins, JSON/custom-header browser writes, no state-changing GET. | Server boundary tests pass; CORS/proxy verification pending |
| v5.0.0-3.4.5 | Prevent code/challenge leakage through referrers. | Host no-referrer tested; downstream callback/proxy validation pending |
| v5.0.0-10.1.1, 10.1.2 | Keep service secrets/verifier on the backend; bind authorization to the exact user session and transaction. | Local race tests pass; downstream secret handling not covered |
| v5.0.0-10.3.1, 10.3.2, 10.3.3 | Audience/service restriction, exact scopes and entitlements, stable issuer plus subject. | Local race tests pass |
| v5.0.0-10.4.1, 10.4.3, 10.4.6 | Exact pre-registered callback, 60-second code lifetime, S256-only PKCE. | Local race tests pass |
| v5.0.0-10.4.2 | One logical code consumption, with narrowly authenticated idempotent response replay. | Approved valid second-use revocation implemented; invalid guesses cannot revoke; frozen replay cannot reactivate |
| v5.0.0-10.4.10, 10.4.11 | Authenticate backchannel requests and grant only host-approved scopes. | Local race tests pass |

Pinned source chapters:
[V3](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x12-V3-Web-Frontend-Security.md),
[V6](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x15-V6-Authentication.md),
[V7](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x16-V7-Session-Management.md),
[V8](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x17-V8-Authorization.md),
[V10](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x19-V10-OAuth-and-OIDC.md).

## Required Route Boundaries

| Route | Required authority and boundary |
| --- | --- |
| `POST /api/app_plugins/:key/authorize` | Current dashboard session with authoritative user/session checks, trusted Origin, JSON body, and Idempotency-Key. PAT identity without a session is insufficient. |
| `POST /api/app_plugins/installations/:id/service-credentials` | Current root authority, trusted browser request, and sensitive-operation reauthentication. Plaintext only in the successful issuance response. |
| `POST /api/app_plugins/installations/:id/service-credentials/rotate` | Same root boundary; a new immutable credential version, older versions capped at ten minutes without extending shorter expiry. |
| `DELETE /api/app_plugins/installations/:id/service-credentials/:credential_id` | Root-only immediate revocation; must remain available when the App is disabled. |
| `PATCH /api/app_plugins/installations` | Existing management permission, root-sensitive restrictions, revision CAS, enabled status-only guard, and ownership locking retained. All enable prerequisites must hold for the locked revision. |
| `POST /internal/apps/v1/launch-codes/exchange` | TLS and current AppService credential bound to the installation and scopes; exact exchange payload and one logical consumption. |
| `POST /internal/apps/v1/sessions/introspect` | TLS and AppService; current user/session/App/scope/entitlement state, exact ownership, no positive authorization cache. |
| `POST /internal/apps/v1/sessions/revoke` | TLS and AppService, exact ownership and Idempotency-Key. Only the selected App session is revoked. |

AppService must never populate dashboard identity or reach dashboard/admin,
relay, or other unregistered internal endpoints. B1.6 task/grant routes are not
part of B1.5 and must not be implicitly enabled by a prefix-based allowlist.
Client-provided forwarding headers alone are not evidence of TLS.

## Replay And Secret Storage

Approved and implemented authorize replay strategy:

- Require an explicitly configured shared deployment secret. The process-random
  defaults in `common.CryptoSecret` and `common.SessionSecret` are not sufficient
  for multi-replica or restart-stable replay.
- Derive a purpose-specific HMAC-SHA256 key with domain separation, following
  the existing `authSigningKey` pattern without sharing its token purpose.
- Derive the launch code from a persisted random salt and a canonical,
  unambiguous binding of the principal/session versions, installation/generation,
  callback, surface, transaction, state/nonce hashes, and PKCE challenge.
- Persist only the code hash and non-secret binding data, never plaintext code,
  a launch URL, or encrypted/recoverable code inside a frozen response.
- Reconstruct only while the stored code is unconsumed and unexpired, verify
  its hash, and fail closed on key mismatch. Key changes must not silently mint
  a replacement code or extend the original 60-second lifetime.
- Freeze only the secret-free exchange identity response for five minutes,
  scoped to the service, exchange request ID, and canonical request hash.
- Freeze session-revoke results without secrets, including the original
  `already_revoked` value. A new key may report `already_revoked=true`.

Configuration reads `APP_PLUGIN_LAUNCH_SECRET`, or explicitly configured
`CRYPTO_SECRET` when the dedicated variable is absent, and requires at least
32 bytes. Replicas must share that configured key. Tests inject a test-only key;
no deployment secret was read or generated. Rotation invalidates pending code
reconstruction without silently replacing it.

A distinct valid exchange request ID using an already-consumed code commits
revocation of the related App session before returning `launch_code_replayed`.
Service identity, installation/generation/callback, surface/transaction,
state/nonce and PKCE are validated before this side effect. A same-logical
five-minute frozen response can reference that revoked session but cannot make
it active again. This is the approved interpretation of v5.0.0-10.4.2.

Service credentials must be independently CSPRNG-generated, at least 256 bits,
opaque, and persisted only as a hash plus ID/version/status/expiry and scoped
binding metadata. Rotation is not a retrieval API. Lists, logs, audit records,
and documentation must not contain usable credentials, codes, verifiers,
cookies, state, or nonce values.

Credential create/rotate use dedicated `app_plugin.credential.create` and
`app_plugin.credential.rotate` proof scopes with installation ID and action
binding. Existing password/MFA/passkey/OAuth policy supplies the factors;
PAT/channel proofs cannot substitute. Proof consumption commits before issuance
and cannot be restored by an action failure. Current root/session authority is
also checked inside the credential transaction. Credential audit events contain
only fixed action and target metadata, never request/response bodies.

## Session And Entitlement Rules

App sessions bind the issuer/subject and dashboard session ID, user auth version,
session version, installation, and manifest generation. Every new authorization
decision must read current database state. Existing cache-based
`ValidateLoginSession` or `authz.Can` results alone do not satisfy this boundary.

Disabled Apps admit no new navigation, launch, or grant. An existing session may
only describe read-only access; downstream code must restrict it to completed,
owned data. An `active` boolean alone is not authorization for a write.
Required entitlements are exact `resource -> actions[]` checks against current
host policy intersected with current user authority.

The canonical downstream cookie contract is a 30-minute idle timeout capped by
the upstream session, with no independent absolute lifetime. B1.5 must not
extend upstream expiry. ASVS v5.0.0-7.3.2 still requires a documented risk
decision for deployments whose upstream session has no absolute maximum.

## Named Test Mapping

| Test | Contract |
| --- | --- |
| `TestAuthorizeReturnsRegisteredOneTimeLaunchURL` | Exact HTTP body, registered callback, session requirement, Origin and no-store |
| `TestAuthorizeBindsOriginCallbackPKCEAndSessionVersions` | S256, state/nonce, current DB versions/status |
| `TestAuthorizeBindsSurfaceTransactionAndRegisteredCallback` | Surface, parent origin, transaction, callback and generation binding |
| `TestAuthorizeIdempotencyDoesNotMintSecondCode` | Replica replay, hash-only persistence, key changes, expiry and consumption |
| `TestExchangeHasOneLogicalWinnerAndFiveMinuteReplay` | Concurrent consumers, request hash conflict and replay expiry |
| `TestIntrospectionReflectsLogoutDisableAndRevoke` | Current invalidation and disabled read-only state |
| `TestIntrospectionReturnsVersionedHostEntitlements` | Permission shrinkage, exact entitlements and immutable policy versions |
| `TestSessionRevokeContractAndOwnership` | Ownership concealment, original-response replay and isolated revocation |
| `TestServiceCredentialPlaintextIsReturnedOnceAndStoredHashed` | Hash-only persistence, metadata redaction, migration and rollback |
| `TestServiceCredentialRotationOverlapsForTenMinutes` | New versions, bounded overlap and shorter expiry preservation |
| `TestServiceCredentialEmergencyRevokeIsImmediate` | Immediate invalidation and terminal installation revoke |
| `TestServiceCredentialVersionIsEnforced` | TLS, current version/status and no credential cache |
| `TestAppEnableRequiresAllPrerequisites` | Locked-revision prerequisites, credential success/proof/redaction/no-store, and retained management boundaries |
| `TestServiceIdentityCannotReachDashboardOrAdmin` | Dashboard/admin/relay separation and internal route allowlist |

## Validation Evidence

- Parent-observed exact-14 RED: `/tmp/newapi-b15-exact14-red.log`, archived at
  `reports/rc36-phase1/b1.5-exact14-red.log`; SHA-256
  `79efcedf335df7335abe3141275abe6ebc66e04d2e9ac8646865f2ee1ae2b091`.
- Interrupted test rerun: `/tmp/newapi-b15-hang-repro.log`, PASS. No SQLite
  transaction/global-DB deadlock reproduced.
- Exact-14 canonical native race GREEN: `/tmp/newapi-b15-green-iteration2.log`,
  all 14 top-level tests PASS.
- Added controlled ownership contention, current DB permission overrides,
  valid/invalid second use, and legacy credential migration/rollback tests:
  model/service PASS in `/tmp/newapi-b15-extended-iteration1.log`.
- That run caught missing credential audit events. The controller now records
  them via the existing safe audit API; credential ceremony/redaction and
  enable tests PASS in `/tmp/newapi-b15-extended-iteration2.log`.
- Native race uses the canonical `run_go_race`, `-p=1`, and bounded test timeout
  with shell-local `DEVELOPER_DIR=/Library/Developer/CommandLineTools`,
  `SDKROOT="$(xcrun --show-sdk-path)"`, and `CC="$(xcrun --find clang)"`.
  No global Xcode selection or license settings changed.
- `/tmp/newapi-b15-exact25-race.log`: exact-14 and the four controller/router
  baseline tests PASS. Seven B1.3 tests failed only their required runtime
  metadata assertions because that invocation omitted the runner environment.
  `/tmp/newapi-b15-baseline7-race-metadata.log` reran precisely those seven with
  SQLite dialect, driver, and actual version metadata: PASS. The original
  aggregate log is retained as a nonzero run, not relabeled GREEN.
- `/tmp/newapi-b15-exact25-db-matrix.log`: 25/25 named tests and five packages
  PASS on each of SQLite 3.50.4, MySQL 5.7.44 (`linux/amd64`), and PostgreSQL
  15.19 (`linux/arm64`). This includes fresh/repeated migration, frozen B1.4
  credential schema upgrade, preserved installation responses/claims, unique
  binding constraints, failed-rotation rollback, and controlled contention.
  Existing dialect-specific baseline subtests skip only on inapplicable
  engines; these skips do not stand in for the real MySQL/PostgreSQL runs.
- Last code changes, after that full matrix: credential controller locks now
  follow ownership -> installation -> user/session; installation-route
  `no-store` is applied before authentication. These were semantic changes,
  followed by gofmt, not formatting-only changes.
- `/tmp/newapi-b15-controller-final-race.log` and
  `/tmp/newapi-b15-controller-final-db-matrix.log` revalidated only the affected
  six tests: the two B1.5 controller tests plus the four B1.4 tests.
  Native race PASS; every database 6/6 PASS and matrix exit 0. Unchanged
  model/service/middleware results remain from the full exact-25 run.
- `CGO_ENABLED=0 go vet -p=1 ./model ./service ./middleware ./controller ./router`:
  PASS, `/tmp/newapi-b15-vet.log`, exit 0. `git diff --check` passed.
- Both matrices ran sequentially via `scripts/test_app_plugin_db_matrix.sh --
  env CGO_ENABLED=0 go test -p=1 ... -count=1 -v -timeout=5m` with exact anchored
  named-test selections, `NEW_API_TEST_ROOT` set to this worktree, and
  `DOCKER_CONTEXT=colima-newapi-b15-check-20260915`. Driver versions were
  `github.com/glebarez/sqlite@v1.11.0`, `gorm.io/driver/mysql@v1.5.7`, and
  `gorm.io/driver/postgres@v1.5.9`. Pinned images:
  MySQL `sha256:4bc6bc963e6d8443453676cae56536f4b8156d78bae03c0145cbe47c2aad73bb`;
  PostgreSQL `sha256:9b1d34adbce1dd07ee6e94b4a2cf698884b89bd44a6c9c12f5da8f3acbfe4957`.
- Cleanup verified: zero containers and only `bridge/host/none` networks in the
  approved context. The Colima profile remains available for parent reviewers.
  No unrelated Docker resources were modified. Coverage-percentage collection
  was not requested and was skipped; no coverage or full-ASVS-compliance claim.

### Final Evidence Digests

All paths below are under `/tmp/`; SHA-256:

| Log | SHA-256 |
| --- | --- |
| `newapi-b15-exact25-db-matrix.log` | `ae46f417dbd7e259f06060afe6d95f50875e1dac5071d6dc19f96028c7eaf075` |
| `newapi-b15-controller-final-db-matrix.log` | `110d169fc2cea0fdc7d365f81a48e0fce6e3bed602f821e2defa099421ee1aac` |
| `newapi-b15-controller-final-race.log` | `7f1a5f403af3ccbec67a05485283d19dc24ed8d4b2ceac60827223f3df8674bc` |
| `newapi-b15-exact25-race.log` | `5d67df82fe871835bec85833c8ea89ecdf3a8afea150481d9f86ea33ad57bdcc` |
| `newapi-b15-baseline7-race-metadata.log` | `b129f312636bbc4c07cfc86cf14fdd3346042360bdbc8100d81daa0c7b12b200` |
| `newapi-b15-vet.log` | `1a5a3d202bd92a84b923d872ec19e6e4c49c0b7d743274c26373353c9c424a96` |
| `newapi-b15-ratelimit-red.log` | `aa1f9e4756b458ad66cd0c93cad57a0712f842e56e4805c61f8413a997f012fa` |
| `newapi-b15-ratelimit-green.log` | `0979ec5c1210e866a3985ac337b55f18f3dcc286bd9ec00d7a5016b324580136` |

## 429 Spec Review Fix

The shared limiters abort with 429 and `Retry-After` but no body. The existing
normalizer committed that response both before authentication (empty JSON)
and after authentication (`id` present). Moreover, `SetApiRouter` installs
`GlobalAPIRateLimit` before route-local middleware, including the inherited
`/internal/apps/v1` sibling group. Fixing only route-local responses was insufficient.

The correction recognizes empty-body 429 before the authentication/JSON guards,
returns the canonical section 10.4 envelope with `code=rate_limited`,
`message="Too many app plugin requests"`, `field_errors=[]`, `retryable=true`,
and a server-generated request ID, and preserves buffered headers including
`Retry-After`. It ensures `no-store` and `no-referrer` even when the global
limiter prevents route-local security headers from running.

The new parent boundary precedes GlobalAPI and matches the HTTP method plus
Gin's registered `FullPath()` against exactly the 11 current App route entries:
navigation, three installation operations, authorize, credential create/rotate/
revoke, and internal exchange/introspect/revoke. It does not use prefix matching
or mutate Gin's internal handler chain. Non-App routes, non-429 responses,
existing canonical errors (including nonempty 429), and global limiter semantics
are unchanged.

| Correction file | Incremental change |
| --- | --- |
| `router/app-plugin-router.go` | Exact App API boundary and empty-body 429 normalization; original B1.5 route wiring retained |
| `router/api-router.go` | One boundary registration before `GlobalAPIRateLimit` |
| `router/app_plugin_router_test.go` | Subtests under the existing dashboard canonical name and a request fixture; second canonical test unchanged |
| `docs/plugin-api/app-plugin-security-controls.md` | Scope and correction evidence only; previous evidence digests retained |

Both runs used the trusted canonical helpers and shell-local compiler settings
documented above, with exact source discovery from
`router/app_plugin_router_test.go` for these two top-level tests:

```text
TestAppPluginDashboardRoutesAndRedaction
TestAppPluginPatchRequiresRevisionAndRootForSensitiveChanges
```

```bash
run_go_race -p=1 ./router \
  -run '^(TestAppPluginDashboardRoutesAndRedaction|TestAppPluginPatchRequiresRevisionAndRootForSensitiveChanges)$' \
  -count=1 -v -timeout=3m
```

- RED: `/tmp/newapi-b15-ratelimit-red.log`, captured by `expect_red` with
  `B1.5 429 canonical envelope missing` and verified by `assert_go_tests_ran`.
  All 20 App limiter cases failed on missing envelopes: six IP, three
  authenticated-user, and 11 actual `SetApiRouter` GlobalAPI cases.
  Logs show `second_status=429 body_bytes=0`. The other canonical test passed.
- GREEN: `/tmp/newapi-b15-ratelimit-green.log`, native race exit 0, both
  canonical tests PASS; the 20 App cases report `body_bytes=164` and assert the
  complete envelope, headers, fresh request IDs, and rejection of client IDs.
  User-limit cases omit RequestId middleware to exercise server-ID fallback.
- Negative controls retain empty global 429 on `/api/models` and `/api/user/self`.
  Canonical 429/401, empty 500, and legacy business-error bodies/headers pass
  unchanged. An initial negative control incorrectly assumed `SetApiRouter`
  registers `/api/infinite-canvas`; it was corrected before the recorded RED.
- Limits are 1 per 60 seconds with independent direct client IPs and user IDs,
  no sleeps or Redis. Local cases isolate Critical/UserCritical via
  `registerAppPluginRoutes`; global cases use real `SetApiRouter` without a
  bypass. Tests reuse the existing fixture, adding only five users, and restore
  limiter settings and the feature flag.
- No DB/schema/dependency behavior changed; the parent's existing full race and
  three-database evidence is retained, not rerun or relabeled. No new repository
  files, staging, or commits. The original review-input artifact/hash is not
  rewritten by this correction.

## Review And Final Verification

The initial specification review identified only the empty 429 response.
Independent focused re-review approved its correction, and subsequent
independent quality review approved all 14 paths without required fixes.
The parent additionally ran all 25 canonical tests with native race after the
initial implementation freeze, then reran both affected router tests after
the 429 correction. Both invocations exited 0; logs are archived externally:

- `reports/rc36-phase1/b1.5-parent-exact25-race.log`, SHA-256
  `516e730f802b02dbbb2dae1f978fa1b3e918240857d6546b1076f3c637e1c56f`.
- `reports/rc36-phase1/b1.5-parent-ratelimit-race.log`, SHA-256
  `a6ead182914a864b77fb5a6f2b28a0a87ffa733915a1a77f3d5b8db00bf283e2`.

All 14 file hashes matched the frozen quality-review input. Only this evidence
document was updated afterward to record the approved reviews. The approved
B1.5 exact-14 paths, relative to this worktree, are:

```text
model/app_plugin_launch.go
model/app_plugin_launch_test.go
model/main.go
service/app_plugin_auth.go
service/app_plugin_auth_test.go
middleware/app_service_auth.go
middleware/app_service_auth_test.go
controller/app_plugin.go
controller/app_plugin_test.go
router/app-plugin-router.go
docs/plugin-api/app-plugin-security-controls.md
service/security_verification.go
router/app_plugin_router_test.go
router/api-router.go
```

`controller/token.go` was not edited, formatted, or staged. Its unchanged
SHA-256 is `318740943abbaee750ffa55d4423d4db8d3609aa082b3d78511a452580b72091`.
Staging/commit uses canonical `commit_task` with the exact-14 allowlist above.
The resulting verified SHA belongs in the external evidence record, not a
self-referential prediction here. Default App Plugin flags remain false;
B1.6 was not implemented in this task.

## Deployment Risks

- TLS termination, trusted proxy configuration, rate limits, secret provisioning,
  no-store/referrer behavior, and callback query-log suppression require actual
  deployment verification. No forwarding-header trust should be assumed.
- Network probes enforce HTTPS, exact host policy, public IP resolution at
  dial time, no redirects, bounded timeouts, and no ambient proxy or insecure
  TLS bypass. A mocked successful probe is not deployment reachability evidence.
- The canonical same-host/different-port deployment is not cookie isolation.
  ASVS v5.0.0-3.5.4 recommends distinct hostnames; the residual risk needs review
  even with scoped cookie paths, SameSite, and exact origins.
- Browser iframe/postMessage/CSP controls and downstream completed-own-data
  restrictions require B1.6/Seedance verification; these tests do not prove them.
- New tables are confined to the primary database; this task does not change
  the separately configured log database schema. Audit transport availability
  and retention still require deployment verification.
