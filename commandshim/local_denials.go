package commandshim

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// InstallLocalDenials is InstallDenials for a Local task's brokered run or a
// Local role session without a broker (FAE-MODE-002): its git wrapper exits
// 78 with the Local-mode reason for a push or remote change (push, remote,
// send-pack, http-push), found past git's global options as RunShim finds it
// (gitSubcommand), and refuses every other call with InstallDenials' text and
// 77. The other wrappers are InstallDenials'.
func InstallLocalDenials(parent string) (string, error) {
	dir, err := InstallDenials(parent)
	if err != nil {
		return "", err
	}
	git := filepath.Join(dir, "git")
	denial, err := os.ReadFile(git)
	if err != nil {
		return "", err
	}
	script := "#!/bin/sh\n" +
		"# FAE-MODE-002: a Local task's push or remote change exits 78; every other git call is refused below.\n" +
		"sub= skip=\n" +
		"for a in \"$@\"; do\n" +
		"\tif [ -n \"$skip\" ]; then skip=; continue; fi\n" +
		"\tcase $a in\n" +
		"\t" + strings.Join(slices.Sorted(maps.Keys(gitValueOptions)), "|") + ") skip=1 ;;\n" +
		"\t-*) ;;\n" +
		"\t*) sub=$a; break ;;\n" +
		"\tesac\n" +
		"done\n" +
		"case $sub in\n" +
		strings.Join(slices.Sorted(maps.Keys(gitExternalSubcommands)), "|") + ")\n" +
		"\tprintf 'fae: command refused in Local mode (exit 78): git %s: a Local task makes no external effect, so pushes and remote changes are refused\\n' \"$sub\" >&2\n" +
		"\texit 78 ;;\n" +
		"esac\n" +
		strings.TrimPrefix(string(denial), "#!/bin/sh\n")
	if err = os.Remove(git); err != nil {
		return "", err
	}
	if err = os.WriteFile(git, []byte(script), 0500); err != nil {
		return "", err
	}
	return dir, nil
}
