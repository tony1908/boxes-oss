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

def test_sandbox_tags_are_sent_verbatim_and_filterable():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.method == "GET":
            return httpx.Response(200,json={"sandboxes":[]})
        return httpx.Response(200,json={"sandbox":{"id":"bx_t","tags":{"env_name":"prod"}}})
    with Boxes("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(tags={"env_name":"prod"})
        c.sandboxes.update("bx_t",tags={})
        assert c.sandboxes.list(tags=["team=agents","owner"])==[]
    assert json.loads(requests[0].read())=={"tags":{"env_name":"prod"}}
    assert json.loads(requests[1].read())=={"tags":{}}
    assert requests[2].url.params.get_list("tag")==["team=agents","owner"]

def test_network_policy_nested_keys_are_camel_cased_on_the_wire():
    requests=[]
    def handler(request):
        requests.append(request)
        return httpx.Response(200,json={"id":"bx_n","sandbox":{"id":"bx_n"}})
    with Boxes("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(network_policy={"mode":"allowlist","allow_domains":["api.github.com"],"allow_cidrs":["10.0.0.0/8"],"deny_cidrs":["10.1.0.0/16"]})
        c.sandboxes.update("bx_n",network_policy=None)
    assert json.loads(requests[0].read())=={"networkPolicy":{"mode":"allowlist","allowDomains":["api.github.com"],"allowCidrs":["10.0.0.0/8"],"denyCidrs":["10.1.0.0/16"]}}
    assert json.loads(requests[1].read())=={"networkPolicy":None}
