#!/usr/bin/env bash
# Usage: deploy/setup.sh <dir> <public|hub> <init|pair|finish> [options]
#
# Pulls the pinned release tools image (with SATBOOTH_BUILD=1 in the environment: builds it from this repository
# instead) and runs `satbooth-setup` in it: no network, a read-only root, no capabilities, the calling user, and only
# <dir> (and each file named by -answers, -handoff or -wallet-rpc-pass, read-only) visible. <dir> is created with mode
# 0700 if it is missing; an existing directory is left as it is. A symbolic link as <dir> is refused.
# -dir and -host-dir are set here, never taken from the caller. With SATBOOTH_BUILD=1 the wrapper also adds
# -source-build, so the start commands the report prints name the compose.build.yml of the host. Options are passed through unchanged, in order.
# The command's report goes to standard output, its prompts and the image pull or build to standard error; the exit
# code is the command's (0 done, 1 refused or failed, 2 usage).
set -euo pipefail

case "${SATBOOTH_BUILD:-}" in
  "" | 0 | 1) ;;
  *) echo "setup.sh: SATBOOTH_BUILD must be empty, 0 or 1: ${SATBOOTH_BUILD}" >&2; exit 2 ;;
esac

usage() {
  echo "usage: deploy/setup.sh <dir> <public|hub> <init|pair|finish> [options]" >&2
  echo "       (options are those of satbooth-setup, except -dir and -host-dir, which are set here)" >&2
  exit 2
}

[ "$#" -ge 3 ] || usage
dir=$1
host=$2
stage=$3
shift 3
[ -n "$dir" ] || usage
case "$dir" in
  -*) echo "setup.sh: <dir> may not start with a dash: $dir" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
# The release image of the setup command, pinned by tag and digest (scripts/release-pins.sh). SATBOOTH_BUILD=1 builds
# it from this repository instead.
tools_image=ghcr.io/satbooth/satbooth-tools:v0.1.0-rc.1@sha256:9b2ff55fe8ef1b831f415c11334bbb34b9a4d3d1e364bfa76874f591f160fe2f

# Refuse a colon before anything is created (a relative <dir> is under the current directory).
case "$dir" in
  /*) full=$dir ;;
  *) full=$PWD/$dir ;;
esac
case "$full" in
  *:*) echo "setup.sh: the path of <dir> may not contain a colon (Docker's mount syntax): $dir" >&2; exit 2 ;;
esac

# Refuse a symbolic link as <dir>. Trailing slashes and a trailing "/." would hide it from -L, so they are stripped
# first; a last component of "." or ".." is refused as it is. The message does not repeat the path.
link=$dir
while [ "$link" != / ] && { [ "${link%/}" != "$link" ] || [ "${link%/.}" != "$link" ]; }; do
  case "$link" in
    */.) link=${link%/.} ;;
    *) link=${link%/} ;;
  esac
done
case "$link" in
  . | .. | */..) echo "setup.sh: <dir> may not end in . or ..: give the real directory" >&2; exit 2 ;;
esac
[ ! -L "$link" ] || { echo "setup.sh: <dir> is a symbolic link: give the real directory" >&2; exit 2; }

nocolon() { # <what> <path>: Docker's -v syntax splits on a colon
  case "$2" in
    *:*) echo "setup.sh: $1 may not contain a colon (Docker's mount syntax): $2" >&2; exit 2 ;;
  esac
}
mounts=()
opts=()
seen=" "
add_file() { # <option name> <host path>
  local name=$1 path=$2 fixed
  case "$name" in
    answers) fixed=/in/answers ;;
    handoff) fixed=/in/handoff ;;
    wallet-rpc-pass) fixed=/in/wallet-rpc-pass ;;
  esac
  case "$seen" in
    *" $name "*) echo "setup.sh: -$name given twice" >&2; exit 2 ;;
  esac
  seen="$seen$name "
  [ ! -L "$path" ] || { echo "setup.sh: -$name: a symbolic link, not a regular file: $path" >&2; exit 2; }
  [ -f "$path" ] || { echo "setup.sh: -$name: not a regular file: $path" >&2; exit 2; }
  local d b
  d=$(cd -- "$(dirname -- "$path")" && pwd -P)
  b=$(basename -- "$path")
  nocolon "-$name's path" "$d/$b"
  mounts+=(-v "$d/$b:$fixed:ro")
  opts+=("-$name" "$fixed")
}

