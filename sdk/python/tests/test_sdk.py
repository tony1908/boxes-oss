import json
import httpx
import pytest
from boxes_sdk import Boxes, BoxesError

def test_auth_idempotency_and_error_envelope():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/sandboxes") and request.method == "POST":
            return httpx.Response(201,json={"id":"bx_1","state":"ready"})
        return httpx.Response(400,json={"error":{"code":"invalid_type","message":"bad type"}})
    c=Boxes("https://example.test","secret",transport=httpx.MockTransport(handler))
    box=c.sandboxes.create(idempotency_key="key",type="small")
    assert box["id"] == "bx_1"
    assert requests[0].headers["authorization"] == "Bearer secret"
    assert requests[0].headers["idempotency-key"] == "key"
    with pytest.raises(BoxesError) as error: c.sandboxes.get("bad")
    assert (error.value.status,error.value.code,error.value.message)==(400,"invalid_type","bad type")
    c.close()

def test_snake_case_wire_names_and_response_unwrapping():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/sandboxes"):
            return httpx.Response(201,json={"id":"bx_2","state":"ready"})
        if request.url.path.endswith("/environments"):
            return httpx.Response(201,json={"environment":{"id":"env_1"}})
        if request.url.path.endswith("/named-snapshots"):
            return httpx.Response(200,json={"namedSnapshots":[]})
        return httpx.Response(204)
    with Boxes("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(ttl_seconds=3600)
        assert c.environments.create(name="dev", setup_script="echo ok")=={"id":"env_1"}
        assert c.named_snapshots.list()==[]
        c.sandboxes.delete("bx_2")
    assert requests[0].read() == b'{"ttlSeconds":3600}'
    assert requests[1].read() == b'{"name":"dev","setupScript":"echo ok"}'


def test_machine_returns_local_vm_record():
    machine = {"cpus": 4, "memoryBytes": 8589934592, "memAvailableBytes": 4294967296, "diskFreeBytes": 17179869184, "swapFreeBytes": 0, "memoryPressureSomeAvg10": 0.0, "status": "healthy", "runningBoxes": 2, "lastSeen": "2026-09-27T12:00:00Z"}
    def handler(request):
        assert request.headers["authorization"] == "Bearer secret"
        assert request.url.path == "/api/v1/machine"
        return httpx.Response(200, json={"machine": machine})
    with Boxes("https://example.test", "secret", transport=httpx.MockTransport(handler)) as c:
        assert c.machine() == machine
