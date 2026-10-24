"""Small synchronous, dependency-light client for the boxes-oss public API."""
from __future__ import annotations

import time
from typing import Any, Dict, Iterator, Optional
from urllib.parse import quote

import httpx


class BoxesError(Exception):
    """HTTP error returned by boxes-oss."""

    def __init__(self, status: int, code: str, message: str):
        self.status, self.code, self.message = status, code, message
        super().__init__(message)


def _wire(value: Any) -> Any:
    """Convert Python snake_case keys to the API's camelCase JSON keys."""
    if isinstance(value, dict):
        # Tag keys are user-defined (e.g. "env_name"), so the "tags" map is sent verbatim.
        return {("".join(part.title() if i else part for i, part in enumerate(key.split("_")))): (item if key == "tags" else _wire(item)) for key, item in value.items()}
    if isinstance(value, list):
        return [_wire(item) for item in value]
    return value


class _Namespace:
    def __init__(self, client: "Boxes"):
        self._c = client

    @staticmethod
    def _id(value: str) -> str:
        return quote(value, safe="")


class Boxes:
    """Client for a boxes-oss server. Call :meth:`close` when finished."""

    def __init__(self, base_url: str, api_key: str, *, transport: Optional[httpx.BaseTransport] = None, poll_interval: float = 0.5, timeout: float = 60.0):
        self.base_url = base_url.rstrip("/") + "/api/v1"
        self.api_key, self.poll_interval = api_key, poll_interval
        self._http = httpx.Client(transport=transport, headers={"Authorization": f"Bearer {api_key}"}, timeout=timeout)
        self.sandboxes = _Sandboxes(self)
        self.exec = _Exec(self)
        self.files = _Files(self)
        self.snapshots = _Snapshots(self)
        self.named_snapshots = _NamedSnapshots(self)
        self.hosts = _Hosts(self)
        self.desktop = _Desktop(self)
        self.environments = _Environments(self)
        self.api_keys = _APIKeys(self)
        self.webhooks = _Webhooks(self)

    def _request(self, method: str, path: str, *, json: Any = None, content: Any = None, headers: Optional[dict] = None, params: Optional[dict] = None) -> Any:
        response = self._http.request(method, self.base_url + path, json=_wire(json) if json is not None else None, content=content, headers=headers, params=params)
        if not response.is_success:
            try:
                error = response.json().get("error", {})
            except (ValueError, AttributeError):
                error = {}
            raise BoxesError(response.status_code, error.get("code", "http_error"), error.get("message", response.text or response.reason_phrase))
        if response.status_code == 204:
            return None
        if "json" in response.headers.get("content-type", ""):
            return response.json()
        return response.content

    def machine(self) -> Any:
        """Return the local VM capacity and health record."""
        return self._request("GET", "/machine")["machine"]

    def prompt(self, sandbox_id: str, prompt: str, provider: str, **options: Any) -> Any:
        body = {"prompt": prompt, "provider": provider, **options}
        return self._request("POST", f"/sandboxes/{quote(sandbox_id, safe='')}/prompt", json=body)

    def events(self, sandbox_id: str, *, after: int = 0, conversation: Optional[str] = None) -> Iterator[dict]:
        """Yield events continuously using the server's cursor long-poll API."""
        cursor = after
        while True:
            data = self._request("GET", f"/sandboxes/{quote(sandbox_id, safe='')}/events", params={"after": cursor, "wait": 30, "conversation": conversation})
            yield from data.get("events", [])
            cursor = max(cursor, data.get("cursor", cursor))

    def usage(self, sandbox_id: str) -> Any:
        return self._request("GET", f"/sandboxes/{quote(sandbox_id, safe='')}/usage")

    def provider_models(self) -> Any:
        return self._request("GET", "/provider-models")

    def push(self, sandbox_id: str, archive: Any) -> Any:
        return self.files.push(sandbox_id, archive)

    def pull(self, sandbox_id: str, path: str) -> bytes:
        return self.files.pull(sandbox_id, path)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Boxes":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()


class _Sandboxes(_Namespace):
    def create(self, *, idempotency_key: Optional[str] = None, **options: Any) -> Any:
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else None
        return self._c._request("POST", "/sandboxes", json=options, headers=headers)

    def list(self, filter: Optional[str] = None, tags: Optional[list] = None) -> Any:
        """tags: "key=value" (exact match) or "key" (has key); all must match."""
        params: dict = {"filter": filter}
        if tags:
            params["tag"] = list(tags)
        return self._c._request("GET", "/sandboxes", params=params)["sandboxes"]

    def get(self, sandbox_id: str) -> Any:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}")["sandbox"]

    def update(self, sandbox_id: str, **options: Any) -> Any:
        return self._c._request("PATCH", f"/sandboxes/{self._id(sandbox_id)}", json=options)["sandbox"]

    def stop(self, sandbox_id: str) -> Any:
        return self._c._request("POST", f"/sandboxes/{self._id(sandbox_id)}/stop", json={})

    def resume(self, sandbox_id: str, **options: Any) -> Any:
        data = self._c._request("POST", f"/sandboxes/{self._id(sandbox_id)}/resume", json=options)
        return data.get("sandbox", data)

    def fork(self, sandbox_id: str, *, idempotency_key: Optional[str] = None, **options: Any) -> Any:
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else None
        return self._c._request("POST", f"/sandboxes/{self._id(sandbox_id)}/fork", json=options, headers=headers)["sandbox"]

    def delete(self, sandbox_id: str) -> None:
        data = self._c._request("DELETE", f"/sandboxes/{self._id(sandbox_id)}")
        operation = data.get("operation") if data else None
        while operation and operation.get("status") == "pending":
            time.sleep(self._c.poll_interval)
            operation = self._c._request("GET", f"/operations/{self._id(operation['id'])}")["operation"]
        if operation and operation.get("status") == "failed":
            raise BoxesError(500, "operation_failed", operation.get("error", "Operation failed"))


