import { Paddock } from 'paddock-sdk';

const paddock = new Paddock({ baseUrl: process.env.PADDOCK_URL, apiKey: process.env.PADDOCK_API_KEY });
const box = await paddock.sandboxes.create({ type: 'small', ttlSeconds: 3600 });
try {
  console.log(await paddock.exec.run(box.id, 'echo hello'));
  await paddock.sandboxes.stop(box.id);
} finally {
  await paddock.sandboxes.delete(box.id);
}
