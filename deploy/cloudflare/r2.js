export async function bifrostObjects(request,env) {
 const supplied=request.headers.get('X-Bifrost-R2-Capability')||'';
 if(!/^[a-f0-9]{64}$/.test(supplied) || !/^[a-f0-9]{64}$/.test(env.BIFROST_R2_CAPABILITY||'') ||
  !crypto.subtle.timingSafeEqual(new TextEncoder().encode(supplied),new TextEncoder().encode(env.BIFROST_R2_CAPABILITY)) ||
  request.headers.get('X-Bifrost-R2-Bucket')!==env.BIFROST_R2_BUCKET) return new Response(null,{status:404});
 const url=new URL(request.url),bucket=env.BIFROST_DATA,key=url.searchParams.get('key');
 if(url.pathname==='/health' && request.method==='HEAD') {await bucket.list({limit:1});return new Response(null,{status:200});}
 if(url.pathname==='/list' && request.method==='GET') {
  const page=await bucket.list({prefix:url.searchParams.get('prefix')||'',cursor:url.searchParams.get('cursor')||undefined,limit:1000});
  return Response.json({objects:page.objects.map(object=>({key:object.key,lastModified:object.uploaded.toISOString()})),truncated:page.truncated,cursor:page.truncated?page.cursor:''});
 }
 if(url.pathname==='/delete' && request.method==='POST') {
  const length=Number(request.headers.get('Content-Length'));if(!request.headers.has('Content-Length') || !Number.isSafeInteger(length) || length<0 || length>1024*1024)return new Response(null,{status:413});
  const text=await request.text();if(new TextEncoder().encode(text).byteLength>1024*1024)return new Response(null,{status:413});
  let keys;try {keys=JSON.parse(text);}catch{return new Response(null,{status:400});}
  if(!Array.isArray(keys) || keys.length>1000 || keys.some(key=>typeof key!=='string' || !key || key.length>1024)) return new Response(null,{status:400});
  await bucket.delete(keys);return new Response(null,{status:204});
 }
 if(url.pathname!=='/object' || !key || key.length>1024) return new Response(null,{status:400});
 if(request.method==='GET') {
  const object=await bucket.get(key);if(!object)return new Response(null,{status:404});
  return new Response(object.body,{headers:{'Content-Type':'application/octet-stream','X-Object-Encoding':object.httpMetadata?.contentEncoding||'','Cache-Control':'no-store'}});
 }
 if(request.method==='PUT') {
  const size=Number(request.headers.get('Content-Length'));
  if(!request.headers.has('Content-Length') || !Number.isSafeInteger(size) || size<0 || size>100*1024*1024)return new Response(null,{status:413});
  const encoding=request.headers.get('X-Object-Encoding')||'';if(encoding && encoding!=='gzip')return new Response(null,{status:400});
  const tags=request.headers.get('X-Object-Tags')||'';if(new TextEncoder().encode(tags).byteLength>2000)return new Response(null,{status:413});
  await bucket.put(key,request.body||new Uint8Array(),{httpMetadata:{contentType:'application/octet-stream',contentEncoding:encoding||undefined},customMetadata:tags?{bifrost_tags:tags}:undefined});
  return new Response(null,{status:204});
 }
 if(request.method==='DELETE') {await bucket.delete(key);return new Response(null,{status:204});}
 return new Response(null,{status:405});
}
