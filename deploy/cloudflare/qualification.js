import {bifrostObjects} from './r2.js';
export {SQLiteFiles} from './storage.js';
// Local workerd qualification only. Real Containers own generation activation.
export default {async fetch(request,env) {
 const url=new URL(request.url),parts=url.pathname.split('/');
 if(parts[1]==='objects') {url.pathname='/'+parts.slice(2).join('/');return bifrostObjects(new Request(url,request),env);}
 if(!['config','logs'].includes(parts[1])) return new Response(null,{status:404});
 const authority=env.FILES.getByName(parts[1]);url.pathname='/'+parts.slice(2).join('/');
 if(url.pathname==='/__readiness' && request.method==='GET') {
  if(request.headers.get('X-Storage-Capability')!==env.STORAGE_TOKEN) return new Response(null,{status:404});
  return Response.json(await authority.readiness());
 }
 if(url.pathname==='/__activate' && request.method==='POST') {
  if(request.headers.get('X-Storage-Capability')!==env.STORAGE_TOKEN) return new Response(null,{status:404});
  const input=await request.json();return Response.json(await authority.activate((await authority.generation())+1,input.capability));
 }
 return authority.fetch(new Request(url,request));
}};
