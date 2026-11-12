#!/usr/bin/env python3
"""Opt-in two-task real Claude Code (or Codex) + herdr-core + Dolt acceptance with drained restart and opt-in crash/loop/resident checks.
All code/effects remain in a disposable repository; credentials are not copied.
"""
import argparse,signal,threading,datetime,hashlib,json,os,pathlib,shutil,subprocess,tempfile,time,http.server,urllib.parse,tarfile
p=argparse.ArgumentParser();p.add_argument('--provider',choices=['claude','codex'],default='claude');p.add_argument('--model');p.add_argument('--claude',default=shutil.which('claude'));p.add_argument('--claude-config-dir',default=os.environ.get('CLAUDE_CONFIG_DIR',str(pathlib.Path.home()/'.claude')));p.add_argument('--dolt',required=True);p.add_argument('--codex',default=shutil.which('codex'));p.add_argument('--herdr',default=shutil.which('herdr-core') or shutil.which('herdr'));p.add_argument('--timeout',type=int,default=2400);p.add_argument('--evidence-dir',type=pathlib.Path);p.add_argument('--persistent-sessions',action='store_true');p.add_argument('--resident-sessions',action='store_true');p.add_argument('--provider-loop',action='store_true');p.add_argument('--crash-during-turn',action='store_true');p.add_argument('--cron-schedules',action='store_true');p.add_argument('--controller-lifecycle',action='store_true');p.add_argument('--drive-restart',action='store_true');p.add_argument('--deployment-roll',action='store_true');p.add_argument('--prophet-sessions',action='store_true');p.add_argument('--target-configs',action='store_true');p.add_argument('--retirement',action='store_true');p.add_argument('--normal-targets',action='store_true');p.add_argument('--census-cleanup',action='store_true');p.add_argument('--dynamic-provisioning',action='store_true');p.add_argument('--scratch-cleanup',action='store_true');p.add_argument('--fleet-provisioning',action='store_true');p.add_argument('--adapter-retirement',action='store_true');p.add_argument('--backup-policy',action='store_true');p.add_argument('--retention-automation',action='store_true');p.add_argument('--backup-schedules',action='store_true');p.add_argument('--backup-storage',action='store_true');a=p.parse_args()
if a.model is None:
 if a.provider!='claude':p.error('--model is required with --provider codex')
 a.model='claude-opus-5-5'
cli=a.claude if a.provider=='claude' else a.codex
if a.backup_storage:a.backup_schedules=True
if a.backup_schedules:a.retention_automation=True
if a.retention_automation:a.backup_policy=True;a.scratch_cleanup=True
if a.provider_loop or a.resident_sessions:a.persistent_sessions=True
if a.fleet_provisioning or a.adapter_retirement:a.dynamic_provisioning=True
if a.dynamic_provisioning:a.normal_targets=True
if a.normal_targets:a.target_configs=True
if a.census_cleanup:a.retirement=True
if a.deployment_roll:a.controller_lifecycle=True
# Codex announces thread/turn events; Claude Code stream-json announces one
# system/init record per turn and a terminal result record.
def is_start(e):return e.get('type') in ('thread.started','turn.started') or (e.get('type')=='system' and e.get('subtype')=='init')
def is_turn_start(e):return e.get('type')=='turn.started' or (e.get('type')=='system' and e.get('subtype')=='init')
def is_done(e):return e.get('type')=='turn.completed' or (e.get('type')=='result' and e.get('subtype')=='success' and not e.get('is_error'))
def start_summary(e):return {k:e[k] for k in ('type','subtype','thread_id','session_id') if k in e}
def task_id(name):
 return 'cron-'+hashlib.sha256(('dogfood-'+name).encode()).hexdigest()[:24]+'-0' if a.cron_schedules and not (a.normal_targets and name=='beta') else 'provider-'+name
alpha_id,beta_id=task_id('alpha'),task_id('beta')
root=pathlib.Path(__file__).resolve().parent.parent
binary=root/'bin/fae';bridge=root/('bin/fae-%s-bridge'%a.provider)
if not all([cli,a.herdr,binary.is_file(),bridge.is_file()]):p.error('build both binaries and install %s/herdr-core'%a.provider)
files=subprocess.check_output(['rg','--files','-g','*.go'],cwd=root,text=True).splitlines()+['go.mod','go.sum','scripts/dogfood.py','bin/fae',str(bridge.relative_to(root))]
source_hashes={name:hashlib.sha256((root/name).read_bytes()).hexdigest() for name in sorted(set(files))}
report=root/'.cache/dogfood-result.json';report.parent.mkdir(exist_ok=True)
if a.evidence_dir:
 a.evidence_dir=a.evidence_dir.resolve();a.evidence_dir.mkdir(parents=True,exist_ok=False)
scratch=pathlib.Path(tempfile.mkdtemp(prefix='fhd-',dir='/private/tmp' if os.uname().sysname=='Darwin' else '/var/tmp'))
repo=scratch/'repo';repo.mkdir();logs=scratch/'daemon.log';process=None;log=None;launcher_process=None;launcher_log=None
result={'provider':a.provider,'model':a.model,'herdr_version':subprocess.check_output([a.herdr,'--version'],text=True).strip(),'provider_version':subprocess.check_output([cli,'--version'],text=True).strip(),'adapter':'herdr','scratch':str(scratch),'status':'started','task_ids':[alpha_id,beta_id]}
replica_server=None;replica_thread=None;replica_uploads=[];replica_reads=[];replica_receipts={}
effect_server=None;effect_thread=None;effect_calls=[];effect_reads=[];provision_calls=[];fixture_tools={};effect_receipts={};effect_lock=threading.Lock();task_admissions={};task_targets={}
def start_effect_fixture():
 global effect_server,effect_thread,fixture_tools
 pins={'destination':'main','push_namespace':'fae/dogfood','ticket_room':'local-fixture','draft':'true'}
 class Handler(http.server.BaseHTTPRequestHandler):
  def log_message(self,*args):pass
  def send_json(self,status,value):
   raw=json.dumps(value).encode();self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
  def do_GET(self):
   path=urllib.parse.urlparse(self.path).path
   if path=='/schemas':
    descriptors=[]
    for tool in fixture_tools.values():
     args=tool['arguments'];descriptor={'name':tool['name'],'inputSchema':{'type':'object','additionalProperties':False,'required':list(args),'properties':{k:{'type':'string'} for k in args}}}
     if tool['name'] in ('review_baseline','review_read','fleet_prepare'):descriptor['outputSchema']={'type':'object'}
     descriptors.append(descriptor)
    self.send_json(200,{'server':'dogfood','tools':descriptors});return
   if path.startswith('/reads/'):
    intent=json.loads(urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)['intent'][0])
    if intent.get('tool')!='review_baseline' or intent.get('task')!=beta_id:self.send_json(403,{});return
    args=intent['arguments']
    if args.get('package')!='sample' or args.get('integration_tip')!=git('rev-parse','fae/integration'):self.send_json(403,{});return
    if a.fleet_provisioning and not provision_calls:self.send_json(409,{});return
    with effect_lock:effect_reads.append(intent)
    self.send_json(200,{'key':intent['key'],'task':intent['task'],'tool':intent['tool'],'digest':intent['digest'],'external_id':'local-baseline','state':'performed','tree':intent['tree'],'data':{'package':'sample','integration_tip':args['integration_tip'],'status':'Pass'}});return
   if path.startswith('/effects/'):
    with effect_lock:receipt=effect_receipts.get(path[len('/effects/'):])
    self.send_json(200 if receipt else 404,receipt or {});return
   self.send_json(404,{})
  def do_POST(self):
   length=int(self.headers.get('Content-Length','0'))
   if not self.path.startswith('/effects/') or length<1 or length>1048576:self.send_json(400,{});return
   intent=json.loads(self.rfile.read(length))
   if intent.get('tool')=='fleet_prepare' and a.fleet_provisioning:
    if intent.get('task')!=beta_id or intent.get('arguments')!=fixture_tools['fleet']['arguments']:self.send_json(403,{});return
    with effect_lock:
     provision_calls.append(intent)
     receipt={'key':intent['key'],'task':intent['task'],'tool':intent['tool'],'digest':intent['digest'],'external_id':'local-fleet-workspace','state':'performed','tree':intent['tree'],'data':dict(intent['arguments'])}
     effect_receipts[intent['key']]=receipt
    # Persist the operation, then fail the acknowledgment. Only Query may recover it.
    self.send_json(503,receipt);return
   if intent.get('arguments')!=pins or intent.get('task')!=beta_id or intent.get('tool')!='fixture_create':self.send_json(403,{});return
   with effect_lock:
    effect_calls.append(intent)
    receipt={'key':intent['key'],'task':intent['task'],'tool':intent['tool'],'digest':intent['digest'],'external_id':'local-dogfood-effect','state':'performed','tree':intent['tree']}
    effect_receipts[intent['key']]=receipt
   self.send_json(201,receipt)
 effect_server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
 effect_thread=threading.Thread(target=effect_server.serve_forever,daemon=True);effect_thread.start()
 tool={'endpoint':'http://127.0.0.1:'+str(effect_server.server_address[1]),'server':'dogfood','name':'fixture_create','role':'planner','arguments':pins}
 fixture_tools={'dogfood-effect':tool}
 if a.dynamic_provisioning:
  for alias,args in [('baseline',{'package':'$package','integration_tip':'$integration_tip'}),('read',{'review_id':'$review'})]:
   fixture_tools[alias]=dict(tool,name='review_'+alias,arguments=args)
 return tool
def start_replica_fixture():
 global replica_server,replica_thread
 directory=scratch/'replica';directory.mkdir(mode=0o700)
 class Handler(http.server.BaseHTTPRequestHandler):
  def log_message(self,*args):pass
  def send_json(self,status,value):
   raw=json.dumps(value).encode();self.send_response(status);self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
  def do_PUT(self):
   key=self.path.removeprefix('/backups/');size=int(self.headers.get('Content-Length','0'))
   if len(key)!=64 or any(c not in '0123456789abcdef' for c in key) or not 0<size<=1073741824:self.send_json(400,{});return
   digest=hashlib.sha256();remaining=size
   with (directory/key).open('wb') as out:
    while remaining:
     chunk=self.rfile.read(min(remaining,65536))
     if not chunk:raise RuntimeError('short replica upload')
     out.write(chunk);digest.update(chunk);remaining-=len(chunk)
   assert digest.hexdigest()==key and self.headers['Idempotency-Key']==key
   replica_uploads.append(key)
   replica_receipts[key]={'id':key,'sha256':key,'manifest_sha256':self.headers['X-Fae-Manifest-SHA256'],'size':size,'state':'stored'}
   self.send_json(503,replica_receipts[key]) # Simulate lost acknowledgement after durable storage.
  def do_GET(self):
   key=self.path.removeprefix('/backups/').removesuffix('/content');receipt=replica_receipts.get(key)
   if not receipt:self.send_json(404,{});return
   replica_reads.append(self.path)
   if self.path.endswith('/content'):
    self.send_response(200);self.send_header('Content-Length',str(receipt['size']));self.end_headers()
    with (directory/key).open('rb') as source:shutil.copyfileobj(source,self.wfile)
   else:self.send_json(200,receipt)
 replica_server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
 replica_thread=threading.Thread(target=replica_server.serve_forever,daemon=True);replica_thread.start()
 return 'http://127.0.0.1:'+str(replica_server.server_address[1])