class _Exec(_Namespace):
    def run(self, sandbox_id: str, cmd: Any, **options: Any) -> Any:
        return self._c._request("POST", f"/sandboxes/{self._id(sandbox_id)}/commands", json={"cmd": cmd, **options})

    def detached(self, sandbox_id: str, cmd: Any, **options: Any) -> Any:
        return self.run(sandbox_id, cmd, detached=True, **options)

    def get_command(self, sandbox_id: str, pid: str) -> Any:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/commands/{self._id(pid)}")


class _Files(_Namespace):
    def read(self, sandbox_id: str, path: str) -> bytes:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/files", params={"path": path})

    def write(self, sandbox_id: str, path: str, content: Any) -> Any:
        return self._c._request("PUT", f"/sandboxes/{self._id(sandbox_id)}/files", content=content, params={"path": path}, headers={"Content-Type": "application/octet-stream"})

    def push(self, sandbox_id: str, archive: Any) -> Any:
        return self._c._request("PUT", f"/sandboxes/{self._id(sandbox_id)}/archive", content=archive, headers={"Content-Type": "application/gzip"})

    def pull(self, sandbox_id: str, path: str) -> bytes:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/artifacts", params={"path": path})


class _Snapshots(_Namespace):
    def list(self, sandbox_id: str) -> Any:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/snapshots")["snapshots"]

    def list_all(self, *, limit: int = 50) -> Any:
        snapshots, cursor = [], None
        while True:
            params = {"limit": limit, "cursor": cursor}
            page = self._c._request("GET", "/snapshots", params=params)
            snapshots.extend(page["snapshots"])
            cursor = page.get("nextCursor")
            if not cursor:
                return snapshots

    def latest(self, sandbox_id: str) -> Any:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/snapshots/latest")["snapshot"]

    def delete(self, sandbox_id: str, snapshot_id: str, force: bool = False) -> Any:
        return self._c._request("DELETE", f"/sandboxes/{self._id(sandbox_id)}/snapshots/{self._id(snapshot_id)}", params={"force": str(force).lower()})

    def pull(self, snapshot_id: str) -> bytes:
        return self._c._request("GET", f"/snapshots/{self._id(snapshot_id)}/download")


class _NamedSnapshots(_Namespace):
    def list(self) -> Any:
        return self._c._request("GET", "/named-snapshots")["namedSnapshots"]

    def create(self, **data: Any) -> Any:
        return self._c._request("POST", "/named-snapshots", json=data)["namedSnapshot"]

    def get(self, name: str) -> Any:
        return self._c._request("GET", f"/named-snapshots/{self._id(name)}")["namedSnapshot"]

    def delete(self, name: str) -> Any:
        return self._c._request("DELETE", f"/named-snapshots/{self._id(name)}")


class _Hosts(_Namespace):
    def expose(self, sandbox_id: str, port: int, private: bool = False) -> Any:
        return self._c._request("POST", f"/sandboxes/{self._id(sandbox_id)}/hosts", json={"port": port, "private": private})["host"]

    def list(self, sandbox_id: str) -> Any:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/hosts")["hosts"]

    def remove(self, sandbox_id: str, port: int) -> Any:
        return self._c._request("DELETE", f"/sandboxes/{self._id(sandbox_id)}/hosts/{port}")


class _Desktop(_Namespace):
    def url(self, sandbox_id: str) -> str:
        return self._c._request("GET", f"/sandboxes/{self._id(sandbox_id)}/desktop")["url"]


class _Environments(_Namespace):
    def list(self) -> Any:
        return self._c._request("GET", "/environments")["environments"]

    def create(self, **data: Any) -> Any:
        return self._c._request("POST", "/environments", json=data)["environment"]

    def get(self, environment_id: str) -> Any:
        return self._c._request("GET", f"/environments/{self._id(environment_id)}")["environment"]

    def update(self, environment_id: str, **data: Any) -> Any:
        return self._c._request("PATCH", f"/environments/{self._id(environment_id)}", json=data)["environment"]

    def delete(self, environment_id: str) -> Any:
        return self._c._request("DELETE", f"/environments/{self._id(environment_id)}")


class _APIKeys(_Namespace):
    def list(self) -> Any:
        return self._c._request("GET", "/api-keys")["apiKeys"]

    def create(self, **data: Any) -> Any:
        return self._c._request("POST", "/api-keys", json=data)["apiKey"]

    def delete(self, key_id: str) -> Any:
        return self._c._request("DELETE", f"/api-keys/{self._id(key_id)}")

    def usage(self, key_id: str) -> Any:
        return self._c._request("GET", f"/api-keys/{self._id(key_id)}/usage")["usage"]


class _Webhooks(_Namespace):
    def list(self) -> Any:
        return self._c._request("GET", "/webhooks")["webhooks"]

    def create(self, **data: Any) -> Any:
        return self._c._request("POST", "/webhooks", json=data)

    def update(self, webhook_id: str, **data: Any) -> Any:
        return self._c._request("PATCH", f"/webhooks/{self._id(webhook_id)}", json=data)["webhook"]

    def delete(self, webhook_id: str) -> Any:
        return self._c._request("DELETE", f"/webhooks/{self._id(webhook_id)}")

    def rotate_secret(self, webhook_id: str) -> Any:
        return self._c._request("POST", f"/webhooks/{self._id(webhook_id)}/rotate-secret", json={})
