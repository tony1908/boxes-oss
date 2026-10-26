import { Paddock } from 'paddock-sdk';

const paddock = new Paddock({ baseUrl: process.env.PADDOCK_URL!, apiKey: process.env.PADDOCK_API_KEY! });
try {
  const box = await paddock.sandboxes.create({ type: 'small', ttlSeconds: 3600 });
  const result = await paddock.exec.run(box.id, 'echo hello');
  console.log(result);
  await paddock.sandboxes.stop(box.id);
} finally {
  // Paddock uses global fetch and has no persistent connection to close.
}
