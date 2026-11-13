import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["models", "storage"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const user = (await import("@testing-library/user-event")).default;
const { ModelDetailModal } = await import("../src/pages/ModelsPageParts.tsx");
const originalFetch = globalThis.fetch;
const current = { id: 9, source: "url", source_url: "http://models.private:8080/qwen/", source_id: "qwen", revision: "", name: "qwen", arch: "", quant_label: "", weight_gib: 0, created_at: "2026-11-12T00:00:00Z", nodes: [], storages: [] };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), {status, headers:{"Content-Type":"application/json"}});
type Call = { path: string; method: string; body?: unknown };
function fixture(restrict = true, options: { editable?: boolean; status?: "blocked" | "unresolved"; conflict?: boolean; role?: string; stored?: typeof current; policyFailure?: boolean; browser?: boolean } = {}) {
 const stored = options.stored ?? current;
 const sourceHost = stored.source === "url" ? "models.private" : "mirror.private";
 const calls: Call[] = [];
 const unexpected: string[] = [];
 let policy = { restrict_private_hosts: restrict, allow_private_hosts: ["retained.corp"], editable: options.editable ?? true, revision: "r1" };
 globalThis.fetch = (async (input, init) => {
  const call: Call = {path:String(input),method:init?.method ?? "GET",body:init?.body ? JSON.parse(String(init.body)) : undefined};calls.push(call);
  if(call.path === "/api/v1/auth/me") return json({id:1,username:options.role ?? "admin",role:options.role ?? "admin"});
  if(call.path.startsWith("/api/v1/server/outbound-policy") && options.role === "viewer") return json({code:"PERMISSION_DENIED",error:"administrator permission required"},403);
  if(call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return options.policyFailure ? json({error:"policy unavailable"},503) : json(policy);
  if(call.path === "/api/v1/server/outbound-policy/check") {
   const hosts = (call.body as {hosts:string[]}).hosts;
   return json({items:hosts.map(host => ({host,status:options.status === "blocked" ? "blocked" : !policy.restrict_private_hosts || policy.allow_private_hosts.includes(host) ? "allowed" : options.status ?? "needs_allow"})),total:hosts.length});
  }
  if(call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
   const next=call.body as typeof policy;
   if(!policy.editable) return json({code:"SERVER_OUTBOUND_POLICY_READ_ONLY",error:"read-only policy"},409);
   if(options.conflict || next.revision !== policy.revision) return json({code:"SERVER_OUTBOUND_POLICY_CONFLICT",error:"policy changed"},409);
   policy={...policy,...next,revision:"r2"};return json(policy);
  }
  if(call.path === "/api/v1/models/9/metadata/refresh" && call.method === "POST") {
   return (stored.source === "huggingface" && (call.body as {browser_metadata?:boolean})?.browser_metadata) || !policy.restrict_private_hosts || policy.allow_private_hosts.includes(sourceHost)
    ? json({...stored,readme:"# refreshed metadata"})
    : json({code:"MODEL_METADATA_UNREACHABLE",error:"management node could not reach models.private",params:{hosts:"models.private",source:"url"}},502);
  }
  if(call.path.startsWith("/api/v1/pull/metadata-sources?source=")) return json({source:stored.source,bases:["http://mirror.private"]});
  if(call.path.startsWith("http://mirror.private/")) {
   if(!options.browser) return json({error:"browser cannot reach mirror"},503);
   if(call.path.includes("/raw/") && call.path.endsWith("/config.json")) return json({architectures:["Qwen2ForCausalLM"]});
   if(call.path.endsWith("README.md")) return new Response("# read by browser");
   if(call.path.includes("/api/models/")) return json({siblings:[]});
   return json({error:"Not Found"},404);
  }
  unexpected.push(call.path);return json({error:"Not Found"},404);
 }) as typeof fetch;
 return {calls,unexpected};
}
function show(onChanged: (...args: unknown[]) => void, model = current) {
 render(<I18nextProvider i18n={i18n}><ModelDetailModal model={model} onClose={()=>{}} onDownload={()=>{}} onCopy={()=>{}} onRegisterPath={()=>{}} onChanged={onChanged} /></I18nextProvider>);
}
afterEach(()=>{cleanup();globalThis.fetch=originalFetch;});

test("model detail refresh lets an administrator explicitly allow the current source before retrying",async()=>{
 const {calls,unexpected}=fixture();const changed:unknown[]=[];show(m=>changed.push(m));const actor=user.setup();
 await actor.click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
 const checkbox=await screen.findByRole("checkbox",{name:/models\.private/});
 assert.equal(calls.filter(c=>c.method==="PUT").length,0,"opening permission never grants it");
 await actor.click(checkbox);
 await actor.click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
 await waitFor(()=>assert.equal(changed.length,1));
 assert.deepEqual(calls.filter(c=>c.method==="PUT").map(c=>c.body),[{restrict_private_hosts:true,allow_private_hosts:["retained.corp","models.private"],revision:"r1"}]);
 for(const c of calls.filter(c=>c.path.endsWith("/check")))assert.deepEqual(c.body,{hosts:["models.private"]});
 assert.deepEqual(unexpected,[]);
});

test("global SSRF restrictions off keeps metadata refresh direct without host checks or permission writes",async()=>{
 const {calls,unexpected}=fixture(false);const changed:unknown[]=[];show(m=>changed.push(m));
 await user.setup().click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
 await waitFor(()=>assert.equal(changed.length,1));
 assert.equal(screen.queryByRole("checkbox"),null);
 assert.deepEqual(calls.filter(c=>c.path.endsWith("/check")||c.method==="PUT"),[]);
 assert.deepEqual(unexpected,[]);
});

test("an unchecked metadata source is never added to the global allowlist",async()=>{
 const {calls}=fixture();const changed:unknown[]=[];show(m=>changed.push(m));const actor=user.setup();
 const refresh=()=>screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)});
 await actor.click(refresh());await screen.findByRole("checkbox",{name:/models\.private/});
 await actor.click(refresh());
 await waitFor(()=>assert.ok(calls.some(c=>c.path.endsWith("/metadata/refresh"))));
 assert.equal(changed.length,0);assert.deepEqual(calls.filter(c=>c.method==="PUT"),[]);
});