def git(*args):return subprocess.check_output(['git','-C',str(repo),*args],text=True).strip()
# Backup, replication and retention runs pass the two-reading I/O gate (FAE-HOST-010): at least 30 s after a restart.
io_gate_wait=150
deferred=('IOPressure','IOPressureHold','HostPressure','HostPressureHold','PaneLimit','MemoryReservation')
def call(*args,timeout=30,instance='dogfood'):
 # A deferred host-admission refusal precedes any write; retry it as an operator would (bounded, recorded).
 for attempt in range(21):
  r=subprocess.run([str(binary),'--home',str(scratch/'i'),'--instance',instance,'--json',*args],cwd=repo,text=True,capture_output=True,timeout=timeout)
  # A schedule control refused during a run says to retry after it; treat it like a deferral.
  if r.returncode and attempt<20 and (any('host admission: '+k+'\n' in r.stderr+'\n' for k in deferred) or 'retry control after current run' in r.stderr):
   result.setdefault('deferred_host_refusals',[]).append({'command':' '.join(args[:2]),'refusal':r.stderr.strip(),'at':datetime.datetime.now(datetime.timezone.utc).isoformat()});time.sleep(15);continue
  if r.returncode:raise RuntimeError(r.stderr)
  return json.loads(r.stdout)
def start(recover=False):
 global process,log
 log=logs.open('a');process=subprocess.Popen([str(binary),'--home',str(scratch/'i'),'--instance','dogfood','serve','--workspace',str(repo),'--adapter','multiplexer','--live-config',str(config),'--prophet-provider',a.provider,'--prophet-model',a.model,'--dolt',a.dolt,'--foreground','--max-tasks-in-flight','2']+(['--scratch'] if a.scratch_cleanup and not (scratch/'i'/'dogfood').exists() else []),cwd=repo,stdout=log,stderr=log)
 for _ in range(200):
  if process.poll() is not None:
   if recover and _<100:
    log.close();time.sleep(.2)
    log=logs.open('a');process=subprocess.Popen([str(binary),'--home',str(scratch/'i'),'--instance','dogfood','serve','--workspace',str(repo),'--adapter','multiplexer','--live-config',str(config),'--prophet-provider',a.provider,'--prophet-model',a.model,'--dolt',a.dolt,'--foreground','--max-tasks-in-flight','2']+(['--scratch'] if a.scratch_cleanup and not (scratch/'i'/'dogfood').exists() else []),cwd=repo,stdout=log,stderr=log)
    continue
   raise RuntimeError('daemon exited; inspect '+str(logs))
  try:call('status');return
  except RuntimeError:time.sleep(.1)
 raise RuntimeError('startup timeout')
class LaunchedDaemon:
 # Controller launcher owns wait/reaping; guard PID reuse before cleanup signals.
 def __init__(self,pid):
  self.pid=pid;self.birth=self.identity()
  if not self.birth:raise RuntimeError('accepted daemon disappeared')
 def identity(self):
  r=subprocess.run(['ps','-p',str(self.pid),'-o','lstart=','-o','stat='],text=True,capture_output=True)
  row=r.stdout.strip()
  return ' '.join(row.split()[:-1]) if r.returncode==0 and row and not row.split()[-1].startswith('Z') else None
 def poll(self):return None if self.identity()==self.birth else 0
 def wait(self,timeout=30):
  until=time.monotonic()+timeout
  while self.poll() is None:
   if time.monotonic()>until:raise subprocess.TimeoutExpired('controller daemon',timeout)
   time.sleep(.1)
  return 0
 def terminate(self):
  if self.poll() is None:os.kill(self.pid,signal.SIGTERM)
 def kill(self):
  if self.poll() is None:os.kill(self.pid,signal.SIGKILL)
def controller_restart():
 global process,launcher_process,launcher_log
 launcher_dir=scratch/'launcher';launcher_dir.mkdir(mode=0o700)
 socket=launcher_dir/'runtime.sock'
 launcher_log=(scratch/'launcher.log').open('a')
 launcher_process=subprocess.Popen([str(binary),'--home',str(scratch/'i'),'--instance','dogfood','runtime-launcher','--socket',str(socket)],cwd=repo,stdout=launcher_log,stderr=launcher_log)
 until=time.monotonic()+10
 while not socket.exists():
  if launcher_process.poll() is not None or time.monotonic()>until:raise RuntimeError('runtime launcher failed')
  time.sleep(.1)
 reaper=threading.Thread(target=process.wait,daemon=True);reaper.start()
 if a.deployment_roll:
  old_pid=process.pid
  rolled=call('roll','--binary',str(binary),'--launcher-socket',str(socket),'--note','two-task dogfood guarded roll','--authorizing-message','authorization','--max-wait','25s',timeout=120)
  reaper.join(timeout=30)
  assert not reaper.is_alive() and process.returncode==0
  process=LaunchedDaemon(rolled['pid'])
  assert rolled['accepted'] and rolled['drive_rounds']>0 and rolled['pid']!=old_pid
  assert call('maintenance','status')=={'maintenance':False}
  result['controller_lifecycle']={'roll':rolled,'old_pid':old_pid,'maintenance_marker_cleared':True}
  (scratch/'controller-lifecycle.json').write_text(json.dumps(result['controller_lifecycle'],indent=2))
  return
 stopped=call('maintenance','stop','--launcher-socket',str(socket),'--note','two-task dogfood restart','--authorizing-message','authorization','--max-wait','25s',timeout=120)
 reaper.join(timeout=30)
 assert not reaper.is_alive() and process.returncode==0
 status=call('maintenance','status')
 assert status['state']=='stopped' and status['reason']=='two-task dogfood restart'
 watchdoglog=scratch/'watchdog.log'
 with watchdoglog.open('w') as stream:
  watchdog=subprocess.Popen([str(binary),'--home',str(scratch/'i'),'--instance','dogfood','watchdog','--period','100ms','--authorizing-message','authorization'],cwd=repo,stdout=stream,stderr=stream)
  try:
   statepath=scratch/'i'/'dogfood'/'run'/'watchdog-state.json';until=time.monotonic()+5
   while not statepath.exists():
    if watchdog.poll() is not None or time.monotonic()>until:raise RuntimeError('watchdog failed during maintenance')
    time.sleep(.1)
   time.sleep(.3)
   watchdogstate=json.loads(statepath.read_text())
   assert not {'daemon-dead','drive-exit','drive-stall'} & set(watchdogstate['Pending']) and watchdogstate['Heartbeat']!='0001-01-01T00:00:00Z'
  finally:
   watchdog.terminate()
   try:watchdog.wait(timeout=10)
   except subprocess.TimeoutExpired:watchdog.kill();watchdog.wait(timeout=10)
 assert 'maintenance ' in watchdoglog.read_text()
 started=call('maintenance','start','--launcher-socket',str(socket),'--authorizing-message','authorization','--max-wait','25s',timeout=120)
 process=LaunchedDaemon(started['pid'])
 assert started['accepted'] and started['drive_rounds']>0 and started['pid']!=stopped['old_pid']
 assert call('maintenance','status')=={'maintenance':False}
 result['controller_lifecycle']={'stopped':stopped,'started':started,'maintenance_marker_cleared':True,'watchdog_suppressed_dead_alert':True}
 (scratch/'controller-lifecycle.json').write_text(json.dumps(result['controller_lifecycle'],indent=2))
def exercise_drive_restart():
 until=time.monotonic()+30
 while time.monotonic()<until:
  live={task:history(task) for task in tasks}
  active_sessions={session['id'] for snap in live.values() for session in (snap['payload']['job'].get('in_flight') or {}).get('sessions',[])}
  started=set();start_events=[]
  for path in (scratch/'i'/'dogfood'/'sessions').glob('session-*/provider-*/events.jsonl'):
   launch=json.loads((path.parent.parent/'config.json').read_text());ev=entries(path)
   if launch['session_id'] in active_sessions and any(is_turn_start(e) for e in ev) and not any(is_done(e) for e in ev):
    started.add(launch['session_id']);start_events.append({'session_id':launch['session_id'],'dispatch_id':launch['dispatch_id'],'events':[start_summary(e) for e in ev if is_start(e)]})
  if started:break
  time.sleep(.1)
 else:raise RuntimeError('no active provider work for independent drive restart')
 before=call('drive','status','--authorizing-message','authorization')
 call('drive','stop','--authorizing-message','authorization')
 while time.monotonic()<until:
  stopped=call('drive','status','--authorizing-message','authorization')
  if not stopped['DriveAlive']:break
  time.sleep(.05)
 else:raise RuntimeError('drive did not stop independently')
 frozen={task:history(task) for task in tasks}
 active={task:snap['payload']['job']['in_flight'] for task,snap in frozen.items() if snap['payload']['job'].get('in_flight')}
 retained=started & {session['id'] for dispatch in active.values() for session in dispatch['sessions']}
 assert retained and stopped['PID']==before['PID'] and stopped['DoltPID']==before['DoltPID']
 time.sleep(.3)
 assert {task:history(task) for task in tasks}==frozen, 'stopped drive advanced checkpoints'
 watchdoglog=scratch/'drive-watchdog.log'
 with watchdoglog.open('w') as stream:
  watchdog=subprocess.Popen([str(binary),'--home',str(scratch/'i'),'--instance','dogfood','watchdog','--period','100ms','--authorizing-message','authorization'],cwd=repo,stdout=stream,stderr=stream)
  try:
   until=time.monotonic()+15
   while time.monotonic()<until:
    resumed=call('drive','status','--authorizing-message','authorization')
    if resumed['DriveAlive'] and resumed['DriveGeneration']>stopped['DriveGeneration']:break
    if watchdog.poll() is not None:raise RuntimeError('drive watchdog exited')
    time.sleep(.1)
   else:raise RuntimeError('watchdog did not independently rearm drive')
  finally:
   watchdog.terminate()
   try:watchdog.wait(timeout=10)
   except subprocess.TimeoutExpired:watchdog.kill();watchdog.wait(timeout=10)
 assert resumed['PID']==before['PID'] and resumed['DoltPID']==before['DoltPID']
 result['independent_drive']={'before':before,'stopped':stopped,'resumed':resumed,'retained_dispatches':{task:d['id'] for task,d in active.items()},'daemon_and_plane_unchanged':True,'retained_provider_sessions':sorted(retained),'provider_start_evidence':[e for e in start_events if e['session_id'] in retained]}
 (scratch/'drive-restart-checkpoint.json').write_text(json.dumps(frozen,indent=2))
 (scratch/'drive-restart.json').write_text(json.dumps(result['independent_drive'],indent=2))
 print('watchdog rearmed stopped drive; daemon, Dolt and active dispatch identities preserved',flush=True)
