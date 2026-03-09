"""No-op OAuth provider for local MCP server.

Implements the MCP SDK's OAuthAuthorizationServerProvider interface with
auto-approve behaviour so Claude Code's Authenticate flow succeeds without
requiring real credentials.
"""

from __future__ import annotations

import secrets
import time
from urllib.parse import urlencode

from mcp.server.auth.provider import (
    AccessToken,
    AuthorizationCode,
    AuthorizationParams,
    OAuthAuthorizationServerProvider,
    OAuthToken,
    TokenVerifier,
)
from mcp.shared.auth import OAuthClientInformationFull


class NoopOAuthProvider(OAuthAuthorizationServerProvider[AuthorizationCode, str, AccessToken]):
    """OAuth provider that auto-approves all requests."""

    def __init__(self) -> None:
        self._clients: dict[str, OAuthClientInformationFull] = {}
        self._codes: dict[str, AuthorizationCode] = {}
        self._tokens: dict[str, AccessToken] = {}

    # -- Client registration --------------------------------------------------

    async def get_client(self, client_id: str) -> OAuthClientInformationFull | None:
        return self._clients.get(client_id)

    async def register_client(self, client_info: OAuthClientInformationFull) -> None:
        self._clients[client_info.client_id] = client_info

    # -- Authorization ---------------------------------------------------------

    async def authorize(
        self, client: OAuthClientInformationFull, params: AuthorizationParams
    ) -> str:
        code = secrets.token_urlsafe(32)
        self._codes[code] = AuthorizationCode(
            code=code,
            scopes=params.scopes or [],
            expires_at=time.time() + 300,
            client_id=client.client_id,
            code_challenge=params.code_challenge,
            redirect_uri=params.redirect_uri,
            redirect_uri_provided_explicitly=params.redirect_uri_provided_explicitly,
            resource=params.resource,
        )
        query = urlencode({"code": code, **({"state": params.state} if params.state else {})})
        return f"{params.redirect_uri}?{query}"

    # -- Token exchange --------------------------------------------------------

    async def load_authorization_code(
        self, client: OAuthClientInformationFull, authorization_code: str
    ) -> AuthorizationCode | None:
        return self._codes.get(authorization_code)

    async def exchange_authorization_code(
        self, client: OAuthClientInformationFull, authorization_code: AuthorizationCode
    ) -> OAuthToken:
        self._codes.pop(authorization_code.code, None)
        token = secrets.token_urlsafe(32)
        self._tokens[token] = AccessToken(
            token=token,
            client_id=client.client_id,
            scopes=authorization_code.scopes,
            resource=authorization_code.resource,
        )
        return OAuthToken(
            access_token=token,
            token_type="Bearer",
            expires_in=3600,
            scope=" ".join(authorization_code.scopes) if authorization_code.scopes else None,
        )

    # -- Refresh / revoke (unused but required) --------------------------------

    async def load_refresh_token(
        self, client: OAuthClientInformationFull, refresh_token: str
    ) -> str | None:
        return None

    async def exchange_refresh_token(
        self, client: OAuthClientInformationFull, refresh_token: str, scopes: list[str]
    ) -> OAuthToken:
        msg = "Refresh not supported"
        raise NotImplementedError(msg)

    async def load_access_token(self, token: str) -> AccessToken | None:
        return self._tokens.get(token)

    async def revoke_token(self, token: AccessToken | str) -> None:
        if isinstance(token, AccessToken):
            self._tokens.pop(token.token, None)
        else:
            self._tokens.pop(token, None)


class NoopTokenVerifier(TokenVerifier):
    """Token verifier that delegates to the NoopOAuthProvider's token store."""

    def __init__(self, provider: NoopOAuthProvider) -> None:
        self._provider = provider

    async def verify_token(self, token: str) -> AccessToken | None:
        return await self._provider.load_access_token(token)
