#!/usr/bin/env bash
# Native prerequisites: JDK 17, Z3, SBT, C compiler, Bison and timeout.
# macOS: brew install openjdk@17 z3 sbt bison coreutils
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD
export GOCACHE="$root/.cache/go-build" GOPATH="$root/.tools/gopath"
export TMPDIR="$root/.cache/tmp"
mkdir -p "$GOCACHE" "$TMPDIR" .tools verification/logs
if [[ -d /opt/homebrew/opt/openjdk@17 ]]; then
 export JAVA_HOME=/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home
 export PATH="$JAVA_HOME/bin:/opt/homebrew/opt/bison/bin:/opt/homebrew/opt/coreutils/libexec/gnubin:$PATH"
fi
for executable in go java sbt z3 cc bison timeout; do command -v "$executable" >/dev/null || { echo "Missing native prerequisite: $executable" >&2; exit 1; }; done
checkout() {
 local url=$1 path=$2 revision=$3
 if [[ ! -d $path ]]; then
  git init "$path"
  git -C "$path" remote add origin "$url"
  git -C "$path" fetch --depth 1 origin "$revision"
  git -C "$path" checkout --detach FETCH_HEAD
 fi
 actual=$(git -C "$path" rev-parse HEAD)
 [[ $actual == "$revision" ]] || { printf 'Dependency revision mismatch at %s: expected %s, got %s\n' "$path" "$revision" "$actual" >&2; exit 1; }
}
checkout https://github.com/viperproject/gobra .tools/gobra d2c5af5450540c502855e53f4a5af1ba7630c41f
if [[ ! -f .tools/gobra/target/scala-2.13/gobra.jar ]]; then
 git -C .tools/gobra submodule update --init --recursive --depth 1
 (cd .tools/gobra; sbt assembly) > verification/logs/gobra-build.log 2>&1
fi
checkout https://github.com/nimble-code/Spin .tools/spin 090f74209025a53297990ec17deca1bd51cb92a5
if [[ ! -x .tools/spin/Src/spin ]]; then make -C .tools/spin/Src > verification/logs/spin-build.log 2>&1; fi
checkout https://github.com/nicolasdilley/Gomela .tools/gomela-pinned b115cd2fd9556566817f092635edaa667a580898
if [[ ! -x .tools/gomela-pinned-bin ]]; then
 (cd .tools/gomela-pinned
  if ! git apply --reverse --check "$root/verification/patches/gomela-go127.patch" 2>/dev/null; then
   git apply "$root/verification/patches/gomela-go127.patch"
  fi
  go mod tidy
  go build -o ../gomela-pinned-bin .)
fi