while [ "$#" -gt 0 ]; do
  arg=$1
  shift
  case "$arg" in
    -dir | --dir | -dir=* | --dir=* | -host-dir | --host-dir | -host-dir=* | --host-dir=*)
      echo "setup.sh: $arg is set by the wrapper and cannot be given" >&2
      exit 2
      ;;
    -answers | --answers | -handoff | --handoff | -wallet-rpc-pass | --wallet-rpc-pass)
      [ "$#" -ge 1 ] || { echo "setup.sh: $arg needs a value" >&2; exit 2; }
      name=${arg#-}
      name=${name#-}
      add_file "$name" "$1"
      shift
      ;;
    -answers=* | --answers=* | -handoff=* | --handoff=* | -wallet-rpc-pass=* | --wallet-rpc-pass=*)
      name=${arg%%=*}
      name=${name#-}
      name=${name#-}
      add_file "$name" "${arg#*=}"
      ;;
    --)
      echo "setup.sh: a bare -- is not accepted" >&2
      exit 2
      ;;
    *)
      opts+=("$arg")
      ;;
  esac
done

# The wrapper's --user and the 0700 base directory only work on Docker Engine in its default mode: a rootless
# engine (or Podman) maps the process to a sub-uid that does not own the directory. No override.
info=$(docker info --format '{{range .SecurityOptions}}{{.}} {{end}}' 2>/dev/null) \
  || { echo "setup.sh: Docker is not reachable (docker info failed)" >&2; exit 2; }
version=$(docker --version 2>/dev/null || true)
case "$info $version" in
  *rootless* | *[Pp]odman*)
    echo "setup.sh: a rootless Docker engine or Podman is not supported by this wrapper (the bundles are qualified on Docker Engine in its default mode): see deploy/README.md" >&2
    exit 2
    ;;
esac

# Make <dir> if it is missing; then use its absolute, symlink-free path.
if [ ! -e "$dir" ]; then
  (umask 077; mkdir -p -m 0700 -- "$dir")
fi
[ -d "$dir" ] || { echo "setup.sh: not a directory: $dir" >&2; exit 2; }
absdir=$(cd -- "$dir" && pwd -P)
nocolon "the absolute path of <dir>" "$absdir"
hostopts=()
case "$host $stage" in
  "public init" | "hub init") hostopts=(-host-dir "$absdir") ;;
esac

if [ "${SATBOOTH_BUILD:-}" = 1 ]; then
  # From source: the tools image is built from this repository.
  run_image=satbooth-tools:local
  opts+=(-source-build)
  docker build -q --target satbooth-tools -t "$run_image" -f "$root/deploy/image/Dockerfile" "$root" >&2
else
  run_image=$tools_image
  # A ref pinned by digest is immutable: a copy that is already local is the same image, so the registry is needed
  # only when there is none.
  if ! docker image inspect "$run_image" > /dev/null 2>&1 && ! docker pull "$run_image" >&2; then
    echo "setup.sh: could not pull $run_image" >&2
    echo "setup.sh: check the network and the registry, or set SATBOOTH_BUILD=1 to build the image from source" >&2
    exit 1
  fi
fi

exec docker run --rm -i \
  --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
  --user "$(id -u):$(id -g)" \
  -v "$absdir:/work" "${mounts[@]+"${mounts[@]}"}" \
  "$run_image" /satbooth-setup "$host" "$stage" -dir /work "${hostopts[@]+"${hostopts[@]}"}" "${opts[@]+"${opts[@]}"}"
