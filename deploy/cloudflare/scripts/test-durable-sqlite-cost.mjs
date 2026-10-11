import { Miniflare, convertV4MiniflareOptions } from 'miniflare';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';
import { randomBytes, randomUUID } from 'node:crypto';
import assert from 'node:assert/strict';

const root=fileURLToPath(new URL('../',import.meta.url));
// Test-only metering retains actual SQLite cursors. Every query still executes
// against workerd's real durable SQL; no result or storage operation is replaced.
const metering=`
import { SQLiteFiles } from '../storage.js';
export class MeteredFiles extends SQLiteFiles {
 constructor(ctx,env) {
  super(ctx,env);this.cursors=[];
  this.sql={exec:(...args)=>{const cursor=ctx.storage.sql.exec(...args);this.cursors.push(cursor);return cursor;}};
 }
 cost() {const rowsRead=this.cursors.reduce((sum,cursor)=>sum+cursor.rowsRead,0);this.cursors=[];return {rowsRead};}
}
export default {async fetch(request,env) {
 const store=env.FILES.getByName(request.headers.get('X-Cost-Authority')||'cost');
 const path=new URL(request.url).pathname;
 if(path==='/__cost') return Response.json(await store.cost());
 if(path==='/__activate') {const input=await request.json();return Response.json(await store.activate(input.sequence||1,input.capability));}
 return store.fetch(request);
}};`;
const token=randomBytes(32).toString('hex'),owner=randomUUID();
let generation=randomBytes(32).toString('hex');
const runtime=new Miniflare(convertV4MiniflareOptions({
 modulesRoot:root,port:18884,compatibilityDate:'2026-10-09',
 modules:[{type:'ESModule',path:join(root,'scripts/cost-worker.js'),contents:metering},
  {type:'ESModule',path:join(root,'storage.js'),contents:await readFile(join(root,'storage.js'),'utf8')}],
 durableObjects:{FILES:{className:'MeteredFiles',useSQLite:true}},
 bindings:{STORAGE_TOKEN:token,REQUIRE_IMPORT:'false'}
}));
let epoch,revision=0;
const headers={'X-Storage-Capability':token,'X-Storage-Generation':generation,'X-Storage-Owner':owner};
async function request(path,body) {
 headers['X-Storage-Revision']=String(revision);
 return runtime.dispatchFetch('http://localhost'+path,{method:body===undefined?'GET':'POST',headers,body:body===undefined?undefined:JSON.stringify(body)});
}
async function json(path,body,status=200) {
 const response=await request(path,body);assert.equal(response.status,status,path);return response.json();
}
async function begin(size,pages) {
 const id=randomUUID();await json('/begin',{id,revision,size,count:pages.length});
 for(let start=0;start<pages.length;start+=128) await json('/stage',{id,pages:pages.slice(start,start+128)});
 return id;
}
async function commit(id,status=200) {
 const response=await request('/commit',{id,revision});assert.equal(response.status,status);
 if(status===200) {const result=await response.json();assert.equal(result.revision,revision+1);revision=result.revision;}
 else await response.arrayBuffer();
}
const page=(offset,value)=>({offset,data:Buffer.alloc(4096,value).toString('base64')});
try {
 await runtime.ready;
 epoch=(await json('/__activate',{capability:generation})).epoch;
 headers['X-Storage-Epoch']=String(epoch);await json('/claim',{});
 // Initial imports must cover every offset, including page zero.
 const missingInitial=await begin(8192,[page(4096,1)]);
 await commit(missingInitial,400);
 assert.deepEqual((await json('/meta')).files,[]);
 const size=2048*4096;
 const initial=await begin(size,Array.from({length:2048},(_,index)=>page(index*4096,1)));
 await commit(initial);
 const update=await begin(size,[page(4096,2)]);
 await json('/__cost');await commit(update);
 const cost=await json('/__cost');assert.ok(cost.rowsRead<64,JSON.stringify(cost));
 const content=await request('/range?name=main.db&offset=4096&length=4096');
 assert.equal(content.status,200);
 assert.deepEqual(Buffer.from(await content.arrayBuffer()),Buffer.alloc(4096,2));
 // An existing file cannot grow with a missing middle page, even when the
 // caller's declared staged count matches the actual staging rows.
 const missingGrowth=await begin(size+8192,[page(size+4096,3)]);
 await commit(missingGrowth,400);
 const unchanged=await json('/meta');assert.equal(unchanged.revision,revision);assert.equal(unchanged.files[0].size,size);
 const mixedHole=await begin(size+8192,[page(4096,9),page(size+4096,3)]);
 await commit(mixedHole,400);
 const preserved=await request('/range?name=main.db&offset=4096&length=4096');assert.equal(preserved.status,200);
 assert.deepEqual(Buffer.from(await preserved.arrayBuffer()),Buffer.alloc(4096,2));
 const grown=await begin(size+8192,[page(size,3),page(size+4096,4)]);
 await commit(grown);assert.equal((await json('/meta')).files[0].size,size+8192);
 const shrink=await begin(4096,[]);await commit(shrink);
 assert.equal((await json('/meta')).files[0].size,4096);
 const gone=await request('/range?name=main.db&offset=4096&length=4096');assert.equal(gone.status,404);await gone.arrayBuffer();
 const regrowHole=await begin(12288,[page(8192,5)]);await commit(regrowHole,400);
 assert.equal((await json('/meta')).files[0].size,4096);
 const regrown=await begin(12288,[page(4096,6),page(8192,7)]);await commit(regrown);
 const newTail=await request('/range?name=main.db&offset=4096&length=8192');assert.equal(newTail.status,200);
 assert.deepEqual(Buffer.from(await newTail.arrayBuffer()),Buffer.concat([Buffer.alloc(4096,6),Buffer.alloc(4096,7)]));
 // Reusing a superseded pending ID must not revive its previously staged rows.
 const superseded=await begin(16384,[page(12288,8)]);
 await begin(12288,[]);
 await json('/begin',{id:superseded,revision,size:16384,count:1});
 await commit(superseded,409);
 await json('/stage',{id:superseded,pages:[page(12288,8)]});
 // Activating another generation must cascade pending/staged data away too.
 generation=randomBytes(32).toString('hex');
 epoch=(await json('/__activate',{sequence:2,capability:generation})).epoch;
 headers['X-Storage-Epoch']=String(epoch);headers['X-Storage-Generation']=generation;
 await json('/claim',{});
 await json('/begin',{id:superseded,revision,size:16384,count:1});
 await commit(superseded,409);
 const empty=await begin(0,[]);await commit(empty);assert.equal((await json('/meta')).files[0].size,0);
 const emptyHole=await begin(8192,[page(4096,8)]);await commit(emptyHole,400);
 console.log('Real durable SQL: 2048-page database, one-page commit reads '+cost.rowsRead+' rows; import/growth/rollback, shrink/regrow, zero truncation and staging cleanup passed');
} finally {await runtime.dispose();}
