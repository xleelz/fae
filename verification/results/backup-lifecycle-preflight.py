import pathlib,sys
source=pathlib.Path('scripts/dogfood.py').resolve();text=source.read_text()
code=text.split(' prompts={}')[0]
code+=text[text.index(' if a.backup_policy:\n'):text.index(' shutdown()\n owned=scratch')]
code+='''
 print(json.dumps({'preflight':'passed','backup_policy':result['backup_policy']}),flush=True)
 shutdown()
 cleanup=call('cleanup','scratch','--apply','--dolt',a.dolt,'--authorizing-message','authorization',timeout=240)
 assert cleanup['completed']
finally:
 if process is not None and process.poll() is None:
  process.terminate();process.wait(timeout=30)
 if replica_server is not None:
  replica_server.shutdown();replica_server.server_close();replica_thread.join(timeout=5)
 if effect_server is not None:
  effect_server.shutdown();effect_server.server_close();effect_thread.join(timeout=5)
'''
sys.argv=[str(source),'--model','gpt-6-astra','--dolt','/tmp/dolt-darwin-arm64/bin/dolt','--codex','/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex','--herdr','/Users/lbethay/.local/bin/herdr','--scratch-cleanup','--backup-policy']
exec(compile(code,str(source),'exec'),{'__file__':str(source),'__name__':'__main__'})
