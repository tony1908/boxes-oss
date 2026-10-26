import json
import httpx
import pytest
from paddock_sdk import Paddock, PaddockError

def test_auth_idempotency_and_error_envelope():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/sandboxes") and request.method == "POST":
            return httpx.Response(201,json={"id":"bx_1","state":"ready"})
        return httpx.Response(400,json={"error":{"code":"invalid_type","message":"bad type"}})
    c=Paddock("https://example.test","secret",transport=httpx.MockTransport(handler))
    box=c.sandboxes.create(idempotency_key="key",type="small")
    assert box["id"] == "bx_1"
    assert requests[0].headers["authorization"] == "Bearer secret"
    assert requests[0].headers["idempotency-key"] == "key"
    with pytest.raises(PaddockError) as error: c.sandboxes.get("bad")
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
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
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
    with Paddock("https://example.test", "secret", transport=httpx.MockTransport(handler)) as c:
        assert c.machine() == machine

def test_sandbox_tags_are_sent_verbatim_and_filterable():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.method == "GET":
            return httpx.Response(200,json={"sandboxes":[]})
        return httpx.Response(200,json={"sandbox":{"id":"bx_t","tags":{"env_name":"prod"}}})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(tags={"env_name":"prod"})
        c.sandboxes.update("bx_t",tags={})
        assert c.sandboxes.list(tags=["team=agents","owner"])==[]
    assert json.loads(requests[0].read())=={"tags":{"env_name":"prod"}}
    assert json.loads(requests[1].read())=={"tags":{}}
    assert requests[2].url.params.get_list("tag")==["team=agents","owner"]

def test_sandbox_source_is_sent_verbatim_on_create():
    requests=[]
    def handler(request):
        requests.append(request)
        return httpx.Response(201,json={"id":"bx_s","state":"provisioning"})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(type="small",source={"type":"tarball","url":"https://example.test/src.tar.gz","path":"app"})
    assert json.loads(requests[0].read())["source"]=={"type":"tarball","url":"https://example.test/src.tar.gz","path":"app"}

def test_keep_snapshots_and_restore_use_wire_names():
    requests=[]
    def handler(request):
        requests.append(request)
        return httpx.Response(200,json={"id":"bx_k","sandbox":{"id":"bx_k","state":"stopped"}})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(type="small",keep_snapshots=3)
        c.sandboxes.update("bx_k",keep_snapshots=2)
        assert c.sandboxes.restore("bx_k","sn_1")["state"]=="stopped"
    assert json.loads(requests[0].read())=={"type":"small","keepSnapshots":3}
    assert json.loads(requests[1].read())=={"keepSnapshots":2}
    assert requests[2].method=="POST" and requests[2].url.path=="/api/v1/sandboxes/bx_k/restore"
    assert json.loads(requests[2].read())=={"snapshotId":"sn_1"}

def test_network_policy_nested_keys_are_camel_cased_on_the_wire():
    requests=[]
    def handler(request):
        requests.append(request)
        return httpx.Response(200,json={"id":"bx_n","sandbox":{"id":"bx_n"}})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        c.sandboxes.create(network_policy={"mode":"allowlist","allow_domains":["api.github.com"],"allow_cidrs":["10.0.0.0/8"],"deny_cidrs":["10.1.0.0/16"]})
        c.sandboxes.update("bx_n",network_policy=None)
    assert json.loads(requests[0].read())=={"networkPolicy":{"mode":"allowlist","allowDomains":["api.github.com"],"allowCidrs":["10.0.0.0/8"],"denyCidrs":["10.1.0.0/16"]}}
    assert json.loads(requests[1].read())=={"networkPolicy":None}

def test_detached_command_streams_logs_and_controls_process():
    import json as jsonlib
    seen = []
    def handler(request):
        path = request.url.path
        seen.append((request.method, path, request.url.params.get("close"), request.content))
        if request.method == "POST" and path.endswith("/commands"):
            return httpx.Response(202, json={"processId": "p1"})
        if path.endswith("/p1/logs") and request.url.params.get("follow") == "1":
            body = b'{"stream":"stdout","data":"hi\\n","offset":3}\n{"exit":true,"exitCode":4}\n'
            return httpx.Response(200, content=body, headers={"content-type": "application/x-ndjson"})
        if path.endswith("/p1/kill"):
            return httpx.Response(200, json={"processId": "p1", "signal": "KILL"})
        if path.endswith("/p1/stdin"):
            return httpx.Response(200, json={"bytes": len(request.content)})
        if request.method == "GET" and path.endswith("/commands"):
            return httpx.Response(200, json={"commands": [{"processId": "p1", "running": False, "exitCode": 4}]})
        if path.endswith("/p1"):
            return httpx.Response(200, json={"running": False, "exitCode": 4})
        return httpx.Response(404, json={"error": {"code": "process_not_found", "message": "exec process not found"}})
    c = Paddock("https://example.test", "secret", transport=httpx.MockTransport(handler), poll_interval=0)
    command = c.exec.detached("bx_3", "cat", stdin=True)
    assert command.process_id == "p1"
    assert list(command.logs(follow=True)) == [{"stream": "stdout", "data": "hi\n", "offset": 3}, {"exit": True, "exitCode": 4}]
    assert command.wait()["exitCode"] == 4
    command.kill("KILL")
    command.write_stdin("abc")
    command.close_stdin()
    assert c.sandboxes.list_commands("bx_3") == [{"processId": "p1", "running": False, "exitCode": 4}]
    kill = next(entry for entry in seen if entry[1].endswith("/p1/kill"))
    assert jsonlib.loads(kill[3]) == {"signal": "KILL"}
    assert ("POST", "/api/v1/sandboxes/bx_3/commands/p1/stdin", None, b"abc") in seen
    assert ("POST", "/api/v1/sandboxes/bx_3/commands/p1/stdin", "1", b"") in seen
    with pytest.raises(PaddockError) as error:
        list(command.logs())
    assert (error.value.status, error.value.code) == (404, "process_not_found")
    c.close()

