# boxes-sdk

Install with `pip install boxes-sdk`. Requires Python 3.9+; httpx is the only runtime dependency.

```python
import os
from boxes_sdk import Boxes
with Boxes(os.environ['BOXES_URL'], os.environ['BOXES_API_KEY']) as boxes:
    box = boxes.sandboxes.create(type='small', ttl_seconds=3600)
    result = boxes.exec.run(box['id'], 'echo hello')
    print(result)
    snapshots = boxes.snapshots.list(box['id'])
    print(f"Snapshots: {len(snapshots)}")
    boxes.sandboxes.stop(box['id'])
    boxes.sandboxes.delete(box['id'])
```

Set `BOXES_URL` and `BOXES_API_KEY` before running the [example](examples/quickstart.py). The client also supports file transfer, snapshots, prompts and event streams, hosts, usage, environments, API keys, and webhooks. See the [public API contract](../../api/openapi.yaml).
