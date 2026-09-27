# @boxes-oss/sdk

Install with `npm install @boxes-oss/sdk`. Requires Node.js 18+ and its global `fetch`.

```ts
import { Boxes } from '@boxes-oss/sdk';
const boxes = new Boxes({ baseUrl: process.env.BOXES_URL!, apiKey: process.env.BOXES_API_KEY! });
const box = await boxes.sandboxes.create({ type: 'small', ttlSeconds: 3600 });
const result = await boxes.exec.run(box.id, 'echo hello');
console.log(result);
const snapshots = await boxes.snapshots.list(box.id);
console.log(`Snapshots: ${snapshots.length}`);
console.log((await boxes.providerModels()).providers.map(provider => provider.id));
await boxes.sandboxes.stop(box.id);
await boxes.sandboxes.delete(box.id);
```

Set `BOXES_URL` and `BOXES_API_KEY` before running `node examples/quickstart.mjs` after installing the package. The client also supports file transfer, snapshots, prompts and event streams, hosts, usage, environments, API keys, and webhooks. See the [public API contract](../../api/openapi.yaml).
