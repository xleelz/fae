package workflow

import "testing"

// FAE-SEC-005: the protected-path rule a model-chosen task scope is held to.
// CI and forge configuration and the operator's tool configuration count at
// the repository root only; repository metadata, credential directories and
// credential-like names count at any depth; key stores count outside a
// testdata directory; names compare without letter case.
func TestProtectedPathCorpus(t *testing.T) {
	protected := map[string]string{
		".github/workflows/test.yml": "CI", ".GitHub/CODEOWNERS": "CI", ".gitlab-ci.yml": "CI", ".gitlab/ci/x.yml": "CI", ".circleci/config.yml": "CI", ".buildkite/pipeline.yml": "CI",
		".travis.yml": "CI", ".drone.yml": "CI", "Jenkinsfile": "CI", "azure-pipelines.yml": "CI", "bitbucket-pipelines.yml": "CI", ".woodpecker/build.yml": "CI",
		".husky/pre-commit": "tools", ".pre-commit-config.yaml": "tools", "lefthook.yml": "tools", ".envrc": "tools", ".devcontainer/devcontainer.json": "tools",
		".vscode/tasks.json": "tools", ".claude/settings.json": "tools", ".codex/config.toml": "tools", ".cursor/rules": "tools", ".mcp.json": "tools",
		".gitmodules": "metadata", "vendor/x/.gitmodules": "metadata",
		"home/.ssh/config": "credential directory", ".aws/credentials": "credential directory", "x/.gnupg/pubring.kbx": "credential directory", ".docker/config.json": "credential directory", ".kube/config": "credential directory",
		".env": "credential", "deploy/.env.production": "credential", ".netrc": "credential", "_netrc": "credential", "web/.npmrc": "credential", ".pypirc": "credential",
		".pgpass": "credential", ".htpasswd": "credential", ".git-credentials": "credential", "credentials.json": "credential", "kubeconfig": "credential",
		"keys/id_rsa": "credential", "id_ed25519": "credential", "tls/server.pem": "credential", "tls/server.KEY": "credential", "store.p12": "credential", "a.jks": "credential", "putty.ppk": "credential",
	}
	for name, kind := range protected {
		if why, ok := ProtectedPath(name); !ok || why == "" {
			t.Errorf("%s (%s) not protected", name, kind)
		}
	}
	for _, name := range []string{"main.go", "pkg/answer_test.go", ".gitignore", ".gitattributes", "docs/.nojekyll", ".env.example", ".env.sample", ".env.template", "README.md",
		"nested/.github/workflows/test.yml", "tools/Jenkinsfile.md", "pkg/testdata/server.key", "x/testdata/certs/ca.pem", "github/workflow.go", "src/.github.go", "id_rsa.pub", "keyboard.go", "cmd/credentials_test.go", ".ssh", "environment.go"} {
		if why, ok := ProtectedPath(name); ok {
			t.Errorf("%s protected (%s)", name, why)
		}
	}
}
