import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { Boxes, BoxesError } from '../dist/index.js';
test('creates with auth/idempotency and maps server errors', async t => {
  const seen=[]; const server=createServer((req,res)=>{seen.push(req); if(req.url==='/api/v1/sandboxes' && req.method==='POST'){res.writeHead(201,{'content-type':'application/json'});res.end(JSON.stringify({id:'bx_1',state:'ready'}));return;} res.writeHead(400,{'content-type':'application/json'});res.end(JSON.stringify({error:{code:'invalid_type',message:'bad type'}}));});
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close()); const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  assert.equal((await c.sandboxes.create({type:'small'},'key')).id,'bx_1'); assert.equal(seen[0].headers.authorization,'Bearer secret');assert.equal(seen[0].headers['idempotency-key'],'key');
  await assert.rejects(c.sandboxes.get('bad'),e=>e instanceof BoxesError && e.status===400 && e.code==='invalid_type' && e.message==='bad type');
});

test('unwraps API envelopes and waits for asynchronous deletion', async t => {
  const seen=[]; let polls=0;
  const server=createServer((req,res)=>{
    seen.push(`${req.method} ${req.url}`);
    res.setHeader('content-type','application/json');
    if(req.url==='/api/v1/sandboxes/bx_2' && req.method==='DELETE') { res.writeHead(202);res.end(JSON.stringify({operation:{id:'op_1',status:'pending'}}));return; }
    if(req.url==='/api/v1/operations/op_1') { polls++;res.end(JSON.stringify({operation:{id:'op_1',status:polls===1?'pending':'done'}}));return; }
    if(req.url==='/api/v1/named-snapshots') { res.end(JSON.stringify({namedSnapshots:[{name:'gold'}]}));return; }
    if(req.url==='/api/v1/environments' && req.method==='POST') { res.end(JSON.stringify({environment:{id:'env_1'}}));return; }
    res.writeHead(404);res.end('{}');
  });
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close());
  const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret',pollIntervalMs:1});
  assert.deepEqual(await c.namedSnapshots.list(),[{name:'gold'}]);
  assert.deepEqual(await c.environments.create({name:'dev'}),{id:'env_1'});
  await c.sandboxes.delete('bx_2');
  assert.equal(polls,2);
  assert.ok(seen.includes('GET /api/v1/operations/op_1'));
});

test('listAll follows snapshot pagination cursors', async t => {
  const urls=[];
  const server=createServer((req,res)=>{
    urls.push(req.url);res.setHeader('content-type','application/json');
    if(req.url==='/api/v1/snapshots?limit=1') { res.end(JSON.stringify({snapshots:[{id:'s1'}],nextCursor:'next'}));return; }
    if(req.url==='/api/v1/snapshots?limit=1&cursor=next') { res.end(JSON.stringify({snapshots:[{id:'s2'}],nextCursor:''}));return; }
    res.writeHead(404);res.end('{}');
  });
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close());
  const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  assert.deepEqual(await c.snapshots.listAll({limit:1}),[{id:'s1'},{id:'s2'}]);
  assert.equal(urls.length,2);
});


test('reads the authenticated single-machine record', async t => {
  let auth;
  const machine={cpus:4,memoryBytes:8589934592,memAvailableBytes:4294967296,diskFreeBytes:17179869184,swapFreeBytes:0,memoryPressureSomeAvg10:0,status:'healthy',runningBoxes:2,lastSeen:'2026-09-27T12:00:00Z'};
  const server=createServer((req,res)=>{auth=req.headers.authorization;res.setHeader('content-type','application/json');res.end(JSON.stringify({machine}));});
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close());
  const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  assert.deepEqual(await c.machine(),machine); assert.equal(auth,'Bearer secret');
});
