import os

from boxes_sdk import Boxes

with Boxes(os.environ["BOXES_URL"], os.environ["BOXES_API_KEY"]) as boxes:
    box = boxes.sandboxes.create(type="small", ttl_seconds=3600)
    result = boxes.exec.run(box["id"], "echo hello")
    print(result)
    boxes.sandboxes.stop(box["id"])
