package workflow

import (
	"path"
	"strings"
)

// ProtectedPath reports a path no model-chosen task scope may hold, and why
// (FAE-SEC-005): task text, and so every file a model derives from it, is
// untrusted, so a Spec's declared_files and a scope widen never put one of
// these in a task's scope. They are files that run programs or carry
// authority outside the task's own code:
//
//   - repository metadata: a .gitmodules file at any depth (a .git component
//     is already refused by DeclaredPathSafe);
//   - CI and forge configuration at the repository root: .github/, .gitlab/,
//     .gitlab-ci.yml, .circleci/, .buildkite/, .woodpecker/, .woodpecker.yml,
//     .travis.yml, .drone.yml, .cirrus.yml, appveyor.yml, .appveyor.yml,
//     azure-pipelines.yml, bitbucket-pipelines.yml and Jenkinsfile;
//   - configuration that makes the operator's tools run programs, at the
//     root: git hook managers (.husky/, .pre-commit-config.yaml,
//     lefthook.yml, .lefthook.yml), .envrc, dev containers (.devcontainer/,
//     .devcontainer.json), editor tasks (.vscode/) and coding-agent
//     configuration (.claude/, .codex/, .cursor/, .mcp.json);
//   - credential-like files at any depth: .ssh/, .aws/, .gnupg/, .docker/
//     and .kube/ directories; .env and .env.* (not .env.example, .sample or
//     .template); .netrc, _netrc, .npmrc, .pypirc, .pgpass, .htpasswd,
//     .git-credentials, .dockercfg, credentials, credentials.json,
//     kubeconfig and private keys named id_rsa, id_dsa, id_ecdsa or
//     id_ed25519; and key stores (.pem, .key, .p12, .pfx, .jks, .keystore,
//     .ppk) outside a testdata directory, where Go test fixtures live.
//
// Names compare without letter case, as on a case-insensitive file system.
// The operator can still name such a file: an admission's declared files
// carry the operator's authorization, and a pinned scope is configuration.
func ProtectedPath(name string) (string, bool) {
	parts := strings.Split(strings.ToLower(name), "/")
	base := parts[len(parts)-1]
	if protectedRootCI[parts[0]] {
		return "CI or forge configuration", true
	}
	if protectedRootTools[parts[0]] {
		return "configuration that makes the operator's tools run programs", true
	}
	fixture := false
	for i, part := range parts {
		if part == ".gitmodules" {
			return "repository metadata", true
		}
		if i < len(parts)-1 && protectedCredentialDirs[part] {
			return "a credential directory", true
		}
		if part == "testdata" && i < len(parts)-1 {
			fixture = true
		}
	}
	switch {
	case protectedCredentialNames[base]:
		return "a credential-like file", true
	case base == ".env" || strings.HasPrefix(base, ".env.") && !protectedEnvExamples[strings.TrimPrefix(base, ".env.")]:
		return "a credential-like file", true
	case !fixture && protectedKeyExtensions[path.Ext(base)]:
		return "a credential-like file", true
	}
	return "", false
}

var (
	protectedRootCI = map[string]bool{
		".github": true, ".gitlab": true, ".gitlab-ci.yml": true, ".circleci": true, ".buildkite": true, ".woodpecker": true, ".woodpecker.yml": true,
		".travis.yml": true, ".drone.yml": true, ".cirrus.yml": true, "appveyor.yml": true, ".appveyor.yml": true, "azure-pipelines.yml": true, "bitbucket-pipelines.yml": true, "jenkinsfile": true,
	}
	protectedRootTools = map[string]bool{
		".husky": true, ".pre-commit-config.yaml": true, "lefthook.yml": true, ".lefthook.yml": true, ".envrc": true, ".devcontainer": true, ".devcontainer.json": true,
		".vscode": true, ".claude": true, ".codex": true, ".cursor": true, ".mcp.json": true,
	}
	protectedCredentialDirs  = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true, ".docker": true, ".kube": true}
	protectedCredentialNames = map[string]bool{
		".netrc": true, "_netrc": true, ".npmrc": true, ".pypirc": true, ".pgpass": true, ".htpasswd": true, ".git-credentials": true, ".dockercfg": true,
		"credentials": true, "credentials.json": true, "kubeconfig": true, "id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	}
	protectedEnvExamples   = map[string]bool{"example": true, "sample": true, "template": true}
	protectedKeyExtensions = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".keystore": true, ".ppk": true}
)
