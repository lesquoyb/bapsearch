# Authentik OIDC Configuration

This project expects Authentik to act as the OpenID Connect provider for `oauth2-proxy`.

## Target values

Use these exact values when creating the Authentik application:

- Application name: `bap-search`
- Application slug: `bap-search`
- Provider type: `OAuth2/OpenID Provider`
- Redirect URI: `http://localhost:8080/oauth2/callback`
- Launch URL: `http://localhost:8080/`
- Issuer URL used by `oauth2-proxy`: `http://authentik-server:9000/application/o/bap-search/`

## Initial setup

1. Start the stack with Authentik enabled.
2. Open `http://localhost:9000/if/flow/initial-setup/`.
3. Complete the bootstrap flow and log in as `akadmin`.

## Create the provider

1. Open `Applications`.
2. Open `Providers`.
3. Create a new `OAuth2/OpenID Provider`.
4. Set `Name` to `bap-search-provider`.
5. Set `Authorization flow` to the default authentication flow.
6. Set `Client type` to `Confidential`.
7. Set `Redirect URIs/Origins` to `http://localhost:8080/oauth2/callback`.
8. Keep the default signing key unless you already manage certificates in Authentik.
9. Add scopes:
   - `openid`
   - `profile`
   - `email`
10. Save the provider.

After saving, copy these two values into your environment file:

- `Client ID` -> `OAUTH2_PROXY_CLIENT_ID`
- `Client Secret` -> `OAUTH2_PROXY_CLIENT_SECRET`

## Create the application

1. Open `Applications`.
2. Create a new application.
3. Set `Name` to `bap-search`.
4. Set `Slug` to `bap-search`.
5. Set `Provider` to `bap-search-provider`.
6. Set `Launch URL` to `http://localhost:8080/`.
7. Save.

The slug matters because `oauth2-proxy` is configured to use this issuer path:

```text
http://authentik-server:9000/application/o/bap-search/
```

## Assign access

1. Create a test user in Authentik if you do not want to use `akadmin` for daily access.
2. Grant the user or group access to the `bap-search` application.
3. Log out and test login via `http://localhost:8080`.

## Environment values

These values are already aligned with the Compose files and should match your Authentik app:

```env
OAUTH2_PROXY_PROVIDER=oidc
OAUTH2_PROXY_OIDC_ISSUER_URL=http://authentik-server:9000/application/o/bap-search/
OAUTH2_PROXY_SCOPE=openid profile email
OAUTH2_PROXY_CODE_CHALLENGE_METHOD=S256
OAUTH2_PROXY_USER_ID_CLAIM=sub
OAUTH2_PROXY_EMAIL_CLAIM=email
OAUTH2_PROXY_REDIRECT_URL=http://localhost:8080/oauth2/callback
```

## Secure mode

For production-style deployment, use [docker/docker-compose.auth-secure.yml](../docker/docker-compose.auth-secure.yml). This removes the direct host port publishing from the backend so users can only reach the app through `auth-proxy`.