def shutdown():
 call('shutdown','--authorizing-message','authorization');assert process.wait(timeout=30)==0
 log.close()
def history(task):return call('history','--task',task)
def entries(path):
 out=[]
 for line in path.read_text().splitlines():
  try:out.append(json.loads(line))
  except json.JSONDecodeError:continue
 return out
def admit(task,prompt):
 if not a.cron_schedules or (a.normal_targets and task==beta_id):
  extra=[]
  for key,value in task_admissions.get(task,{}).items():
   flag={'target_config':'--target-config','target_revision':'--target-revision','revision':'--revision','mode':'--mode','commit_external':'--commit-external','ticket_room':'--ticket-room','destination':'--destination','push_namespace':'--push-namespace','run_id':'--run-id'}[key]
   if key=='commit_external':
    if value:extra.append(flag)
   else:extra += [flag,str(value).lower() if key=='mode' else str(value)]
  return call('run','--id',task,'--prompt',prompt,'--authorizing-message','authorization',*extra)
 name=goals[task][0];schedule_id='dogfood-'+name
 anchor=datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=2)
 spec={'id':schedule_id,'kind':'cron','anchor':anchor.isoformat(),'interval_seconds':60,'end':(anchor+datetime.timedelta(seconds=1)).isoformat(),'catch_up_limit':1,'template':{'workflow_id':'default','revision':0,'content':prompt,'priority':10,'mode':'Local'}}
 spec['template'].update(task_admissions.get(task,{}))
 path=scratch/(schedule_id+'.json');path.write_text(json.dumps(spec))
 call('schedule','create','--kind','cron','--spec',str(path),'--authorizing-message','authorization')
 until=time.monotonic()+30
 while time.monotonic()<until:
  row=call('schedule','inspect','--id',schedule_id)
  if row['admitted']==1:
   assert row['last_task']==task and row['next_occurrence']==1
   return history(task)
  if row.get('error'):raise RuntimeError('cron admission: '+row['error'])
  time.sleep(.2)
 raise RuntimeError('cron admission timeout')
crash_sessions=set();loop_snapshot=None

