import { Boxes } from '@boxes-oss/sdk';

const boxes = new Boxes({ baseUrl: process.env.BOXES_URL!, apiKey: process.env.BOXES_API_KEY! });
try {
  const box = await boxes.sandboxes.create({ type: 'small', ttlSeconds: 3600 });
  const result = await boxes.exec.run(box.id, 'echo hello');
  console.log(result);
  await boxes.sandboxes.stop(box.id);
} finally {
  // Boxes uses global fetch and has no persistent connection to close.
}
