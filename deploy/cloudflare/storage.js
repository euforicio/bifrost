import { DurableObject } from "cloudflare:workers";

// Qualification prototype: main SQLite pages are durable; the engine's local
// rollback journals and temporary files are disposable. Production adoption
// requires the application, crash and controller-generation gates.
export class SQLiteFiles extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec(`
      CREATE TABLE IF NOT EXISTS authority (id INTEGER PRIMARY KEY CHECK(id=1), epoch INTEGER NOT NULL, owner TEXT NOT NULL, revision INTEGER NOT NULL, last_id TEXT, last_hash TEXT);
      INSERT OR IGNORE INTO authority(id,epoch,owner,revision) VALUES(1,0,'',0);
      CREATE TABLE IF NOT EXISTS files (name TEXT PRIMARY KEY, size INTEGER NOT NULL);
      CREATE TABLE IF NOT EXISTS pages (file TEXT NOT NULL REFERENCES files(name) ON DELETE CASCADE, offset INTEGER NOT NULL, bytes BLOB NOT NULL, PRIMARY KEY(file,offset));
      CREATE TABLE IF NOT EXISTS generation (id INTEGER PRIMARY KEY CHECK(id=1), sequence INTEGER NOT NULL, capability TEXT NOT NULL);
      INSERT OR IGNORE INTO generation VALUES(1,0,'');
      CREATE TABLE IF NOT EXISTS import_receipt (id INTEGER PRIMARY KEY CHECK(id=1), size INTEGER NOT NULL, sha256 TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS pending (id TEXT PRIMARY KEY, epoch INTEGER NOT NULL, revision INTEGER NOT NULL, size INTEGER NOT NULL, count INTEGER NOT NULL);
      CREATE TABLE IF NOT EXISTS staged (id TEXT NOT NULL REFERENCES pending(id) ON DELETE CASCADE, offset INTEGER NOT NULL, bytes BLOB NOT NULL, PRIMARY KEY(id,offset));
    `);
  }

  // Only trusted Worker/Container controllers invoke this RPC. Container HTTP
  // clients cannot mint a new generation or reactivate a fenced process.
  activate(sequence,capability) {
    if (!integer(sequence,1,Number.MAX_SAFE_INTEGER) || !/^[a-f0-9]{64}$/.test(capability || "")) throw new Error("Invalid generation capability");
    return this.ctx.storage.transactionSync(()=>{
      const current=this.sql.exec("SELECT sequence,capability FROM generation WHERE id=1").one();
      if (current.sequence===sequence && current.capability===capability) return this.sql.exec("SELECT epoch FROM authority WHERE id=1").one();
      if (sequence<=current.sequence) throw new Error("Stale controller generation");
      this.sql.exec("UPDATE generation SET sequence=?,capability=? WHERE id=1",sequence,capability);
      this.sql.exec("UPDATE authority SET epoch=epoch+1,owner='' WHERE id=1");
      this.sql.exec("DELETE FROM pending");
      return this.sql.exec("SELECT epoch FROM authority WHERE id=1").one();
    });
  }
  generation() { return this.sql.exec("SELECT sequence FROM generation WHERE id=1").one().sequence; }
  readiness() {
    const state=this.sql.exec("SELECT revision FROM authority WHERE id=1").one();
    const file=this.sql.exec("SELECT size FROM files WHERE name='main.db'").toArray()[0];
    const proof=this.sql.exec('SELECT size,sha256 FROM import_receipt WHERE id=1').toArray()[0];
    return {ready:state.revision>0 && !!file && file.size>0,verified_import:!!proof,revision:state.revision,size:file?.size || 0};
  }
  async completeImport(size,sha256) {
    if (!integer(size,4096,1024**3) || size%4096 || !/^[a-f0-9]{64}$/.test(sha256 || '')) throw new Error('Invalid import receipt');
    const state=this.readiness();
    if(state.verified_import) {
      const proof=this.sql.exec('SELECT size,sha256 FROM import_receipt WHERE id=1').one();
      if(proof.size===size && proof.sha256===sha256 && state.revision===1 && state.size===size) return {verified:true};
      throw new Error('Import receipt mismatch');
    }
    if (!state.ready || state.revision!==1 || state.size!==size || state.verified_import) throw new Error('Import cannot be verified');
    // Hash the durable bytes here; a client-provided receipt alone is not proof.
    // DigestStream retains only the hash state, even for a 1 GiB database.
    const epoch=this.sql.exec('SELECT epoch FROM authority WHERE id=1').one().epoch;
    const stream=new crypto.DigestStream('SHA-256'), writer=stream.getWriter();
    try {
      for(let offset=0;offset<size;offset+=256*1024) {
        const end=Math.min(size,offset+256*1024);
        const pages=this.sql.exec("SELECT offset,bytes FROM pages WHERE file='main.db' AND offset>=? AND offset<? ORDER BY offset",offset,end).toArray();
        let next=offset;
        for(const page of pages) {
          if(page.offset!==next || page.bytes.byteLength!==4096) throw new Error('Import page missing or malformed');
          await writer.write(page.bytes);next+=4096;
        }
        if(next!==end) throw new Error('Import page missing');
      }
      await writer.close();
    } catch(error) {await writer.abort(error);throw error;}
    const actual=Array.from(new Uint8Array(await stream.digest),value=>value.toString(16).padStart(2,'0')).join('');
    if(actual!==sha256) throw new Error('Durable import checksum mismatch');
    this.ctx.storage.transactionSync(()=>{
      const current=this.sql.exec('SELECT epoch,revision FROM authority WHERE id=1').one();
      if(current.epoch!==epoch || current.revision!==1 || this.readiness().size!==size) throw new Error('Import changed during verification');
      this.sql.exec('INSERT INTO import_receipt VALUES(1,?,?)',size,actual);
      // The importer cannot mutate bytes covered by the verified receipt.
      // A Container controller must activate a fresh generation to write.
      this.sql.exec("UPDATE generation SET capability='' WHERE id=1");
      this.sql.exec("UPDATE authority SET epoch=epoch+1,owner='' WHERE id=1");
    });
    return {verified:true};
  }
  initializeImport(capability) {
    if (this.readiness().ready) throw new Error("Database already imported");
    return this.activate(this.generation()+1,capability);
  }

  checkGeneration(request) {
    const current=this.sql.exec("SELECT capability FROM generation WHERE id=1").one().capability;
    const supplied=request.headers.get("X-Storage-Generation") || "";
    if (!/^[a-f0-9]{64}$/.test(current) || !/^[a-f0-9]{64}$/.test(supplied) || !crypto.subtle.timingSafeEqual(new TextEncoder().encode(current),new TextEncoder().encode(supplied))) throw new Conflict();
  }

  authorized(request) {
    const supplied = request.headers.get("X-Storage-Capability") || "";
    return /^[a-f0-9]{64}$/.test(supplied) && /^[a-f0-9]{64}$/.test(this.env.STORAGE_TOKEN || "") &&
      crypto.subtle.timingSafeEqual(new TextEncoder().encode(supplied),new TextEncoder().encode(this.env.STORAGE_TOKEN));
  }
  importing(request) {
    const supplied = request.headers.get("X-Migration-Capability") || "";
    return /^[a-f0-9]{64}$/.test(supplied) && /^[a-f0-9]{64}$/.test(this.env.DEPLOYMENT_TOKEN || "") &&
      crypto.subtle.timingSafeEqual(new TextEncoder().encode(supplied), new TextEncoder().encode(this.env.DEPLOYMENT_TOKEN));
  }
  state(request) {
    this.checkGeneration(request);
    const state = this.sql.exec("SELECT * FROM authority WHERE id=1").one();
    if (!uuid(state.owner) || String(state.epoch)!==request.headers.get("X-Storage-Epoch") || state.owner!==request.headers.get("X-Storage-Owner")) throw new Conflict();
    return state;
  }

  async fetch(request) {
    if (!this.authorized(request)) return new Response("Not found",{status:404});
    const url = new URL(request.url);
    try {
      this.checkGeneration(request);
      if (url.pathname==="/claim" && request.method==="POST") {
        if (this.env.REQUIRE_IMPORT !== "false" && !this.readiness().verified_import && !this.importing(request)) {
          return new Response("Verified database import required", { status: 503 });
        }
        const owner=request.headers.get("X-Storage-Owner");
        if (!uuid(owner)) return new Response("Invalid owner",{status:400});
        return Response.json(this.ctx.storage.transactionSync(()=>{
          const state=this.sql.exec("SELECT * FROM authority WHERE id=1").one();
          if (state.owner!=="" && state.owner!==owner) throw new Conflict();
          this.sql.exec("UPDATE authority SET owner=? WHERE id=1",owner);
          return this.sql.exec("SELECT epoch,revision FROM authority WHERE id=1").one();
        }));
      }
      const current = this.state(request);
      if (url.pathname==="/meta" && request.method==="GET") return Response.json({epoch:current.epoch,revision:current.revision,files:this.sql.exec("SELECT name,size FROM files WHERE name='main.db'").toArray()});
      if (url.pathname==="/range" && request.method==="GET") {
        if (String(current.revision)!==request.headers.get("X-Storage-Revision")) throw new Conflict();
        const offset=Number(url.searchParams.get("offset")), length=Number(url.searchParams.get("length"));
        if (url.searchParams.get("name")!=="main.db" || !integer(offset,0,1024**3) || offset%4096 || !integer(length,1,256*1024)) return new Response("Invalid range",{status:400});
        const file=this.sql.exec("SELECT size FROM files WHERE name='main.db'").toArray()[0];
        if (!file || offset>=file.size) return new Response("Not found",{status:404});
        const bytes=new Uint8Array(Math.min(length,file.size-offset)); let expected=offset;
        for (const page of this.sql.exec("SELECT offset,bytes FROM pages WHERE file='main.db' AND offset>=? AND offset<? ORDER BY offset",offset,offset+bytes.length)) {
          if (page.offset!==expected || page.bytes.byteLength!==4096) throw new Error("Durable SQLite page missing or malformed");
          bytes.set(new Uint8Array(page.bytes).subarray(0,bytes.length-(page.offset-offset)),page.offset-offset);
          expected+=4096;
        }
        if (expected<offset+bytes.length) throw new Error("Durable SQLite page missing");
        return new Response(bytes,{headers:{"content-type":"application/octet-stream","cache-control":"no-store"}});
      }
      if (request.method!=="POST" || !["/begin","/stage","/commit"].includes(url.pathname)) return new Response("Not found",{status:404});
      const input=await boundedJSON(request);
      if (!input || !uuid(input.id)) return new Response("Invalid operation",{status:400});
      return Response.json(this.ctx.storage.transactionSync(()=>{
        const state=this.state(request); // Body read yielded; fencing must be checked again.
        if (url.pathname==="/begin") {
          if (!integer(input.revision,0,Number.MAX_SAFE_INTEGER) || !integer(input.size,0,1024**3) || input.size%4096 || !integer(input.count,0,input.size/4096)) throw new Invalid();
          if (state.revision!==input.revision) throw new Conflict();
          const existing=this.sql.exec("SELECT * FROM pending WHERE id=?",input.id).toArray()[0];
          if (existing && (existing.epoch!==state.epoch || existing.revision!==input.revision || existing.size!==input.size || existing.count!==input.count)) throw new Conflict();
          if (!existing) {
            this.sql.exec("DELETE FROM pending"); // One engine and one in-flight transaction.
            this.sql.exec("INSERT INTO pending VALUES(?,?,?,?,?)",input.id,state.epoch,input.revision,input.size,input.count);
          }
          return {revision:state.revision};
        }
        if (url.pathname==="/commit" && state.last_id===input.id && state.revision===input.revision+1) return {revision:state.revision};
        const pending=this.sql.exec("SELECT * FROM pending WHERE id=?",input.id).toArray()[0];
        if (!pending || pending.epoch!==state.epoch || pending.revision!==state.revision) throw new Conflict();
        if (url.pathname==="/stage") {
          if (!Array.isArray(input.pages) || input.pages.length>128) throw new Invalid();
          for (const page of input.pages) {
            if (!integer(page.offset,0,pending.size-4096) || page.offset%4096 || typeof page.data!=="string" || page.data.length!==5464) throw new Invalid();
            let bytes; try { bytes=Uint8Array.from(atob(page.data),c=>c.charCodeAt(0)); } catch { throw new Invalid(); }
            if (bytes.length!==4096) throw new Invalid();
            const old=this.sql.exec("SELECT bytes FROM staged WHERE id=? AND offset=?",input.id,page.offset).toArray()[0];
            if (old && !equalBytes(new Uint8Array(old.bytes),bytes)) throw new Conflict();
            if (!old) this.sql.exec("INSERT INTO staged VALUES(?,?,?)",input.id,page.offset,bytes.buffer);
          }
          if (this.sql.exec("SELECT COUNT(*) AS count FROM staged WHERE id=?",input.id).one().count>pending.count) throw new Invalid();
          return {revision:state.revision};
        }
        if (input.revision!==state.revision || this.sql.exec("SELECT COUNT(*) AS count FROM staged WHERE id=?",input.id).one().count!==pending.count) throw new Conflict();
        this.sql.exec("INSERT INTO files VALUES('main.db',?) ON CONFLICT(name) DO UPDATE SET size=excluded.size",pending.size);
        this.sql.exec("DELETE FROM pages WHERE file='main.db' AND offset>=?",pending.size);
        this.sql.exec("INSERT INTO pages(file,offset,bytes) SELECT 'main.db',offset,bytes FROM staged WHERE id=? ON CONFLICT(file,offset) DO UPDATE SET bytes=excluded.bytes",input.id);
        if (this.sql.exec("SELECT COUNT(*) AS count FROM pages WHERE file='main.db'").one().count!==pending.size/4096) throw new Invalid();
        this.sql.exec("UPDATE authority SET revision=revision+1,last_id=? WHERE id=1",input.id);
        this.sql.exec("DELETE FROM pending WHERE id=?",input.id);
        return {revision:state.revision+1};
      }));
    } catch(error) {
      if (error instanceof Conflict) return new Response("Writer or revision conflict",{status:409});
      if (error instanceof Invalid) return new Response("Invalid operation",{status:400});
      throw error;
    }
  }
}
class Conflict extends Error {}
class Invalid extends Error {}
function uuid(value) { return /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(value||""); }
function integer(value,min,max) { return Number.isSafeInteger(value) && value>=min && value<=max; }
function equalBytes(a,b) { return a.length===b.length && a.every((value,index)=>value===b[index]); }
async function boundedJSON(request) {
  const reader=request.body?.getReader(); if (!reader) return null;
  const chunks=[]; let length=0;
  for (;;) {
    const {value,done}=await reader.read(); if (done) break;
    length+=value.length; if (length>1024*1024) { await reader.cancel(); return null; } chunks.push(value);
  }
  const bytes=new Uint8Array(length); let offset=0;
  for (const chunk of chunks) { bytes.set(chunk,offset); offset+=chunk.length; }
  try { return JSON.parse(new TextDecoder("utf-8",{fatal:true}).decode(bytes)); } catch { return null; }
}
// Private, synthetic qualification endpoint only. Production Workers expose
// this authority solely through a service binding or Container outbound route.
export default {async fetch(request,env) {
 const authority=env.FILES.getByName("qualification");
 if (new URL(request.url).pathname==="/__activate" && request.method==="POST") {
  if (request.headers.get("X-Storage-Capability")!==env.STORAGE_TOKEN) return new Response("Not found",{status:404});
  const input=await boundedJSON(request); if (!input) return new Response("Invalid generation",{status:400});
  return Response.json(await authority.activate(input.sequence ?? (await authority.generation())+1,input.capability));
 }
 return authority.fetch(request);
}};
