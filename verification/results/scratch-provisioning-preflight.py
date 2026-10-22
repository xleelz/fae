import pathlib,sys
source=pathlib.Path('scripts/dogfood.py').resolve()
code=source.read_text().split(' prompts={}')[0]
code+='''
 print(json.dumps({'preflight':'provisioned','result':result.get('dynamic_provisioning')}),flush=True)
 shutdown()
 cleanup=call('cleanup','scratch','--apply','--dolt',a.dolt,'--authorizing-message','authorization',timeout=240)
 assert cleanup['completed'] and not (scratch/'i'/'dogfood').exists()
 assert any(plan['task']==beta_id for plan in cleanup['plans']), 'unadmitted prepared workspace not archived'
 print(json.dumps({'preflight':'passed','scratch_cleanup':True,'prepared_workspace_archived':True}),flush=True)
finally:
 if process is not None and process.poll() is None:
  process.terminate();process.wait(timeout=30)
 if effect_server is not None:
  effect_server.shutdown();effect_server.server_close();effect_thread.join(timeout=5)
'''
sys.argv=[str(source),'--model','gpt-6-astra','--dolt','/tmp/dolt-darwin-arm64/bin/dolt','--codex','/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex','--herdr','/Users/lbethay/.local/bin/herdr','--dynamic-provisioning','--scratch-cleanup']
exec(compile(code,str(source),'exec'),{'__file__':str(source),'__name__':'__main__'})