test("a policy conflict stops a selected grant before metadata is refreshed",async()=>{
 const {calls}=fixture(true,{conflict:true});show(()=>{});const actor=user.setup();
 const refresh=()=>screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)});
 await actor.click(refresh());await actor.click(await screen.findByRole("checkbox",{name:/models\.private/}));await actor.click(refresh());
 await waitFor(()=>assert.ok(screen.getAllByRole("alert").some(n=>/changed/i.test(n.textContent??""))));
 assert.equal(calls.filter(c=>c.method==="PUT").length,1);
 assert.deepEqual(calls.filter(c=>c.path.endsWith("/metadata/refresh")),[]);
});

test("permanently blocked and read-only sources cannot be granted during metadata refresh",async()=>{
 for(const options of [{status:"blocked" as const},{editable:false}]) {
  const {calls}=fixture(true,options);show(()=>{});const actor=user.setup();
  await actor.click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
  if(options.editable===false) assert.equal((await screen.findByRole("checkbox",{name:/models\.private/})).getAttribute("aria-disabled"),"true");
  else {await screen.findByText(/cannot be allowed by the outbound policy/i);assert.equal(screen.queryByRole("checkbox"),null);}
  assert.deepEqual(calls.filter(c=>c.method==="PUT"),[]);cleanup();
 }
});

test("ModelScope refresh authorizes the actual management-node mirror from source rules",async()=>{
 const stored={...current,source:"modelscope",source_url:""};const {calls,unexpected}=fixture(true,{stored});const changed:unknown[]=[];show(m=>changed.push(m),stored);const actor=user.setup();
 await actor.click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
 await actor.click(await screen.findByRole("checkbox",{name:/mirror\.private/}));
 await actor.click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
 await waitFor(()=>assert.equal(changed.length,1));
 assert.deepEqual(calls.filter(c=>c.method==="PUT").map(c=>c.body),[{restrict_private_hosts:true,allow_private_hosts:["retained.corp","mirror.private"],revision:"r1"}]);
 for(const c of calls.filter(c=>c.path.endsWith("/check")))assert.deepEqual(c.body,{hosts:["mirror.private"]});
 assert.deepEqual(unexpected,[]);
});

test("a successful Hugging Face browser refresh does not request server source authorization",async()=>{
 const stored={...current,source:"huggingface",source_url:""};const {calls,unexpected}=fixture(true,{stored,browser:true});const changed:unknown[]=[];show(m=>changed.push(m),stored);
 await user.setup().click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
 await waitFor(()=>assert.equal(changed.length,1));
 assert.deepEqual(calls.filter(c=>c.path.startsWith("/api/v1/server/outbound-policy")),[]);
 assert.equal((calls.find(c=>c.path.endsWith("/metadata/refresh"))?.body as {browser_metadata?:boolean}).browser_metadata,true);
 assert.equal(screen.queryByRole("checkbox"),null);assert.deepEqual(unexpected,[]);
});

test("a member and an unavailable policy preflight preserve the original backend refresh action",async()=>{
 for(const options of [{role:"viewer"},{policyFailure:true}]) {
  const {calls,unexpected}=fixture(false,options);const changed:unknown[]=[];show(m=>changed.push(m));
  await user.setup().click(screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)}));
  await waitFor(()=>assert.equal(changed.length,1));
  assert.deepEqual(calls.filter(c=>c.path.endsWith("/check")||c.method==="PUT"),[]);assert.deepEqual(unexpected,[]);cleanup();
 }
});

test("switching global SSRF restrictions off discards a pending host selection and refreshes normally",async()=>{
 const {calls}=fixture();const changed:unknown[]=[];show(m=>changed.push(m));const actor=user.setup();
 const refresh=()=>screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)});
 await actor.click(refresh());await actor.click(await screen.findByRole("checkbox",{name:/models\.private/}));
 const reply=await fetch("/api/v1/server/outbound-policy",{method:"PUT",body:JSON.stringify({restrict_private_hosts:false,allow_private_hosts:["retained.corp"],revision:"r1"})});assert.equal(reply.status,200);
 const checksBefore=calls.filter(c=>c.path.endsWith("/check")).length;
 await actor.click(refresh());await waitFor(()=>assert.equal(changed.length,1));
 assert.equal(calls.filter(c=>c.method==="PUT").length,1,"only the administrator's global toggle changed the policy");
 assert.equal(calls.filter(c=>c.path.endsWith("/check")).length,checksBefore);
 assert.equal(screen.queryByRole("checkbox"),null);
});

test("an unavailable policy after explicit selection never grants or starts the metadata request",async()=>{
 const state={policyFailure:false};const {calls}=fixture(true,state);show(()=>{});const actor=user.setup();
 const refresh=()=>screen.getByRole("button",{name:new RegExp(locale.models.detail.refreshMetadata)});
 await actor.click(refresh());await actor.click(await screen.findByRole("checkbox",{name:/models\.private/}));state.policyFailure=true;
 await actor.click(refresh());await waitFor(()=>assert.ok(screen.getAllByRole("alert").some(n=>/policy unavailable/i.test(n.textContent??""))));
 assert.deepEqual(calls.filter(c=>c.method==="PUT"||c.path.endsWith("/metadata/refresh")),[]);
});
