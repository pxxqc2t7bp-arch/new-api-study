# Study deployment session policy

The `study/*` branches add a configurable sliding idle timeout and a browser
presence stream to the upstream New API authentication model.

Production settings:

```env
SESSION_COOKIE_SECURE=true
SESSION_COOKIE_TRUSTED_URL=https://study.chenxy.online:10443
USER_SESSION_IDLE_TIMEOUT_SECONDS=1800
```

`GET /api/user/auth/presence` authenticates with the existing HttpOnly refresh
cookie. While its SSE connection remains alive, it advances the authoritative
database session expiry. The normal refresh endpoint also advances the expiry.
Closing the page or losing the connection stops renewal; the session expires
after the configured idle timeout.

The refresh cookie remains a long-lived opaque carrier. It cannot authorize a
request after the database session expires or is revoked.

## Upgrade

1. Fetch `upstream`.
2. Create a new `study/<upstream-version>-sso` branch from the selected upstream
   tag or commit.
3. Rebase or cherry-pick the Study session commits.
4. Run backend and frontend CI.
5. Publish the image and deploy only by immutable digest.
6. Record upstream commit, fork commit/tree, image digest, and deployment file
   SHA-256 values in the production approval pack.

Do not deploy a moving `latest` tag. A patch conflict or failed authentication
test blocks the upgrade.
