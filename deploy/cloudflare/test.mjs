import {Miniflare,convertV4MiniflareOptions} from 'miniflare';
import {mkdtemp,readFile,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {spawn} from 'node:child_process';
import {randomBytes} from 'node:crypto';
import assert from 'node:assert/strict';
const root=fileURLToPath(new URL('../../',import.meta.url));
const directory=await mkdtemp(join(tmpdir(),'bifrost-native-'));
const token=randomBytes(32).toString('hex');
async function run(command,args,extra={}) {
 const result=await new Promise((resolve,reject)=>{
  const child=spawn(command,args,{cwd:join(root,'framework'),env:{...process.env,...extra},stdio:['ignore','pipe','pipe']});let output='';
  child.stdout.on('data',v=>output+=v);child.stderr.on('data',v=>output+=v);child.on('error',reject);child.on('exit',code=>resolve({code,output}));
 });
 assert.equal(result.code,0,result.output);return result.output;
}
let runtime;
try {
 for(const pkg of ['configstore','logstore','objectstore']) await run('go',['test','-c','-tags','cloudflare','-ldflags=-X github.com/ncruces/go-sqlite3/driver.driverName=cloudflare-sqlite','-o',join(directory,pkg+'.test'),'./'+pkg],{CGO_ENABLED:'0'});
 const modules=[];
 for(const name of ['qualification.js','storage.js','r2.js']) modules.push({type:'ESModule',path:join(root,'deploy/cloudflare',name),contents:await readFile(new URL(name,import.meta.url),'utf8')});
 const options=convertV4MiniflareOptions({modulesRoot:join(root,'deploy/cloudflare'),modules,port:18884,compatibilityDate:'2026-10-09',durableObjects:{FILES:{className:'SQLiteFiles',useSQLite:true}},r2Buckets:['BIFROST_DATA'],bindings:{STORAGE_TOKEN:token,BIFROST_R2_CAPABILITY:token,BIFROST_R2_BUCKET:'bifrost-data'},resourcePersistencePath:join(directory,'authority')});
 runtime=new Miniflare(options);
 for(const action of ['initialize','verify']) {
  if(action==='verify') {await runtime.dispose();runtime=new Miniflare(options);}
  const origin=(await runtime.ready).toString().replace(/\/$/,'');
  for(const [name,pkg,key] of [['config','configstore','BIFROST_CLOUDFLARE_CONFIG_SQL_URL'],['logs','logstore','BIFROST_CLOUDFLARE_LOGS_SQL_URL']]) {
   const generation=randomBytes(32).toString('hex');
   const activated=await runtime.dispatchFetch(`${origin}/${name}/__activate`,{method:'POST',headers:{'X-Storage-Capability':token},body:JSON.stringify({capability:generation})});assert.equal(activated.status,200);await activated.arrayBuffer();
   const output=await run(join(directory,pkg+'.test'),['-test.run=^TestCloudflareNativeMigrations$','-test.v'],{[key]:`${origin}/${name}`,SQLITE_STORAGE_TOKEN:token,SQLITE_STORAGE_GENERATION_CAPABILITY:generation,CLOUDFLARE_ACCEPTANCE_ACTION:action});
   assert.match(output,/--- PASS: TestCloudflareNativeMigrations/);assert.doesNotMatch(output,/--- SKIP:/);console.log(output.trim());
  }
 }
 const origin=(await runtime.ready).toString().replace(/\/$/,'');
 const output=await run(join(directory,'objectstore.test'),['-test.run=^TestCloudflareR2Native$','-test.v'],{BIFROST_CLOUDFLARE_R2_URL:origin+'/objects',BIFROST_CLOUDFLARE_R2_TOKEN:token});
 assert.match(output,/--- PASS: TestCloudflareR2Native/);assert.doesNotMatch(output,/--- SKIP:/);console.log(output.trim());
 console.log('Actual Bifrost migrations, transactions, timestamp precision, R2 and workerd replacement passed');
} finally {if(runtime)await runtime.dispose();await rm(directory,{recursive:true,force:true});}
