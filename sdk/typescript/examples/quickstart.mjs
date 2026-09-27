import { Boxes } from '@boxes-oss/sdk';

const boxes = new Boxes({ baseUrl: process.env.BOXES_URL, apiKey: process.env.BOXES_API_KEY });
const box = await boxes.sandboxes.create({ type: 'small', ttlSeconds: 3600 });
try {
  console.log(await boxes.exec.run(box.id, 'echo hello'));
  await boxes.sandboxes.stop(box.id);
} finally {
  await boxes.sandboxes.delete(box.id);
}
