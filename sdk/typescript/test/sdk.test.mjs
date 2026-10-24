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

test('sends sandbox source on create', async t => {
  const seen=[]; const server=createServer((req,res)=>{let body=''; req.on('data',c=>body+=c); req.on('end',()=>{seen.push(body);res.writeHead(201,{'content-type':'application/json'});res.end(JSON.stringify({id:'bx_s',state:'provisioning'}));});});
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close()); const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  await c.sandboxes.create({type:'small',source:{type:'git',url:'https://example.test/repo.git',revision:'v1.0.0',depth:1,token:'ghp_x',path:'app'}});
  assert.deepEqual(JSON.parse(seen[0]).source,{type:'git',url:'https://example.test/repo.git',revision:'v1.0.0',depth:1,token:'ghp_x',path:'app'});
});

test('sends sandbox tags on create, update, and list filters', async t => {
  const seen=[]; const server=createServer((req,res)=>{let body=''; req.on('data',c=>body+=c); req.on('end',()=>{seen.push({method:req.method,url:req.url,body});res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({id:'bx_t',sandbox:{id:'bx_t',tags:{team:'agents'}},sandboxes:[]}));});});
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close()); const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  await c.sandboxes.create({type:'small',tags:{team:'agents'}});
  await c.sandboxes.update('bx_t',{tags:{}});
  await c.sandboxes.list(undefined,['team=agents','owner']);
  assert.deepEqual(JSON.parse(seen[0].body).tags,{team:'agents'});
  assert.deepEqual(JSON.parse(seen[1].body),{tags:{}});
  assert.equal(seen[2].url,'/api/v1/sandboxes?tag=team%3Dagents&tag=owner');
});

test('sends keepSnapshots on create and update and restores from a snapshot', async t => {
  const seen=[]; const server=createServer((req,res)=>{let body=''; req.on('data',c=>body+=c); req.on('end',()=>{seen.push({method:req.method,url:req.url,body});res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({id:'bx_k',sandbox:{id:'bx_k',state:'stopped'}}));});});
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close()); const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  await c.sandboxes.create({type:'small',keepSnapshots:3});
  await c.sandboxes.update('bx_k',{keepSnapshots:2});
  assert.equal((await c.sandboxes.restore('bx_k','sn_1')).state,'stopped');
  assert.deepEqual(JSON.parse(seen[0].body),{type:'small',keepSnapshots:3});
  assert.deepEqual(JSON.parse(seen[1].body),{keepSnapshots:2});
  assert.equal(seen[2].method,'POST');
  assert.equal(seen[2].url,'/api/v1/sandboxes/bx_k/restore');
  assert.deepEqual(JSON.parse(seen[2].body),{snapshotId:'sn_1'});
});

test('sends network policy on create and update as networkPolicy', async t => {
  const seen=[]; const server=createServer((req,res)=>{let body=''; req.on('data',c=>body+=c); req.on('end',()=>{seen.push({method:req.method,body});res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({id:'bx_n',sandbox:{id:'bx_n'}}));});});
  await new Promise(r=>server.listen(0,'127.0.0.1',r)); t.after(()=>server.close()); const addr=server.address(); const c=new Boxes({baseUrl:`http://127.0.0.1:${addr.port}`,apiKey:'secret'});
  const policy={mode:'allowlist',allowDomains:['api.github.com'],allowCidrs:['10.0.0.0/8'],denyCidrs:['10.1.0.0/16']};
  await c.sandboxes.create({type:'small',networkPolicy:policy});
  await c.sandboxes.update('bx_n',{networkPolicy:null});
  assert.deepEqual(JSON.parse(seen[0].body).networkPolicy,policy);
  assert.equal(seen[1].method,'PATCH');
  assert.deepEqual(JSON.parse(seen[1].body),{networkPolicy:null});
});

test('detached commands stream logs across chunks and expose wait, kill, stdin, and listing', async t => {
  const seen = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    seen.push(`${req.method} ${req.url} ${body}`);
    const base = '/api/v1/sandboxes/bx_3/commands';
    if (req.method === 'POST' && req.url === base) { res.writeHead(202, { 'content-type': 'application/json' }); res.end(JSON.stringify({ processId: 'p1' })); return; }
    if (req.method === 'GET' && req.url.startsWith(`${base}/p1/logs`)) {
      res.writeHead(200, { 'content-type': 'application/x-ndjson' });
      res.write('{"stream":"stdout","data":"hel');
      await new Promise(r => setTimeout(r, 20));
      res.end('lo\\n","offset":6,"stream":"stdout"}\n{"exit":true,"exitCode":0}\n');
      return;
    }
    if (req.method === 'GET' && req.url === `${base}/p1`) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ running: false, exitCode: 0, stdout: 'hello\\n', stderr: '' })); return; }
    if (req.method === 'GET' && req.url === base) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ commands: [{ processId: 'p1', running: false, exitCode: 0 }] })); return; }
    if (req.method === 'POST' && req.url === `${base}/p1/kill`) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ processId: 'p1', signal: 'HUP' })); return; }
    if (req.method === 'POST' && req.url.startsWith(`${base}/p1/stdin`)) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ bytes: body.length })); return; }
    res.writeHead(404); res.end('{}');
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  t.after(() => server.close());
  const addr = server.address();
  const c = new Boxes({ baseUrl: `http://127.0.0.1:${addr.port}`, apiKey: 'secret', pollIntervalMs: 1 });

  const command = await c.exec.detached('bx_3', 'cat', { stdin: true });
  assert.equal(command.processId, 'p1');
  const lines = [];
  for await (const line of command.logs({ follow: true })) lines.push(line);
  assert.deepEqual(lines, [{ stream: 'stdout', data: 'hello\n', offset: 6 }, { exit: true, exitCode: 0 }]);
  assert.equal((await command.wait()).exitCode, 0);
  await command.kill('HUP');
  await command.writeStdin('abc');
  await command.closeStdin();
  assert.deepEqual(await c.sandboxes.listCommands('bx_3'), [{ processId: 'p1', running: false, exitCode: 0 }]);
  assert.ok(seen.includes('POST /api/v1/sandboxes/bx_3/commands/p1/kill {"signal":"HUP"}'));
  assert.ok(seen.includes('POST /api/v1/sandboxes/bx_3/commands/p1/stdin abc'));
  assert.ok(seen.some(line => line.startsWith('POST /api/v1/sandboxes/bx_3/commands/p1/stdin?close=1')));
  assert.ok(seen.includes('GET /api/v1/sandboxes/bx_3/commands/p1/logs?follow=1 '));
});