def test_image_on_create_and_usage_totals_are_returned():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/usage"):
            return httpx.Response(200,json={"runningSeconds":5,"totals":{"cpuSeconds":2.5,"rxBytes":10,"txBytes":20},"live":{"cpuSeconds":1,"rxBytes":4,"txBytes":6}})
        return httpx.Response(201,json={"id":"bx_i","image":"ghcr.io/example/custom-box:1","state":"ready"})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        box=c.sandboxes.create(type="small",image="ghcr.io/example/custom-box:1")
        usage=c.usage("bx_i")
    assert box["image"]=="ghcr.io/example/custom-box:1"
    assert json.loads(requests[0].read())=={"type":"small","image":"ghcr.io/example/custom-box:1"}
    assert usage["totals"]=={"cpuSeconds":2.5,"rxBytes":10,"txBytes":20}

def test_drives_manage_volumes_and_mounts_use_wire_names():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/drives") and request.method == "GET":
            return httpx.Response(200,json={"drives":[{"name":"data","mountedBy":[]}]})
        if request.url.path.endswith("/drives/data/fork"):
            return httpx.Response(201,json={"name":"copy"})
        if request.url.path.endswith("/sandboxes"):
            return httpx.Response(201,json={"id":"bx_m","state":"provisioning"})
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(201,json={"name":"data"})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        assert c.drives.create("data")["name"] == "data"
        assert c.drives.list() == [{"name":"data","mountedBy":[]}]
        assert c.drives.fork("data", "copy")["name"] == "copy"
        c.drives.delete("data")
        c.sandboxes.create(type="small", mounts=[{"drive":"cache","path":"/mnt/cache","read_only":True}])
    assert [(r.method, r.url.path) for r in requests] == [
        ("POST","/api/v1/drives"),("GET","/api/v1/drives"),("POST","/api/v1/drives/data/fork"),("DELETE","/api/v1/drives/data"),("POST","/api/v1/sandboxes")]
    assert json.loads(requests[0].read()) == {"name":"data"}
    assert json.loads(requests[2].read()) == {"name":"copy"}
    assert json.loads(requests[4].read())["mounts"] == [{"drive":"cache","path":"/mnt/cache","readOnly":True}]

def test_users_and_groups_use_wire_names_and_exec_runs_as_user():
    requests=[]
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/users") and request.method == "GET":
            return httpx.Response(200,json={"users":[{"username":"alice","homeDir":"/home/alice","groups":["alice"]}]})
        if request.url.path.endswith("/users") and request.method == "POST":
            return httpx.Response(201,json={"username":"alice","homeDir":"/home/alice"})
        if request.url.path.endswith("/groups") and request.method == "POST":
            return httpx.Response(201,json={"name":"team","sharedDir":"/shared/team"})
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.url.path.endswith("/commands"):
            return httpx.Response(200,json={"exitCode":0,"stdout":"alice\n","stderr":""})
        return httpx.Response(200,json={"username":"alice","group":"team"})
    with Paddock("https://example.test","secret",transport=httpx.MockTransport(handler)) as c:
        assert c.sandboxes.create_user("bx_u","alice")["homeDir"] == "/home/alice"
        assert c.sandboxes.list_users("bx_u") == [{"username":"alice","homeDir":"/home/alice","groups":["alice"]}]
        assert c.sandboxes.create_group("bx_u","team")["sharedDir"] == "/shared/team"
        c.sandboxes.add_user_to_group("bx_u","team","alice")
        c.sandboxes.remove_user_from_group("bx_u","team","alice")
        c.sandboxes.delete_user("bx_u","alice")
        assert c.exec.run("bx_u",["id","-un"],user="alice")["stdout"] == "alice\n"
    assert [(r.method, r.url.path) for r in requests] == [
        ("POST","/api/v1/sandboxes/bx_u/users"),("GET","/api/v1/sandboxes/bx_u/users"),("POST","/api/v1/sandboxes/bx_u/groups"),
        ("PUT","/api/v1/sandboxes/bx_u/groups/team/members/alice"),("DELETE","/api/v1/sandboxes/bx_u/groups/team/members/alice"),
        ("DELETE","/api/v1/sandboxes/bx_u/users/alice"),("POST","/api/v1/sandboxes/bx_u/commands")]
    assert json.loads(requests[0].read()) == {"username":"alice"}
    assert json.loads(requests[2].read()) == {"name":"team"}
    assert json.loads(requests[6].read()) == {"cmd":["id","-un"],"user":"alice"}
