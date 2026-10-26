import os

from paddock_sdk import Paddock

with Paddock(os.environ["PADDOCK_URL"], os.environ["PADDOCK_API_KEY"]) as paddock:
    box = paddock.sandboxes.create(type="small", ttl_seconds=3600)
    result = paddock.exec.run(box["id"], "echo hello")
    print(result)
    paddock.sandboxes.stop(box["id"])