try:
 git('init');git('config','user.name','Fae Dogfood');git('config','user.email','fae@localhost')
 (repo/'go.mod').write_text('module example.com/faedogfood\n\ngo 1.23\n')
 tasks={};scopes={};goals={}
 for name,want in [('alpha',42),('beta',7)]:
  task=task_id(name);test='Test'+name.title();failure='got 0 want '+str(want)
  (repo/(name+'.go')).write_text('package sample\n\nfunc '+name+'() int { return 0 }\n')
  scope={'files':[name+'.go',name+'_test.go'],'test_file':name+'_test.go'}
  scopes[task]=scope;tasks[task]=dict(scope,test_id=test,expected_failure=failure)
  goals[task]=(name,want,test,failure)
 git('add','.');git('commit','-m','isolated two-task dogfood base');base=git('rev-parse','HEAD');git('branch','fae/integration',base)
 cfg={'adapter':'multiplexer','herdr_binary':a.herdr,'tasks':tasks,'command':[str(bridge),'--'+a.provider,cli,'--config','{config}','--files','alpha.go,alpha_test.go','--test-file','alpha_test.go','--task-scopes',json.dumps(scopes,separators=(',',':'))],'environment':{'CLAUDE_CONFIG_DIR':a.claude_config_dir} if a.provider=='claude' else {'CODEX_HOME':os.environ.get('CODEX_HOME',str(pathlib.Path.home()/'.codex'))},'allowed_commands':[['go','test','-json','-count=1','-a','-x','./...'],['gofmt','-l','.']],'files':['alpha.go','alpha_test.go'],'base':base,'integration_ref':'refs/heads/fae/integration','commit_message':'Real-provider dogfood verified change','test_file':'alpha_test.go','test_id':'TestAlpha','expected_failure':'got 0 want 42','host_ledger':str(scratch/'host-leases'),'heavy_capacity':2,'timeout_seconds':900,'stall_seconds':300,'stall_safe_replay':True}
 cfg['resident_sessions']=a.resident_sessions;cfg['persistent_loops']=a.provider_loop
 if a.persistent_sessions:cfg['command'] += ['--session-root',str(scratch/'i'/'dogfood'/'provider-sessions')]
 if a.normal_targets:
  normal_tool=start_effect_fixture()
  if not a.dynamic_provisioning:cfg['external_tools']={'dogfood-effect':normal_tool}
 config=scratch/'live.json';config.touch(mode=0o600);config.chmod(0o600);config.write_text(json.dumps(cfg));start()
 call('message','--id','authorization','--text',json.dumps({'text':('I approve live external effects only for the disposable localhost fixture. ' if a.normal_targets else '')+'Execute both isolated real-provider tasks including cron admission, workflow installation, dynamic runtime limits, explicit crash recovery, bounded provider loops, independent drive rearm, guarded roll, versioned target configuration, synthetic retirement/dependency checks and shutdown.','authorize':{'classes':['governance','task','runtime','controller','schedule']}}))
 wf=json.loads(subprocess.check_output([str(binary),'workflow','default','--local','--json'],text=True))
 # The shipped default's Local projection (what Local tasks pin; the Normal
 # code-review sub-workflow needs review/ticket adapters this fixture lacks)
 # runs live unmodified except for its per-instance route
 # chain (FAE-WF-082): this instance's bridge serves one provider, so keep only
 # that provider's bindings, or the requested binding when it is not the default.
 for role in wf['roles'].values():
  if (role['routes'][0]['provider'],role['routes'][0]['model'])==(a.provider,a.model):role['routes']=[r for r in role['routes'] if r['provider']==a.provider]
  else:role['routes']=[{'provider':a.provider,'model':a.model,'effort':'medium'}]
 assert all(stage['command'] in cfg['allowed_commands'] for stage in wf['stages'].values() if stage['primitive']=='HostCommand'), 'default HostCommand outside the dogfood allowlist'
 workflow=scratch/'workflow.json';workflow.write_text(json.dumps(wf));base_workflow=call('template','workflow-put','--content',str(workflow),'--authorizing-message','authorization')
 if a.target_configs:
  target={'schema_version':1,'workspace':str(repo),'integration_ref':cfg['integration_ref'],'protected_branches':[cfg['integration_ref']],'allowed_commands':cfg['allowed_commands']}
  target_file=scratch/'target.json';target_file.write_text(json.dumps(target))
  target_v1=call('template','target-config-put','--id','dogfood-target','--content',str(target_file),'--authorizing-message','authorization')
  assert target_v1['revision']==1
  # New latest policy deliberately cannot run this workflow; explicit revision 1 must remain usable.
  target['allowed_commands']=[cfg['allowed_commands'][1]];target_file.write_text(json.dumps(target))
  target_v2=call('template','target-config-put','--id','dogfood-target','--content',str(target_file),'--authorizing-message','authorization')
  assert target_v2['revision']==2 and target_v2['hash']!=target_v1['hash']
  result['target_configs']={'revision_1':target_v1,'revision_2':target_v2,'pinned_tasks':[]}
  for task in tasks:
   task_admissions[task]={'target_config':'dogfood-target','target_revision':1}
   task_targets[task]=target_v1
 if a.normal_targets:
  normal_tool=fixture_tools['dogfood-effect']
  normal_target={'schema_version':2,'workspace':str(repo),'integration_ref':cfg['integration_ref'],'protected_branches':[cfg['integration_ref']],'allowed_commands':cfg['allowed_commands'],'normal':{'destination':'main','push_namespace':'fae/dogfood','ticket_room':'local-fixture','tools':{'dogfood-effect':normal_tool}}}
  if a.dynamic_provisioning:
   workspace_plan=call('workspace','review-plan','--task',beta_id,'--run-id','dogfood')
   review_policy={'package':'sample','workspace':workspace_plan['workspace'],'branch':workspace_plan['branch'],'integration_tip':workspace_plan['integration_tip'],'baseline_tool':'baseline','status_tool':'read','baseline_approved':True}
   if a.fleet_provisioning:
    fleet_pins={key:review_policy[key] for key in ('package','workspace','branch','integration_tip')}
    fixture_tools['fleet']=dict(normal_tool,name='fleet_prepare',arguments=fleet_pins)
    review_policy['provision_tool']='fleet'
   adapter_document={'schema_version':1,'tools':fixture_tools,'review_policies':{beta_id:review_policy}}
   adapter_file=scratch/'adapter.json';adapter_file.write_text(json.dumps(adapter_document))
   adapter_v1=call('template','adapter-put','--id','dogfood-adapter','--content',str(adapter_file),'--authorizing-message','authorization')
   if a.fleet_provisioning:
    try:call('workspace','review-prepare','--task',beta_id,'--run-id','dogfood','--adapter','dogfood-adapter','--adapter-revision','1','--adapter-hash',adapter_v1['hash'],'--authorizing-message','authorization',timeout=120)
    except RuntimeError as err:
     assert provision_calls and len(provision_calls)==1, str(err)
    else:raise AssertionError('lost provisioning acknowledgment accepted')
    assert not effect_reads, 'baseline ran before verified provisioning receipt'
    shutdown();start()
   prepared=call('workspace','review-prepare','--task',beta_id,'--run-id','dogfood','--adapter','dogfood-adapter','--adapter-revision','1','--adapter-hash',adapter_v1['hash'],'--authorizing-message','authorization',timeout=120)
   assert prepared['state']=='ready' and prepared['health']['green'] and prepared['workspace']==workspace_plan['workspace']
   repeated=call('workspace','review-prepare','--task',beta_id,'--run-id','dogfood','--adapter','dogfood-adapter','--adapter-revision','1','--adapter-hash',adapter_v1['hash'],'--authorizing-message','authorization',timeout=120)
   assert repeated==prepared
   if a.fleet_provisioning:
    assert len(provision_calls)==1 and prepared['provision']['receipt']['external_id']=='local-fleet-workspace'
    result['fleet_provisioning']={'performed':1,'lost_ack_recovered_after_restart':True,'duplicate_mutations':0,'baseline_after_receipt':True,'idempotent_prepare':True}
    (scratch/'fleet-provisioning.json').write_text(json.dumps({'result':result['fleet_provisioning'],'calls':provision_calls,'record':prepared},indent=2))
   normal_target['schema_version']=3
   normal_target['normal'].update(adapter=adapter_v1,tools=fixture_tools,review_task=beta_id,review=review_policy)
   task_admissions[beta_id]['run_id']='dogfood'
  target_file=scratch/'normal-target.json';target_file.write_text(json.dumps(normal_target))
  normal_v1=call('template','target-config-put','--id','dogfood-normal','--content',str(target_file),'--authorizing-message','authorization')
  task_targets[beta_id]=normal_v1
  if a.dynamic_provisioning:
   newer=json.loads(json.dumps(adapter_document));newer['tools']['dogfood-effect']['arguments']['destination']='other'
   adapter_file.write_text(json.dumps(newer));adapter_v2=call('template','adapter-put','--id','dogfood-adapter','--content',str(adapter_file),'--authorizing-message','authorization')
   assert adapter_v2['revision']==2 and adapter_v2['hash']!=adapter_v1['hash']
   fetched=call('template','adapter-get','--id','dogfood-adapter','--revision','1')
   assert fetched['binding']==adapter_v1 and fetched['document']==adapter_document
   result['dynamic_provisioning']={'adapter_revision':adapter_v1,'later_revision':adapter_v2,'workspace':prepared['workspace'],'ready':True,'idempotent_prepare':True}
   (scratch/'dynamic-provisioning.json').write_text(json.dumps({'result':result['dynamic_provisioning'],'plan':workspace_plan,'prepared':prepared,'installed':fetched},indent=2))

  # Distinct workflow revisions and modes deliberately isolate provider contexts.
  receipt_fields=['tool','argument_digest','external_id','reconciliation_state','idempotency_key']
  wf['artifacts']['ExternalReceipt']={'version':1,'schema':{'type':'object','additionalProperties':False,'required':receipt_fields,'properties':{key:{'type':'string'} for key in receipt_fields}}}
  wf['roles']['planner']['outputs'].append('ExternalReceipt')
  # Schema 2: an ExternalEffect stage acts only under its role's effect grant (FAE-WF-042).
  wf['roles']['planner']['profile'].setdefault('effects',[]).append('dogfood-effect')
  wf['stages']['external']={'primitive':'ExternalEffect','role':'planner','normal_only':True,'subject':'dogfood-effect','pins':normal_tool['arguments'],'idempotence_key':'dogfood-create','output':'ExternalReceipt'}
  for edge in wf['edges']:
   if edge['to']=='Done':edge['to']='external'
  wf['edges'].append({'from':'external','to':'Done'})
  workflow.write_text(json.dumps(wf));normal_workflow=call('template','workflow-put','--content',str(workflow),'--authorizing-message','authorization')
  task_admissions[alpha_id]['revision']=base_workflow['Number']
  task_admissions[beta_id].update(target_config='dogfood-normal',target_revision=1,revision=normal_workflow['Number'],mode='Normal',commit_external=True,ticket_room='local-fixture',destination='main',push_namespace='fae/dogfood')
 prompts={}
 for task,(name,want,test,failure) in goals.items():
  prompt=f'Implement {name}() int returning {want} instead of 0. Only {name}.go and {name}_test.go may change. RED: add exactly one ordinary Go unit test named {test} in {name}_test.go; if {name}()!={want} call t.Fatal("{failure}"). Do not change production code until implement stage. Preserve all test bytes in implementation. Use package sample, standard gofmt tabs and blank lines. Report actual complete test source in source fields. No other functions, dependencies, commands, external effects or files are needed. Each reviewer must report its assigned dimension independently and assess actual evidence. Engine performs builds/tests/format checks/commits. Other concurrent tasks have separate file scopes; do not touch them.'
  prompts[task]=prompt
  if not a.crash_during_turn or task==alpha_id:admit(task,prompt)
 deadline=time.monotonic()+a.timeout;last={};checkpoint=None;dynamic_changed=False
 if a.crash_during_turn:
  while time.monotonic()<deadline:
   before={task:history(task) for task in (alpha_id,)}
   inflight=[snap['payload']['job']['in_flight'] for snap in before.values() if snap['payload']['job'].get('in_flight')]
   assert all(d['config']['primitive']=='AgentTurn' and not d['config'].get('change_producer') for d in inflight), 'crash window passed read-only initial turns'
   active={session['id'] for d in inflight for session in d['sessions']}
   started=[];start_events=[]
   for path in (scratch/'i'/'dogfood'/'sessions').glob('session-*/provider-*/events.jsonl'):
    launch=json.loads((path.parent.parent/'config.json').read_text())
    ev=entries(path)
    if launch['session_id'] in active and any(is_start(e) for e in ev) and not any(is_done(e) for e in ev):
     started.append(launch['session_id']);start_events.append({'session_id':launch['session_id'],'dispatch_id':launch['dispatch_id'],'events':[start_summary(e) for e in ev if is_start(e)]})
   if started:
    (scratch/'crash-before.json').write_text(json.dumps(before,indent=2))
    (scratch/'crash-provider-starts.json').write_text(json.dumps(start_events,indent=2))
    process.kill();process.wait(timeout=30);log.close();start(recover=True)
    recovered={task:history(task) for task in before}
    (scratch/'crash-recovered.json').write_text(json.dumps(recovered,indent=2))
    resolutions=[]
    for task,snap in recovered.items():
     job=snap['payload']['job'];prior=before[task]['payload']['job'];dispatch=job.get('in_flight')
     assert (job['provenance'] or [])[:len(prior['provenance'] or [])]==(prior['provenance'] or [])
     if dispatch:
      assert job.get('parked') and job.get('reason')=='UncertainDispatch' and job['failures']['crash-recovery']==1
      assert dispatch['config']['primitive']=='AgentTurn' and not dispatch['config'].get('change_producer')
      assert prior.get('in_flight') and dispatch['id']==prior['in_flight']['id'] and job['sequence']==prior['sequence']
      crash_sessions.update(session['id'] for session in dispatch['sessions'])
    assert crash_sessions and set(started)&crash_sessions, 'provider turn was not durably uncertain after SIGKILL'
    time.sleep(.5)
    for task,snap in recovered.items():
     job=snap['payload']['job'];dispatch=job.get('in_flight')
     if dispatch:
      still=history(task)['payload']['job'];assert still['in_flight']['id']==dispatch['id'] and still['sequence']==job['sequence'] and still.get('parked'), 'uncertain turn replayed automatically'
      resolutions.append(call('resolve-uncertain','--id',task,'--dispatch',dispatch['id'],'--action','redispatch','--authorizing-message','authorization'))
    result['crash_recovery']={'signal':'SIGKILL','started_provider_sessions':started,'uncertain_sessions':sorted(crash_sessions),'resolutions':resolutions,'automatic_replay':False}
    print('SIGKILL during actual provider turn; retired uncertain dispatches explicitly resolved',flush=True)
    break
   time.sleep(.1)
  else:raise RuntimeError('no live read-only provider turn found for crash')
  admit(beta_id,prompts[beta_id])
 if a.adapter_retirement:
  retired=call('template','adapter-retire','--id','dogfood-adapter','--reason','superseded by next operator configuration','--authorizing-message','authorization')
  repeated=call('template','adapter-retire','--id','dogfood-adapter','--reason','later request must preserve first retirement','--authorizing-message','authorization')
  assert repeated==retired and retired['reason']=='superseded by next operator configuration' and retired['retired_at'] and retired['actor']=='operator', 'retirement changed first tombstone'
  fetched=call('template','adapter-get','--id','dogfood-adapter','--revision','1')
  assert fetched['binding']==adapter_v1, 'retirement lost pinned revision'
  refused=[]
  for label,args in [('write',('template','adapter-put','--id','dogfood-adapter','--content',str(adapter_file),'--authorizing-message','authorization')),('target',('template','target-config-put','--id','retired-adapter-target','--content',str(target_file),'--authorizing-message','authorization'))]:
   try:call(*args)
   except RuntimeError as err:
    assert 'retired' in str(err).lower(), str(err)
    refused.append(label)
   else:raise AssertionError('retired adapter accepted '+label)
  result['adapter_retirement']={'first_tombstone_preserved':True,'new_bindings_refused':refused,'bound_task':beta_id}
  (scratch/'adapter-retirement.json').write_text(json.dumps({'result':result['adapter_retirement'],'retirement':retired},indent=2))
 if a.drive_restart:exercise_drive_restart()
 while time.monotonic()<deadline:
  snapshots={task:history(task) for task in tasks}
  for task,s in snapshots.items():
   job=s['payload']['job'];state=job['state']
   if state!=last.get(task):print(task,'stage:',state,flush=True);last[task]=state
   if state=='Escalated' or (job.get('parked') and job.get('reason') not in ('infrastructure-deferred','all-routes-unavailable')):raise RuntimeError(task+' stopped: '+state+' '+job.get('reason',''))
  if not dynamic_changed and any(s['payload']['job'].get('in_flight') for s in snapshots.values()):
   thresholds=call('status')['snapshot']['configuration']['dynamic']['host_thresholds']
   thresholds['Load']=3.5
   changed=call('runtime-config','--pane-limit','16','--heavy-capacity','1','--tasks-in-flight','1','--host-thresholds',json.dumps(thresholds),'--authorizing-message','authorization')
   assert changed['effective_pane_limit']==16 and changed['heavy_capacity']==1 and changed['tasks_in_flight']==1 and changed['host_thresholds']['Load']==3.5
   result['dynamic_limits']={'during_active_turns':changed}
   dynamic_changed=True
  if checkpoint is None and any(len(s['payload']['job']['provenance'] or [])>=1 for s in snapshots.values()):
   call('drain','--authorizing-message','authorization')
   while not call('status')['snapshot'].get('drained'):
    if time.monotonic()>deadline:raise RuntimeError('drain timeout')
    time.sleep(.2)
   checkpoint={task:history(task) for task in tasks}
   for s in checkpoint.values():assert s['payload']['job']['state']!='Done' and not s['payload']['job'].get('in_flight')
   (scratch/'restart-checkpoint.json').write_text(json.dumps(checkpoint,indent=2))
   result['restart_checkpoint']={task:{'state':s['payload']['job']['state'],'version':s['version'],'artifacts':len(s['payload']['job']['provenance'] or [])} for task,s in checkpoint.items()}
   controller_restart() if a.controller_lifecycle else (shutdown(),start())
   resumed=call('status')['snapshot']['configuration']
   assert resumed['dynamic']['effective_pane_limit']==16
   assert resumed['dynamic']['heavy_capacity']==1 and resumed['dynamic']['tasks_in_flight']==1 and resumed['dynamic']['host_thresholds']['Load']==3.5
   result['dynamic_limits']['after_restart']=resumed
   raised=call('runtime-config','--pane-limit','24','--heavy-capacity','2','--tasks-in-flight','2','--authorizing-message','authorization')
   assert raised['effective_pane_limit']==24 and raised['heavy_capacity']==2 and raised['tasks_in_flight']==2
   result['dynamic_limits']['raised']=raised
   print('drained mid-run restart completed; dynamic limit preserved and raised',flush=True)
  if all(s['payload']['job']['state']=='Done' for s in snapshots.values()):
   assert checkpoint is not None and dynamic_changed
   for task,s in snapshots.items():
    job=s['payload']['job'];assert all(x['execution_kind']=='live' for x in job['provenance'])
    prior=checkpoint[task]['payload']['job']['provenance'] or [];assert job['provenance'][:len(prior)]==prior
    if a.crash_during_turn and task==alpha_id:
     assert job['failures']['crash-recovery']==1 and {session['id'] for dispatch in job['resolved_dispatches'] for session in dispatch['sessions']}==crash_sessions
     assert not {p['session'] for p in job['provenance']}&crash_sessions, 'crashed session identity reused'
    name,want,_,_=goals[task];assert f'return {want}' in git('show','fae/integration:'+name+'.go')
   for task,s in snapshots.items():
    evidence=s['payload']['job']['evidence']
    reds=[e['fields'] for e in evidence if e['stage']=='run-red']
    greens=[e['fields'] for e in evidence if e['stage']=='run-tests']
    assert reds and all(r['fresh_build'] and r['expected_failure'] and r['matched_count']==1 and r['executed_count']==1 and r['exit_code'] not in (0,127) for r in reds)
    assert greens and greens[-1]['fresh_build'] and greens[-1]['red_passed'] and greens[-1]['exit_code']==0
   if a.drive_restart:
    frozen=json.loads((scratch/'drive-restart-checkpoint.json').read_text())
    result['independent_drive']['deferred_before_execution']=[]
    for task,prior in frozen.items():
     dispatch=prior['payload']['job'].get('in_flight')
     if dispatch:
      for session in dispatch['sessions']:
       job=snapshots[task]['payload']['job'];count=sum(p['session']==session['id'] for p in job['provenance'])
       if session['id'] in result['independent_drive']['retained_provider_sessions']:
        assert count==1, 'retained provider turn lost or duplicated'
       elif count==0:
        events=[e for e in job['history'] if e.get('dispatch_id')==dispatch['id']]
        assert any(e['kind']=='execution-diagnostic' and e.get('cause','').startswith('startup-not-executed:') for e in events), 'unstarted dispatch missing nonexecution proof'
        assert any(e['kind']=='park' and e.get('cause')=='infrastructure-deferred' for e in events), 'unstarted dispatch missing deferral'
        result['independent_drive']['deferred_before_execution'].append({'session_id':session['id'],'events':events})
       else:assert count==1, 'original dispatch collected more than once'
     assert snapshots[task]['payload']['job']['failures'].get('crash-recovery',0)==prior['payload']['job']['failures'].get('crash-recovery',0), 'drive restart charged crash budget'
   if a.provider_loop:
    call('schedule','create','--id','dogfood-loop','--role','oracle','--provider',a.provider,'--model',a.model,'--effort','medium','--max-iterations','5','--expires-in','15m','--prompt','Return the required message JSON. Remember the marker fae-loop-dogfood in this conversation, and mention it on every iteration. Do not edit files or invoke tools.','--authorizing-message','authorization')
    while time.monotonic()<deadline:
     loop_snapshot=call('schedule','inspect','--id','dogfood-loop')
     if loop_snapshot['state']=='completed':break
     if loop_snapshot['state']!='active':raise RuntimeError('provider loop stopped: '+json.dumps(loop_snapshot))
     time.sleep(.5)
    assert loop_snapshot['state']=='completed' and loop_snapshot['iterations']==5 and not loop_snapshot['in_flight']
    (scratch/'provider-loop.json').write_text(json.dumps(loop_snapshot,indent=2))
    result['provider_loop']={'iterations':5,'state':'completed','session_id':loop_snapshot['session_id']}
   if a.cron_schedules:
    scheduled_names=['alpha'] if a.normal_targets else ['alpha','beta']
    schedules=[call('schedule','inspect','--id','dogfood-'+name) for name in scheduled_names]
    assert all(row['state']=='completed' and row['admitted']==1 and row['next_occurrence']==1 and row['skipped']==0 for row in schedules)
    assert {row['last_task'] for row in schedules}=={task_id(name) for name in scheduled_names}
    result['cron_schedules']={'completed':len(schedules),'admitted':len(schedules),'duplicate_occurrences':0}
    (scratch/'cron-schedules.json').write_text(json.dumps(schedules,indent=2))
   if a.target_configs:
    for task,snapshot in snapshots.items():
     admission=snapshot['payload']['admission']
     assert snapshot['target_id']==task_targets[task]['id'] and snapshot['target_revision']==1
     assert admission['target_config']==task_targets[task]['id'] and admission['target_revision']==1 and admission['target_hash']==task_targets[task]['hash']
     result['target_configs']['pinned_tasks'].append(task)
    (scratch/'target-configs.json').write_text(json.dumps(result['target_configs'],indent=2))
   if a.normal_targets:
    assert len(effect_calls)==1 and effect_calls[0]['task']==beta_id
    assert snapshots[alpha_id]['payload']['job']['disposition']['mode']=='Local'
    assert snapshots[beta_id]['payload']['job']['disposition']['mode']=='Normal'
    result['normal_targets']={'local_task':alpha_id,'live_task':beta_id,'live_effects':len(effect_calls),'normal_revision':normal_v1,'adapter':'disposable localhost fixture'}
    (scratch/'normal-targets.json').write_text(json.dumps({'result':result['normal_targets'],'calls':effect_calls},indent=2))
   if a.prophet_sessions:
    prophet_evidence=[]
    for scope in (alpha_id, ''):
     prompt=('Summarize the task IDs and current states visible in your supplied context. '
             'Include each visible task ID verbatim in your reply. Propose no actions; set action, task, information and context to empty strings and proposals to an empty array. Do not use tools.')
     args=['prophet','chat','--task',scope] if scope else ['prophet','global']
     submitted=call(*args,'--text',prompt)
     assert submitted['queued']
     readargs=['prophet','session','--task',scope] if scope else ['prophet','global']
     reply=None
     while time.monotonic()<deadline:
      page=call(*readargs,'--after',str(submitted['entry']['sequence']),'--limit','100')
      reply=next((m for m in page.get('messages') or [] if m.get('reply_to')==submitted['id']),None)
      if reply:break
      time.sleep(.5)
     assert reply and reply['detail']['outcome']=='answered', ('Prophet turn failed',reply)
     answer=reply['detail']['answer'];assert not answer['action']
     assert alpha_id in answer['reply']
     if not scope:assert beta_id in answer['reply']
     prophet_evidence.append({'scope':scope,'submission':submitted,'reply':reply})
    (scratch/'prophet-sessions.json').write_text(json.dumps(prophet_evidence,indent=2))
    result['prophet_sessions']={'answered':len(prophet_evidence),'scopes':['task','global'],'actions_enacted':0}
   if a.retirement:
    call('drive','stop','--authorizing-message','authorization')
    until=time.monotonic()+15
    while call('drive','status','--authorizing-message','authorization')['DriveAlive']:
     assert time.monotonic()<until, 'drive stop timeout before retirement fixtures'
     time.sleep(.1)
    old,successor,dependent='retire-old','retire-successor','retire-dependent'
    graph={'items':[{'id':item,'content':'Retirement fixture; must never execute','priority':10,'workflow_id':'default','revision':base_workflow['Number']} for item in (old,successor,dependent)],'edges':[{'from':old,'to':dependent,'kind':'blocking'}]}
    graph_file=scratch/'retirement-input.json';graph_file.write_text(json.dumps(graph))
    call('backlog','admit','--file',str(graph_file),'--authorizing-message','authorization')
    call('escalate','--task',old,'--reason','dogfood retirement','--authorizing-message','authorization')
    transferred=call('retire','--task',old,'--reason','superseded-by '+successor,'--authorizing-message','authorization')
    assert transferred['state']=='Retired' and transferred['dependent_effect']=='transferred' and dependent in transferred['dependents']
    after_transfer=call('backlog','show','--include-retired')
    assert {'from':successor,'to':dependent,'kind':'blocking'} in after_transfer['edges']
    assert {'from':old,'to':dependent,'kind':'blocking'} not in after_transfer['edges']
    call('escalate','--task',successor,'--reason','dogfood obsolete ruling','--authorizing-message','authorization')
    if a.census_cleanup:
     proposals=scratch/'census-proposals.json';proposals.write_text(json.dumps([{'id':successor,'reason':'obsolete dogfood-ruling-1','evidence':'Operator-authored disposable fixture ruling; retain dependent hold.'}]))
     census=call('census','--file',str(proposals));artifact=scratch/'census-artifact.json';artifact.write_text(json.dumps(census))
     applied=call('census','apply','--file',str(artifact),'--gap','100ms','--authorizing-message','authorization')
     assert applied['completed']==[successor]
     held={'dependent_effect':'held','held_dependents':[dependent]}
    else:held=call('retire','--task',successor,'--reason','obsolete dogfood-ruling-1','--authorizing-message','authorization')
    assert held['dependent_effect']=='held' and dependent in held['held_dependents']
    visible=call('backlog','show');all_rows=call('backlog','show','--include-retired')
    assert not {old,successor} & {row['id'] for row in visible['items']}
    assert {old,successor} <= {row['id'] for row in all_rows['items']}
    assert dependent not in all_rows['ready'] and all_rows['withheld'].get(dependent)
    assert call('history','--task',old,'--include-retired')['state']=='Retired'
    try:history(old)
    except RuntimeError:pass
    else:raise AssertionError('retired history visible by default')
    result['retirement']={'transferred':transferred,'held':held,'hidden_by_default':True,'dependent_withheld':True}
    (scratch/'retirement.json').write_text(json.dumps({'result':result['retirement'],'after_transfer':after_transfer,'final':all_rows},indent=2))
    if a.census_cleanup:
     backup=call('backup','plane','--authorizing-message','authorization',timeout=120)
     plan=call('cleanup','retired','--backup',backup['path'],'--dry-run',timeout=120)
     assert {row['task'] for row in plan}=={old,successor}
     removed=call('cleanup','retired','--backup',backup['path'],'--apply','--pace','100ms','--authorizing-message','authorization',timeout=120)
     assert all(row['removed'] for row in removed) and {row['task'] for row in removed}=={old,successor}
     for row in removed:
      assert git('rev-parse',row['plan']['archive'])==row['plan']['head']
     result['census_cleanup']={'applied':applied['completed'],'removed':[row['task'] for row in removed],'verified_backup':True,'archives_verified':len(removed)}
     (scratch/'census-cleanup.json').write_text(json.dumps({'census':census,'apply':applied,'backup':backup,'plan':plan,'removed':removed},indent=2))
   ledger=json.loads((scratch/'host-leases'/'state.json').read_text())
   assert ledger['Pool']['Capacity']==2 and ledger['Pool']['AdmissionLimit']==2 and not ledger['Pool']['Holders'] and not ledger['Pool']['Waiting']
   grants=[e for e in ledger['Audit'] if e['Action']=='granted']
   releases=[e for e in ledger['Audit'] if e['Action']=='released']
   recoveredleases=[e for e in ledger['Audit'] if e['Action']=='recovered']
   assert len(grants)>0 and {e['ID'] for e in grants}<={e['ID'] for e in releases+recoveredleases}
   (scratch/'lease-audit.json').write_text(json.dumps(ledger,indent=2))
   result['lease_audit']={'capacity':2,'granted':len(grants),'released':len(releases),'recovered':len(recoveredleases),'final_holders':0}
   panes=json.loads((scratch/'host-leases'/'panes'/'state.json').read_text())
   if not a.resident_sessions:assert not panes.get('Panes')
   result['pane_audit']={'before_shutdown_reservations':len(panes.get('Panes') or [])}
   (scratch/'pane-audit.json').write_text(json.dumps(panes,indent=2))
   health=call('provider-health');providerlog=call('provider-log','--limit','500')
   if a.prophet_sessions:
    for entry in prophet_evidence:
     assert any(row['session']==entry['submission']['id'] and 'outcome ok' in row['line'] for row in providerlog), 'Prophet health observation missing'
    result['prophet_sessions']['durable_health_observations']=2
   (scratch/'provider-health.json').write_text(json.dumps(health,indent=2))
   (scratch/'provider-log.json').write_text(json.dumps(providerlog,indent=2))
   result['stall_recoveries']={task:s['payload']['job'].get('failures',{}).get('stall-recovery',0) for task,s in snapshots.items()}
   (scratch/'final-evidence.json').write_text(json.dumps(snapshots,indent=2))
   result.update(status='passed',tasks={task:{'artifact_records':len(s['payload']['job']['provenance'] or []),'version':s['version']} for task,s in snapshots.items()},integration_sha=git('rev-parse','fae/integration'),base_sha=base)
   assert git('rev-parse','HEAD')==base and git('status','--porcelain')==''
   break
  time.sleep(1)
 else:raise RuntimeError('dogfood timeout')
 if a.backup_policy:
  endpoint=start_replica_fixture()
  first_backup=call('backup','plane','--authorizing-message','authorization',timeout=150)
  newest_backup=call('backup','plane','--authorizing-message','authorization',timeout=150)
  preview=call('backup','retention','--keep','1',timeout=150)
  assert first_backup['path'] in preview['remove'] and newest_backup['path'] in [x['path'] for x in preview['retained']]
  try:
   call('backup','replicate','--backup',newest_backup['path'],'--endpoint',endpoint,'--authorizing-message','authorization',timeout=150)
   raise AssertionError('lost replica acknowledgment unexpectedly succeeded')
  except RuntimeError:pass
  assert len(replica_uploads)==1
  shutdown();start()
  replicated=call('backup','replicate','--backup',newest_backup['path'],'--endpoint',endpoint,'--authorizing-message','authorization',timeout=150)
  assert replicated['state']=='replicated' and len(replica_uploads)==1 and any(x.endswith('/content') for x in replica_reads)
  restored=scratch/'replica-restore';restored.mkdir(mode=0o700)
  with tarfile.open(scratch/'replica'/replicated['sha256']) as archive:
   for member in archive.getmembers():
    assert member.isfile() and not pathlib.PurePosixPath(member.name).is_absolute() and '..' not in pathlib.PurePosixPath(member.name).parts
   archive.extractall(restored)
  remote_manifest=json.loads((restored/'verified.json').read_text())
  for name,digest in remote_manifest['files'].items():assert hashlib.sha256((restored/name).read_bytes()).hexdigest()==digest
  restore_env={k:v for k,v in os.environ.items() if not k.startswith(('DOLT_','FAE_'))}
  restore_env.update(DOLT_ROOT_PATH=str(restored/'isolated-config'),DOLT_DISABLE_EVENT_FLUSH='1')
  for key,value in [('user.name','Fae dogfood'),('user.email','fae@localhost')]:
   subprocess.run([a.dolt,'config','--global','--add',key,value],cwd=restored,env=restore_env,check=True,capture_output=True)
  subprocess.run([a.dolt,'backup','restore',(restored/'remote').as_uri(),'roundtrip'],cwd=restored,env=restore_env,check=True,capture_output=True,timeout=120)
  fsck=subprocess.run([a.dolt,'fsck'],cwd=restored/'roundtrip',env=restore_env,check=True,capture_output=True,text=True,timeout=120)
  (scratch/'replica-fsck.log').write_text(fsck.stdout+fsck.stderr)
  repeated=call('backup','replicate','--backup',newest_backup['path'],'--endpoint',endpoint,'--authorizing-message','authorization',timeout=150)
  assert repeated['state']=='replicated' and len(replica_uploads)==1
  try:
   call('backup','retention','--keep','1','--apply','--plan-digest',preview['plan_digest'],'--authorizing-message','authorization',timeout=150)
   raise AssertionError('replication pin did not invalidate old retention plan')
  except RuntimeError:pass
  preview=call('backup','retention','--keep','1',timeout=150)
  kept=call('backup','retention','--keep','1','--apply','--plan-digest',preview['plan_digest'],'--authorizing-message','authorization',timeout=150)
  assert first_backup['path'] in kept['removed'] and not pathlib.Path(first_backup['path']).exists() and pathlib.Path(newest_backup['path']).is_dir()
  assert call('backup','retention','--keep','1',timeout=150)['remove']==[]
  result['backup_policy']={'lost_ack_recovered_after_restart':True,'uploads':len(replica_uploads),'remote_content_verified':True,'remote_restore_fsck':True,'idempotent_repeat':True,'replication_pin_invalidated_preview':True,'removed':len(kept['removed']),'retained':len(kept['retained'])}
  (scratch/'backup-policy.json').write_text(json.dumps({'result':result['backup_policy'],'replication':replicated,'retention':kept,'reads':replica_reads},indent=2))
 if a.retention_automation:
  configured=call('backup','retention-schedule','configure','--interval','1s','--keep','1','--authorizing-message','authorization')
  paused=call('backup','retention-schedule','pause','--authorizing-message','authorization')
  assert paused['state']=='paused'
  extra_first=call('backup','plane','--authorizing-message','authorization',timeout=150)
  extra_last=call('backup','plane','--authorizing-message','authorization',timeout=150)
  shutdown();start()
  persisted=call('backup','retention-schedule','status')
  assert persisted['state']=='paused' and persisted['keep']==1 and persisted['interval_seconds']==1
  assert pathlib.Path(extra_first['path']).is_dir() and pathlib.Path(extra_last['path']).is_dir()
  resumed=call('backup','retention-schedule','resume','--authorizing-message','authorization')
  until=time.monotonic()+io_gate_wait
  while time.monotonic()<until:
   schedule=call('backup','retention-schedule','status')
   if schedule['state']=='blocked':raise RuntimeError('retention schedule blocked: '+schedule.get('last_error',''))
   if schedule['run_count']>persisted['run_count'] and not schedule['running'] and not pathlib.Path(extra_first['path']).exists():break
   time.sleep(.1)
  else:raise RuntimeError('automatic retention did not prune redundant backup')
  final_schedule=call('backup','retention-schedule','pause','--authorizing-message','authorization')
  assert final_schedule['state']=='paused' and not final_schedule.get('last_error')
  assert pathlib.Path(extra_last['path']).is_dir() and pathlib.Path(newest_backup['path']).is_dir(), 'retention removed newest or replication-pinned source'
  result['retention_automation']={'paused_across_restart':True,'automatic_prune':True,'newest_retained':True,'replication_pin_retained':True,'runs':final_schedule['run_count'],'final_state':final_schedule['state']}
  (scratch/'retention-automation.json').write_text(json.dumps({'result':result['retention_automation'],'configured':configured,'persisted':persisted,'final':final_schedule},indent=2))
 if a.backup_schedules:
  creation=call('backup','creation-schedule','configure','--interval','1s','--authorizing-message','authorization')
  call('backup','creation-schedule','pause','--authorizing-message','authorization')
  call('backup','replication-schedule','configure','--interval','5s','--endpoint',endpoint,'--authorizing-message','authorization')
  call('backup','replication-schedule','pause','--authorizing-message','authorization')
  before_creation=set((scratch/'i'/'dogfood'/'backup').iterdir())
  shutdown();start()
  creation_paused=call('backup','creation-schedule','status')
  replication_paused=call('backup','replication-schedule','status')
  assert creation_paused['state']=='paused' and replication_paused['state']=='paused'
  assert set((scratch/'i'/'dogfood'/'backup').iterdir())==before_creation
  call('backup','creation-schedule','resume','--authorizing-message','authorization')
  until=time.monotonic()+io_gate_wait
  while time.monotonic()<until:
   created=call('backup','creation-schedule','status')
   if created['state']=='blocked':raise RuntimeError('automatic backup blocked: '+created.get('last_error',''))
   if created.get('last_backup') and not created['running']:break
   time.sleep(.1)
  else:raise RuntimeError('automatic backup creation timed out')
  call('backup','creation-schedule','pause','--authorizing-message','authorization')
  created_path=pathlib.Path(created['last_backup']);created_manifest=json.loads((created_path/'verified.json').read_text())
  for name,digest in created_manifest['files'].items():assert hashlib.sha256((created_path/name).read_bytes()).hexdigest()==digest
  before_uploads=len(replica_uploads)
  call('backup','replication-schedule','resume','--authorizing-message','authorization')
  until=time.monotonic()+io_gate_wait
  while time.monotonic()<until:
   uncertain=call('backup','replication-schedule','status')
   if uncertain.get('pending') and not uncertain['running'] and len(replica_uploads)>before_uploads:break
   time.sleep(.1)
  else:raise RuntimeError('scheduled replication did not preserve uncertain upload')
  call('backup','replication-schedule','pause','--authorizing-message','authorization')
  if a.backup_storage:
   try:
    call('backup','unpin','--backup',uncertain['pending']['backup'],'--authorizing-message','authorization',timeout=150)
    raise AssertionError('uncertain scheduled replication was unpinned')
   except RuntimeError:pass
  try:
   call('backup','replication-schedule','configure','--interval','1s','--endpoint',endpoint+'/changed','--authorizing-message','authorization')
   raise AssertionError('pending replication destination changed')
  except RuntimeError:pass
  shutdown();start()
  recovered_schedule=call('backup','replication-schedule','status')
  assert recovered_schedule['state']=='paused' and recovered_schedule['pending']==uncertain['pending']
  call('backup','replication-schedule','resume','--authorizing-message','authorization')
  until=time.monotonic()+io_gate_wait
  while time.monotonic()<until:
   replicated_schedule=call('backup','replication-schedule','status')
   if replicated_schedule['state']=='blocked':raise RuntimeError('automatic replication blocked: '+replicated_schedule.get('last_error',''))
   if replicated_schedule.get('last_backup')==str(created_path) and not replicated_schedule.get('pending') and not replicated_schedule['running']:break
   time.sleep(.1)
  else:raise RuntimeError('automatically created backup was not replicated')
  call('backup','replication-schedule','pause','--authorizing-message','authorization')
  assert len(replica_uploads)==len(set(replica_uploads)), 'automatic replication repeated PUT'
  created_digest=hashlib.sha256((created_path/'verified.json').read_bytes()).hexdigest()
  assert any(receipt['manifest_sha256']==created_digest for receipt in replica_receipts.values())
  result['backup_schedules']={'paused_across_restart':True,'automatic_creation_verified':True,'automatic_replication_verified':True,'pending_destination_change_refused':True,'pending_survived_restart':True,'duplicate_uploads':0,'scheduled_uploads':len(replica_uploads)-before_uploads,'final_creation_state':'paused','final_replication_state':'paused'}
  (scratch/'backup-schedules.json').write_text(json.dumps({'result':result['backup_schedules'],'created':created,'uncertain':uncertain,'recovered':recovered_schedule,'replicated':replicated_schedule},indent=2))
 if a.backup_storage:
  before_puts=len(replica_uploads)
  pinned_preview=call('backup','retention','--keep','1',timeout=150)
  unpin_preview=call('backup','unpin','--backup',newest_backup['path'],'--authorizing-message','authorization',timeout=150)
  unpinned=call('backup','unpin','--backup',newest_backup['path'],'--apply','--plan-digest',unpin_preview['plan_digest'],'--authorizing-message','authorization',timeout=150)
  assert pathlib.Path(newest_backup['path']).is_dir(), 'unpin deleted source directly'
  try:
   call('backup','retention','--keep','1','--apply','--plan-digest',pinned_preview['plan_digest'],'--authorizing-message','authorization',timeout=150)
   raise AssertionError('unpin failed to invalidate prior retention preview')
  except RuntimeError:pass
  fresh_retention=call('backup','retention','--keep','1',timeout=150)
  assert newest_backup['path'] in fresh_retention['remove'], 'redundant confirmed source remained pinned'
  assert len(replica_uploads)==before_puts, 'unpin reuploaded remote data'
  usage=call('backup','quota','status')
  assert usage['backup_count']>0 and usage['usage_bytes']>0
  configured_quota=call('backup','quota','configure','--max-backups',str(usage['backup_count']),'--authorizing-message','authorization')
  backup_paths=set((scratch/'i'/'dogfood'/'backup').iterdir())
  try:
   call('backup','plane','--authorizing-message','authorization',timeout=150)
   raise AssertionError('backup count quota permitted allocation')
  except RuntimeError:pass
  assert set((scratch/'i'/'dogfood'/'backup').iterdir())==backup_paths
  shutdown();start()
  persisted_quota=call('backup','quota','status')
  assert persisted_quota['max_backups']==usage['backup_count']
  call('backup','quota','configure','--max-bytes',str(persisted_quota['usage_bytes']),'--authorizing-message','authorization')
  try:
   call('backup','plane','--authorizing-message','authorization',timeout=150)
   raise AssertionError('backup byte quota permitted allocation')
  except RuntimeError:pass
  assert set((scratch/'i'/'dogfood'/'backup').iterdir())==backup_paths
  call('backup','quota','configure','--max-backups',str(usage['backup_count']+2),'--authorizing-message','authorization')
  quota_backup=call('backup','plane','--authorizing-message','authorization',timeout=150)
  assert pathlib.Path(quota_backup['path'],'verified.json').is_file()
  final_quota=call('backup','quota','status')
  assert final_quota['backup_count']==usage['backup_count']+1 and not final_quota['exceeded']
  result['backup_storage']={'pending_unpin_refused':True,'confirmed_unpin_verified':True,'unpin_preserved_source':True,'unpin_invalidated_retention_preview':True,'unpin_uploads':len(replica_uploads)-before_puts,'count_quota_refused_before_allocation':True,'byte_quota_refused_before_allocation':True,'quota_survived_restart':True,'raised_quota_allowed_verified_backup':True}
  (scratch/'backup-storage.json').write_text(json.dumps({'result':result['backup_storage'],'unpin_preview':unpin_preview,'unpinned':unpinned,'persisted_quota':persisted_quota,'final_quota':final_quota},indent=2))
 shutdown()
 if a.retention_automation:
  final_ledger=json.loads((scratch/'host-leases'/'state.json').read_text())
  assert not final_ledger['Pool']['Holders'] and not final_ledger['Pool']['Waiting'], 'backup or scheduled retention host lease leaked'
  final_grants={entry['ID'] for entry in final_ledger['Audit'] if entry['Action']=='granted'}
  final_releases={entry['ID'] for entry in final_ledger['Audit'] if entry['Action'] in ('released','recovered')}
  assert final_grants<=final_releases, 'backup or retention lease missing release record'
  (scratch/'retention-lease-audit.json').write_text(json.dumps(final_ledger,indent=2))
  result['retention_automation']['final_host_holders']=0
 owned=scratch/'i'/'dogfood'/'sessions'
 assert not list(owned.glob('.processes/*.json*')), 'owned process records or retirement receipts remain'
 assert not list(owned.rglob('mux.sock')), 'owned herdr-core sockets remain'
 panes=json.loads((scratch/'host-leases'/'panes'/'state.json').read_text())
 assert not panes.get('Panes'), 'pane reservations remain after shutdown'
 result['pane_audit']['final_reservations']=0
 (scratch/'pane-audit-final.json').write_text(json.dumps(panes,indent=2))
 turns=[];contexts=[];provider_starts={}
 for path in owned.glob('session-*/provider-*/events.jsonl'):
  entries=[]
  for line in path.read_text().splitlines():
   try:entries.append(json.loads(line))
   except json.JSONDecodeError:continue
  launch=json.loads((path.parent.parent/'config.json').read_text())
  provider_starts[launch['session_id']]=provider_starts.get(launch['session_id'],0)+sum(is_turn_start(e) for e in entries)
  if any(is_done(e) for e in entries):
   assert sum(is_done(e) for e in entries)==1, 'provider turn completed multiple times'
   turns.append({'session_id':launch['session_id'],'task_id':launch['task_id'],'workflow_id':launch.get('workflow_id',''),'provider':launch['binding']['provider'],'model':launch['binding']['model']})
   contextpath=path.parent/'provider-session.json'
   if contextpath.exists():
    context=json.loads(contextpath.read_text());context['session_id']=launch['session_id']
    assert context['task_id']==launch['task_id'] and context['dispatch_id']==launch['dispatch_id']
    assert launch['session_policy'] in ('workflow-global','task-bound') and not launch.get('fresh_context',False)
    context['session_policy']=launch['session_policy']
    contexts.append(context)
 expected={p['session'] for s in snapshots.values() for p in s['payload']['job']['provenance'] if p['primitive'] in ('AgentTurn','Review')}
 loop_sessions={t['session_id'] for t in turns if t['workflow_id']=='fae-loop:dogfood-loop'} if a.provider_loop else set()
 prophet_sessions={t['session_id'] for t in turns if t['workflow_id']=='fae-prophet'}
 if a.prophet_sessions:
  assert len(prophet_sessions)==2, 'expected two real Prophet turns'
  assert not prophet_sessions & {c['session_id'] for c in contexts}, 'Prophet reused opaque provider context'
 else:assert not prophet_sessions
 if a.provider_loop:assert len(loop_sessions)==5
 if a.drive_restart:
  for session in result['independent_drive']['retained_provider_sessions']:
   assert sum(t['session_id']==session for t in turns)==1 and provider_starts.get(session,0)==1, 'retained provider turn executed more than once'
  for deferred in result['independent_drive']['deferred_before_execution']:
   session=deferred['session_id']
   assert provider_starts.get(session,0)==0 and not any(t['session_id']==session for t in turns), 'deferred dispatch actually executed'
  (scratch/'drive-restart.json').write_text(json.dumps(result['independent_drive'],indent=2))
 completed={t['session_id'] for t in turns}
 assert expected|loop_sessions|prophet_sessions <= completed and completed <= expected|loop_sessions|prophet_sessions|crash_sessions, 'provider turn/provenance mismatch'
 result['crashed_completed_uncollected']=sorted((completed&crash_sessions)-expected)
 if a.provider_loop:
  loopcontexts=[c for c in contexts if c['session_id'] in loop_sessions]
  assert len(loopcontexts)==5 and len({c['thread_id'] for c in loopcontexts})==1 and sum(c['resumed'] for c in loopcontexts)==4, 'loop did not reuse the provider context'
  result['provider_loop']['provider_thread_id']=loopcontexts[0]['thread_id']
 if a.resident_sessions:
  residents=[json.loads(path.read_text()) for path in owned.glob('session-*/resident-session.json')]
  groups={}
  for entry in residents:groups.setdefault((entry['resident_id'],entry['server_pid'],entry['pane_pid']),[]).append(entry['session_id'])
  reused=[{'resident_id':key[0],'server_pid':key[1],'pane_pid':key[2],'sessions':sessions} for key,sessions in groups.items() if len(sessions)>1]
  assert reused, 'no actual resident pane reused across turns'
  if a.provider_loop:assert any(loop_sessions<=set(group['sessions']) for group in reused), 'loop did not reuse resident pane'
  result['resident_sessions']={'audited_turns':len(residents),'reused_panes':len(reused)}
  (scratch/'resident-sessions.json').write_text(json.dumps({'turns':residents,'reused':reused},indent=2))
 (scratch/'provider-turns.json').write_text(json.dumps(turns,indent=2))
 result['real_completed_turns']=len(turns)
 if a.persistent_sessions:
  prior={p['session'] for snap in checkpoint.values() for p in snap['payload']['job']['provenance'] or []}
  before={c['thread_id'] for c in contexts if c['session_id'] in prior}
  after=[c for c in contexts if c['session_id'] not in prior and c['resumed'] and c['thread_id'] in before]
  cross=[thread for thread in {c['thread_id'] for c in contexts} if len({c['task_id'] for c in contexts if c['thread_id']==thread})>1]
  assert after, 'provider context did not resume across restart'
  if a.normal_targets:
   assert not cross, 'provider context crossed Local/Normal mode and workflow revision boundaries'
  else:
   assert cross, 'provider context did not resume across compatible tasks'
  bound=[c for c in contexts if c['session_policy']=='task-bound']
  assert any(c['resumed'] for c in bound), 'task-bound context never resumed within its task'
  for thread in {c['thread_id'] for c in bound}:
   assert len({c['task_id'] for c in contexts if c['thread_id']==thread})==1, 'task-bound context crossed tasks'
  result['persistent_sessions']={'audited_turns':len(contexts),'resumed_turns':sum(c['resumed'] for c in contexts),'resumed_after_restart':len(after),'cross_task_threads':len(cross),'mixed_mode_isolation':bool(a.normal_targets and not cross),'task_bound_resumed_turns':sum(c['resumed'] for c in bound)}
  (scratch/'provider-sessions.json').write_text(json.dumps(contexts,indent=2))
 if a.adapter_retirement:
  assert result['normal_targets']['live_effects']==1 and snapshots[beta_id]['state']=='Done'
  result['adapter_retirement']['bound_task_completed_after_restart']=True
  (scratch/'adapter-retirement.json').write_text(json.dumps({'result':result['adapter_retirement'],'retirement':retired},indent=2))
 if a.fleet_provisioning:assert len(provision_calls)==1, 'provisioning repeated on hot path'
 if a.scratch_cleanup:
  scratch_plan=call('cleanup','scratch','--dry-run','--dolt',a.dolt,timeout=120)
  assert not scratch_plan['completed'] and (scratch/'i'/'dogfood').is_dir()
  cleaned=call('cleanup','scratch','--apply','--dolt',a.dolt,'--authorizing-message','authorization',timeout=240)
  assert cleaned['completed'] and cleaned['prepared'] and not (scratch/'i'/'dogfood').exists()
  archive=pathlib.Path(cleaned['archive_path'])
  assert archive.is_dir() and not archive.is_relative_to(scratch/'i'/'dogfood')
  for name,digest in cleaned['files'].items():
   assert hashlib.sha256((archive/name).read_bytes()).hexdigest()==digest, 'scratch backup checksum differs'
  archived_plans=[plan for plan in cleaned['plans'] if not plan['already_removed']]
  for plan in archived_plans:
   assert git('rev-parse',plan['archive'])==plan['head'], 'scratch task archive absent'
  repeated=call('cleanup','scratch','--apply','--dolt',a.dolt,'--authorizing-message','authorization',timeout=120)
  assert repeated['completed'] and repeated['identity']==cleaned['identity'] and repeated['archive_path']==cleaned['archive_path']
  result['scratch_cleanup']={'completed':True,'root_absent':True,'backup_retained':True,'backup_files_verified':len(cleaned['files']),'archives_verified':len(archived_plans),'idempotent_repeat':True}
  (scratch/'scratch-cleanup.json').write_text(json.dumps({'result':result['scratch_cleanup'],'plan':scratch_plan,'cleanup':cleaned},indent=2))
 if a.retention_automation:
  duplicate=archive.parent/(cleaned['identity']['id']+'-redundant')
  shutil.copytree(archive,duplicate)
  (duplicate/'uncovered-file').write_text('not in canonical backup')
  # FAE-CLI-001: archive cleanup of the removed scratch instance runs through a
  # serving instance in the same home, with its own runtime authorization.
  host_ws=scratch/'archive-host';host_ws.mkdir();subprocess.run(['git','init','-q',str(host_ws)],check=True)
  host_log=(scratch/'archive-host.log').open('a')
  host=subprocess.Popen([str(binary),'--home',str(scratch/'i'),'--instance','archive-host','serve','--workspace',str(host_ws),'--adapter','scripted','--dolt',a.dolt,'--foreground'],cwd=host_ws,stdout=host_log,stderr=host_log)
  for _ in range(600):
   if host.poll() is not None:raise RuntimeError('archive host exited; inspect '+str(scratch/'archive-host.log'))
   try:call('status',instance='archive-host');break
   except RuntimeError:time.sleep(.1)
  else:raise RuntimeError('archive host startup timeout')
  call('message','--id','archive-auth','--text',json.dumps({'text':'Remove the verified redundant scratch archive of the removed dogfood instance, then stop this helper.','authorize':{'classes':['runtime']}}),instance='archive-host')
  archive_args=['cleanup','scratch-archive','--scratch-instance','dogfood','--source',str(duplicate),'--survivor',str(archive)]
  try:
   call(*archive_args,timeout=150,instance='archive-host')
   raise AssertionError('uncovered archive data accepted')
  except RuntimeError:pass
  assert duplicate.is_dir() and archive.is_dir()
  (duplicate/'uncovered-file').unlink()
  archive_plan=call(*archive_args,timeout=150,instance='archive-host')
  removed_archive=call(*archive_args,'--apply','--plan-digest',archive_plan['plan_digest'],'--authorizing-message','archive-auth',timeout=150,instance='archive-host')
  assert removed_archive['completed'] and not duplicate.exists() and archive.is_dir()
  repeated_archive=call(*archive_args,'--apply','--plan-digest',archive_plan['plan_digest'],'--authorizing-message','archive-auth',timeout=150,instance='archive-host')
  assert repeated_archive['completed']
  call('shutdown','--authorizing-message','archive-auth',instance='archive-host');assert host.wait(timeout=60)==0;host_log.close()
  final_cleanup=call('cleanup','scratch','--apply','--dolt',a.dolt,'--authorizing-message','authorization',timeout=120)
  assert final_cleanup['completed'] and final_cleanup['archive_path']==str(archive)
  for name,digest in cleaned['files'].items():assert hashlib.sha256((archive/name).read_bytes()).hexdigest()==digest
  result['scratch_archive_cleanup']={'redundant_removed':True,'canonical_retained':True,'uncovered_data_refused':True,'idempotent_retry':True,'scratch_cleanup_still_valid':True}
  (scratch/'scratch-archive-cleanup.json').write_text(json.dumps({'result':result['scratch_archive_cleanup'],'plan':archive_plan,'cleanup':removed_archive},indent=2))
 result['status']='passed'
