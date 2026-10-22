import pathlib,sys
source=pathlib.Path('scripts/dogfood.py').resolve();text=source.read_text()
code=text.split(' prompts={}')[0]
code+=text[text.index(' if a.backup_policy:\n'):text.index(' owned=scratch')]
code+=text[text.rindex(' if a.scratch_cleanup:\n'):text.index(" result['status']='passed'\nexcept Exception as e:")]
code+='''
 print(json.dumps({'preflight':'passed','backup_schedules':result['backup_schedules'],'retention_automation':result['retention_automation'],'scratch_archive_cleanup':result['scratch_archive_cleanup']}),flush=True)
finally:
 if process is not None and process.poll() is None:
  process.terminate();process.wait(timeout=30)
 if replica_server is not None:
  replica_server.shutdown();replica_server.server_close();replica_thread.join(timeout=5)
 if effect_server is not None:
  effect_server.shutdown();effect_server.server_close();effect_thread.join(timeout=5)
'''
sys.argv=[str(source),'--model','gpt-6-astra','--dolt','/tmp/dolt-darwin-arm64/bin/dolt','--codex','/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex','--herdr','/Users/lbethay/.local/bin/herdr','--backup-schedules']
exec(compile(code,str(source),'exec'),{'__file__':str(source),'__name__':'__main__'})
