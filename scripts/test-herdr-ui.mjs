#!/usr/bin/env node
// Exercises the installed custom Herdr mods with in-memory UI/CLI adapters only.
// Override HERDR_FAE_MOD_DIR to validate an unpacked extension before installation.
import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';
import { homedir } from 'node:os';
const modDir = resolve(process.env.HERDR_FAE_MOD_DIR || `${homedir()}/.config/herdr-app/mods/fae`);
const shared = new Map(), clicks = new Map(), texts = [], calls = [];
let snapshotFile = null, execute = async () => ({code: 0, stdout: '{}', stderr: ''});
function element() {
  let id = '';
  const object = new Proxy({}, {get: (_target, name) => {
    if (name === 'then') return undefined;
    return (...args) => {
      if (name === 'id') id = args[0];
      if (name === 'on_click') clicks.set(id, args[0]);
      if (name === 'child' && typeof args[0] === 'string') texts.push(args[0]);
      if (name === 'when' && args[0]) args[1](object);
      return object;
    };
  }});
  return object;
}
globalThis.window = {paint_path: element};
const state = {new: (config = {}) => { let value = config.value || ''; return {value: () => value, set_value: next => {value = next;}}; }};
const ui = {new: () => element(), markdown: () => element()};
const theme = {foreground:'#ffffff', muted:'#888888', background:'#000000', surface:'#111111', raised:'#222222', accent:'#00ffff', border:'#555555', done:'#00ff00', working:'#ffff00', blocked:'#ff0000'};
globalThis.__faeUITest = {
 element, state, ui,
 exec: async (bin, args, options) => { calls.push({bin,args,options}); return execute(bin,args,options); },
 shared: key => shared.get(key), setShared: (key,value) => shared.set(key,value),
 readTextFile: async () => snapshotFile,
 theme: () => theme,
};
const herdr = `const m=globalThis.__faeUITest;
export const exec=(...a)=>m.exec(...a),shared=(k)=>m.shared(k),setShared=(k,v)=>m.setShared(k,v),readTextFile=(...a)=>m.readTextFile(...a),theme=()=>m.theme();
export const machines=()=>[],spaces=()=>[],terminals=()=>[],panes=()=>[],focused=()=>null,props=()=>({}),window=()=>({});
export const notify=()=>{},run=()=>{},hc=async()=>({code:0,stdout:'{}',stderr:''}),openOverlay=()=>{},closeOverlay=()=>{},playSound=()=>{},setAgentOrder=()=>{},writeTextFile=async()=>{};`;
const gpui = `const m=globalThis.__faeUITest;export class View {};
export const div=m.element,svg=m.element,image=m.element,list=m.element,uniform_list=m.element;
export const PathBuilder={stroke:m.element,fill:m.element};export const Background=new Proxy({}, {get:()=>m.element});`;
const base = `const m=globalThis.__faeUITest;export const h_flex=m.element,v_flex=m.element;
export const InputState=m.state,EditorState=m.state,TextareaState=m.state;
export const Input=m.ui,Editor=m.ui,Textarea=m.ui,TextView=m.ui,Button=m.ui,Switch=m.ui,Checkbox=m.ui,Tabs=m.ui,Tab=m.ui,Progress=m.ui;`;
const sources = new Map([['herdr',herdr],['gpui-kit',gpui],['gpui-base',base]]);
registerHooks({
 resolve(specifier, context, next) { return sources.has(specifier) ? {url:`fae-test:${specifier}`,shortCircuit:true} : next(specifier,context); },
 load(url, context, next) { return url.startsWith('fae-test:') ? {format:'module',source:sources.get(url.slice(9)),shortCircuit:true} : next(url,context); }
});
const load = name => import(pathToFileURL(`${modDir}/${name}`).href);
const {snapshotHealth, pendingLandingGates, landingReceiptGates} = await load('ui_state.js');
const {normalizeSnapshot,refreshSnapshot} = await load('rpc.js');
const {renderLanding,initPoolsLandState,renderPools} = await load('pools_land.js');
const {renderTopBar} = await load('chrome.js');
const task={id:'real-done',state:'Done',title:'Real task'};
const current=()=>({reachable:true,ts:Date.now(),status:{state:'RUNNING'},board:{items:[task]}});
for (const [raw,wanted] of [[null,'DISCONNECTED'],[{reachable:false},'DISCONNECTED'],[{...current(),ts:Date.now()-11000},'STALE'],[{...current(),partial:true,errors:{Board:'read refused'}},'DEGRADED'],[{ts:Date.now(),reachable:true},'UNKNOWN'],[current(),'CURRENT']]) {
 assert.equal(snapshotHealth(raw),wanted);
 if(raw) { const normalized=normalizeSnapshot(raw); assert.equal(normalized.health,wanted); if(wanted!=='CURRENT') assert.notEqual(normalized.status.drive_state,'RUNNING'); }
}
assert.equal(normalizeSnapshot({reachable:false}).host.avail_ram_gib,null);
assert.equal(normalizeSnapshot({reachable:false}).host.guard_state,'UNKNOWN');
for(const raw of [{reachable:false}, {...current(),ts:Date.now()-11000},{...current(),partial:true}]) {
 texts.length=0;renderTopBar({}, {}, theme,normalizeSnapshot(raw));assert(!texts.includes('RUN'));assert(texts.includes(snapshotHealth(raw)));
}
assert.equal(normalizeSnapshot(current()).tasks[0].id,task.id);
assert.equal(normalizeSnapshot({...current(),board:{items:[]}}).tasks.length,0);
const freshHost=normalizeSnapshot({...current(),host:{samples:[{memory_available_bytes:8*1024**3,load_per_core:0.3}]}});
assert.equal(freshHost.host.avail_ram_gib,8);assert.equal(freshHost.host.cpu_load_per_core,0.3);
snapshotFile=JSON.stringify({...current(),ts:Date.now()-20000});execute=async()=>({code:1,stdout:'',stderr:'daemon unreachable'});
assert.equal((await refreshSnapshot()).health,'DISCONNECTED');
const view={role:'land'};initPoolsLandState(view);
assert.deepEqual(view._landingViewState.selectedBatchIds,[]);
assert(Object.values(view._landingViewState.gateStatuses).every(g=>g.status==='unknown'));
assert(Object.values(landingReceiptGates({ok:true})).every(g=>g.status==='unknown'));
assert.equal(landingReceiptGates({results:[{gates:[{gate:'build',passed:true,applicable:true,count:2}]}]}).build.status,'pass');
assert.equal(landingReceiptGates({gates:[{gate:'build',passed:true,applicable:true,count:0}]}).build.status,'fail');
assert.equal(landingReceiptGates({gates:[{gate:'build',passed:true,applicable:false,count:0}]}).build.status,'not-applicable');
let pending=[];const cx={notify:()=>{},spawn:fn=>{pending.push(Promise.resolve(fn(cx)));}};
async function click(id) {pending=[];clicks.get(id)({},cx);await Promise.all(pending);}
execute=async (_bin,args)=>({code:0,stdout:JSON.stringify(args.includes('message')?{id:'msg'}:args.includes('land')?{results:[{gates:[{gate:'build',passed:true,applicable:true,count:3}]}]}:{}),stderr:''});
const snap=normalizeSnapshot(current());shared.set('fae.snapshot',snap);
renderLanding(view,cx,theme,snap);calls.length=0;
await click('btn-land-selected');
const mutation=calls.find(c=>c.args.includes('land'));assert(mutation);assert.equal(mutation.args[mutation.args.indexOf('--task')+1],task.id);
assert(calls.every(c=>!c.args.includes('[object Object]')));
assert.equal(view._landingViewState.gateStatuses.build.status,'pass');assert.equal(view._landingViewState.gateStatuses.push.status,'unknown');
view._landingViewState.gateStatuses=pendingLandingGates();await click('btn-gates-self-test');assert(Object.values(view._landingViewState.gateStatuses).every(g=>g.status==='unknown'));
texts.length=0;const empty=normalizeSnapshot({...current(),board:{items:[]}});renderLanding(view,cx,theme,empty);calls.length=0;await click('btn-land-selected');assert.equal(calls.length,0);assert(!texts.some(t=>/T-142|9f84c2b|0 Policy Violations/.test(t)));
renderLanding(view,cx,theme,normalizeSnapshot({...current(),reachable:false}));calls.length=0;await click('btn-land-selected');assert.equal(calls.length,0);
execute=async()=>({code:1,stdout:'',stderr:'provenance unavailable'});await click('btn-verify-provenance');assert.deepEqual(view._landingViewState.provenanceChain,[]);
texts.length=0;renderPools({role:'pools'},cx,theme,normalizeSnapshot({...current(),board:{items:[]}}));assert(!texts.some(t=>/T-142|anthropic-prod|openai-codex/.test(t)));
texts.length=0;renderPools({role:'pools'},cx,theme,normalizeSnapshot({...current(),reachable:false,host:{avail_ram_gib:64}}));assert(!texts.some(t=>/Floor.*✓/.test(t)));
console.log('PASS: snapshot freshness, disconnect/partial health, failed refresh, real task selection, unknown gate/provenance state, receipt-only passes, no actionable sample data');
