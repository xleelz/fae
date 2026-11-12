//go:build darwin && cgo

package controller

/*
#include <dlfcn.h>
#include <sys/types.h>
static int fae_sandboxed(int pid) {
 void *lib=dlopen("/usr/lib/system/libsystem_sandbox.dylib",RTLD_LAZY);
 if(!lib)return -1;
 int (*check)(pid_t,const char*,int,...)=dlsym(lib,"sandbox_check");
 if(!check){dlclose(lib);return -1;}
 int result=check(pid,NULL,0);dlclose(lib);return result;
}
*/
import "C"
import "errors"

func Unsandboxed(pid int) (bool, error) {
	result := int(C.fae_sandboxed(C.int(pid)))
	if result < 0 {
		return false, errors.New("native sandbox state unavailable")
	}
	return result == 0, nil
}

// sandboxCondition names the inherited macOS sandbox of pid.
func sandboxCondition(int) string { return "macOS App Sandbox profile" }