except Exception as e:
 result.update(status='failed',error=str(e))
 try:
  partial={task:history(task) for task in tasks};(scratch/'partial-evidence.json').write_text(json.dumps(partial,indent=2))
 except Exception:pass
 raise
finally:
 cleanup_errors=[]
 # Acceptance can fail after launch. Recover only this private instance's exact
 # live owner, whose recorded birth matches the current process, before cleanup.
 if launcher_process is not None and (process is None or process.poll() is not None):
  ownerpath=scratch/'i'/'dogfood'/'run'/'lock'
  try:
   raw=ownerpath.read_text().strip()
   if raw:
    owner=json.loads(raw);candidate=LaunchedDaemon(owner['pid'])
    if candidate.birth!=' '.join(owner['start'].split()):raise RuntimeError('instance owner identity changed')
    process=candidate
  except (FileNotFoundError,ProcessLookupError):pass
  except Exception as cleanup_error:cleanup_errors.append(str(cleanup_error))
 for child in (process,launcher_process):
  if child is None:continue
  try:
   if child.poll() is None:
    child.terminate()
    try:child.wait(timeout=30)
    except subprocess.TimeoutExpired:child.kill();child.wait(timeout=10)
  except Exception as cleanup_error:cleanup_errors.append(str(cleanup_error))
 if replica_server is not None:
  replica_server.shutdown();replica_server.server_close();replica_thread.join(timeout=5)
 if effect_server is not None:
  effect_server.shutdown();effect_server.server_close();effect_thread.join(timeout=5)
 if launcher_log is not None:launcher_log.close()
 if cleanup_errors:result.update(status='failed',cleanup_errors=cleanup_errors)
 (scratch/'result.json').write_text(json.dumps(result,indent=2)+'\n');report.write_text(json.dumps(result,indent=2)+'\n')
 if a.evidence_dir:
  for name in ['result.json','restart-checkpoint.json','final-evidence.json','partial-evidence.json','lease-audit.json','pane-audit.json','provider-health.json','provider-log.json','provider-turns.json','provider-sessions.json','crash-before.json','crash-recovered.json','crash-provider-starts.json','provider-loop.json','resident-sessions.json','pane-audit-final.json','cron-schedules.json','controller-lifecycle.json','drive-restart.json','drive-restart-checkpoint.json','prophet-sessions.json','target-configs.json','retirement.json','normal-targets.json','census-cleanup.json','dynamic-provisioning.json','scratch-cleanup.json','fleet-provisioning.json','adapter-retirement.json','backup-policy.json','replica-fsck.log','retention-automation.json','scratch-archive-cleanup.json','retention-lease-audit.json','backup-schedules.json','backup-storage.json']:
   if (scratch/name).exists():shutil.copy2(scratch/name,a.evidence_dir/name)
  (a.evidence_dir/'source-hashes.json').write_text(json.dumps(source_hashes,indent=2)+'\n')
  manifest={f.name:hashlib.sha256(f.read_bytes()).hexdigest() for f in sorted(a.evidence_dir.iterdir()) if f.is_file()}
  (a.evidence_dir/'sha256.json').write_text(json.dumps(manifest,indent=2)+'\n')
 print(json.dumps(result),flush=True)
