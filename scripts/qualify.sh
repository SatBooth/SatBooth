#!/usr/bin/env bash
# Dev-only qualification against real Alby Hub containers on a regtest network (dev/regtest/compose.yml).
# Needs Docker with Compose; not part of scripts/check.sh. Usage: scripts/qualify.sh [--keep] [--only <group>]
#   --keep          leave the Compose project (containers, volumes) up afterwards, whatever the outcome.
#   --only <group>  run the setup (the build, TestBuildFlags, the hubs and TestBootstrap, the relays and the hub
#                   restart), then only that group, then the end checks; for a targeted run, not the qualification.
#                   Groups: deploy (TestDeployPublic); image (TestOnchainSetup and TestMoneroSetup, then the
#                   TestImage phases and TestImageBudget); refund (the TestRefund phases, about 15 minutes);
#                   hub (TestQualify and the check on the hub's unknown-app lines, about 1.5 minutes); onchain (every
#                   on-chain phase of the full run, TestOnchainSetup to TestOnchainStuckAfter, with the bitcoind kills
#                   between them, about 15 minutes).
# On failure the logs are saved to dev/regtest/logs/ (git-ignored) first.
set -euo pipefail

cd "$(dirname "$0")/.."

usage() { echo "usage: scripts/qualify.sh [--keep] [--only deploy|image|refund|hub|onchain]" >&2; exit 2; }
keep=0
only=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --keep) keep=1 ;;
    --only)
      [ "$#" -ge 2 ] || usage
      case "$2" in
        deploy | image | refund | hub | onchain) only=$2 ;;
        *) usage ;;
      esac
      shift
      ;;
    *) usage ;;
  esac
  shift
done

# Qualification builds everything from this tree: deploy/setup.sh builds its tools image, and the bundles are started
# with their compose.build.yml (no registry is used).
export SATBOOTH_BUILD=1

project=satbooth-regtest
compose=dev/regtest/compose.yml
logdir=dev/regtest/logs
hubs=(hub-satbooth hub-merchant)
hub_ips=(172.30.78.10 172.30.78.20)

# Both profiles on every call: manual (the relays, the qualify container) and image (the satbooth release image, its
# one-off runs and volume-init), so that down -v and the failure logs cover every container and volume.
dc() { docker compose --profile manual --profile image -p "$project" -f "$compose" "$@"; }
log() { printf '== qualify: %s\n' "$*"; }

# The public host's bundle (TestDeployPublic): deploy/public/compose.yml with its compose.build.yml, with the regtest override and
# values, as a project of its own. Its two host directories exist only while its phases run; the placeholders let
# Compose interpolate the file for build and down outside them.
deploy_project=satbooth-regtest-deploy
deploy_root="" # a temporary directory of this user's, holding etc/ (SATBOOTH_ETC) and backups/ (SATBOOTH_BACKUPS)
deploy_etc=""
deploy_backups=""
ddc() {
  SATBOOTH_ETC="${deploy_etc:-/nonexistent}" SATBOOTH_BACKUPS="${deploy_backups:-/nonexistent}" \
    docker compose --profile oneoff -p "$deploy_project" -f deploy/public/compose.yml \
    -f deploy/public/compose.build.yml -f dev/regtest/deploy-public.override.yml --env-file dev/regtest/deploy-public.env "$@"
}
# rm_tree <dir>: removes a host directory of this run, and what other users own inside it (a container empties it: it
# belongs to root, 65532 or a service's user), then the directory itself, which stays this user's.
rm_tree() {
  [ -n "$1" ] && [ -d "$1" ] || return 0
  dc run --rm --no-deps -T --cap-add DAC_OVERRIDE -v "$1:/x" --entrypoint sh volume-init -c \
    'find /x -mindepth 1 -delete' > /dev/null 2>&1 || true
  rmdir "$1" 2> /dev/null || echo "qualify.sh: could not remove $1" >&2
}
# Down with its volume, then the host directories. It runs before the regtest project's down, whose network the
# bundle's container joins.
deploy_down() {
  ddc down -v --remove-orphans > /dev/null 2>&1 || true
  rm_tree "$deploy_root"
  deploy_root=""
  deploy_etc=""
  deploy_backups=""
}

# The hub host's bundle (the second half of TestDeployPublic's group): deploy/hub/compose.yml with its compose.build.yml, as a project of
# its own, from the env file `satbooth-setup hub init` wrote. hub_pub_root is the scratch directory of the stage 1 run
# whose handoff the hub's stage 2 reads; deploy_tmp holds the answers files and the handoff, and is removed last.
hub_project=satbooth-regtest-deploy-hub
hub_root=""
hub_pub_root=""
hub_override="" # a test-only ports file, set only when a published port of this host is taken
deploy_tmp=""
# The stage 2 answers keep the admin address (127.0.0.3) apart from the two private-link addresses (setup's rule). On
# macOS, Docker Desktop binds 127.0.0.1 alone, so there HUB_ADMIN_ADDR=127.0.0.1 is given to Compose over the env file;
# elsewhere the generated value is used as written.
hdc() {
  local files=(-f deploy/hub/compose.yml -f deploy/hub/compose.build.yml)
  [ -z "$hub_override" ] || files+=(-f "$hub_override")
  if [ "$(uname -s)" = Darwin ]; then
    HUB_ADMIN_ADDR=127.0.0.1 docker compose -p "$hub_project" "${files[@]}" --env-file "${hub_root:-/nonexistent}/compose.env" "$@"
  else
    docker compose -p "$hub_project" "${files[@]}" --env-file "${hub_root:-/nonexistent}/compose.env" "$@"
  fi
}
hub_down() {
  if [ -n "$hub_root" ] && [ -f "$hub_root/compose.env" ]; then
    hdc down -v --remove-orphans > /dev/null 2>&1 || true
  fi
  rm_tree "$hub_root"
  rm_tree "$hub_pub_root"
  hub_root=""
  hub_pub_root=""
}
deploy_cleanup() {
  deploy_down
  hub_down
  rm_tree "$deploy_tmp"
  deploy_tmp=""
}

# finished is set only after the last phase. macOS's bash 3.2 runs the EXIT trap with $? = 0 after a "set -u" abort
# (an unbound variable), so a run that did not reach the end counts as a failure whatever $? says.
finished=0
cleanup() {
  status=$?
  if [ "$status" -eq 0 ] && [ "$finished" -ne 1 ]; then
    echo "qualify.sh: stopped before the end of the run" >&2
    status=1
  fi
  if [ "$status" -ne 0 ]; then
    mkdir -p "$logdir"
    dc logs --no-color --timestamps > "$logdir/compose.log" 2>&1 || true
    ddc logs --no-color --timestamps > "$logdir/deploy-public.log" 2>&1 || true
    if [ -n "$hub_root" ] && [ -f "$hub_root/compose.env" ]; then
      hdc logs --no-color --timestamps > "$logdir/deploy-hub.log" 2>&1 || true
    fi
    echo "qualify.sh: FAIL (exit $status); logs in $logdir/compose.log, $logdir/deploy-public.log and $logdir/deploy-hub.log" >&2
  fi
  if [ "$keep" -eq 1 ]; then
    echo "qualify.sh: --keep: projects $project, $deploy_project and $hub_project left up" >&2
  else
    deploy_cleanup
    dc down -v --remove-orphans > /dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

# phase_passed <test> <output file>: the output of a phase's run shows that the top-level test passed ("--- PASS: <test> ("
# at the start of a line, so a subtest's line does not count), and has no "no tests to run".
phase_passed() {
  ! grep -qF 'no tests to run' "$2" && grep -qE "^--- PASS: $1 \(" "$2"
}

# One test phase: a single run of the qualify container (service $1) for the top-level test $2. The name is anchored so
# that a phase never matches a longer test name. The output goes to the log as it comes (tee), and the phase passes only
# if the run exits 0 and its output shows that test passed: a test that was renamed, removed or skipped fails the phase.
phase_in() {
  local out rc=0
  log "phase $2"
  out=$(mktemp)
  dc run --rm --no-deps -T -e "PHASE=^$2\$" "$1" 2>&1 | tee "$out" || rc=$?
  if [ "$rc" -eq 0 ] && ! phase_passed "$2" "$out"; then
    echo "phase $2: the run does not show $2 passing (a missing, renamed or skipped test)" >&2
    rc=1
  fi
  rm -f "$out"
  return "$rc"
}

phase() { phase_in qualify "$1"; }

# A phase of TestDeployPublic: the qualify container on the bundle's network too (service qualify-deploy).
phase_deploy() { phase_in qualify-deploy "$1"; }

# The output of a one-off run of the image ($2) has no ALERT line and no "did not close cleanly" line ($1 names the run).
oneoff_clean() {
  local bad
  for bad in "ALERT" "did not close cleanly"; do
    if grep -qF -- "$bad" <<< "$2"; then
      echo "$1: a line with '$bad'" >&2
      return 1
    fi
  done
}

# The public host's bundle (plan 10), from the setup command's output (plan 12): deploy/public/compose.yml,
# run with its compose.build.yml, the regtest override and values (dev/regtest/deploy-public.override.yml, deploy-public.env) as a
# project of its own. A second SatBooth with its own data volume. Every file the bundle mounts is written by
# `deploy/setup.sh <dir> public init` and `public finish`, which run before the bundle's first start; the test patches
# only the regtest topology into satbooth.json (three keys, listed in qualify/REPORT.md). Lightning only, no payout
# block. It runs after the satbooth container stopped for good (both use the invoice connection), and before
# TestImageBudget, which then shows that it made no payout. Its phases run in qualify-deploy (on both networks).
# deploy_hub_group follows it: the hub host's bundle from stages 1 to 3.
mode_of() { stat -c '%a' "$1" 2> /dev/null || stat -f '%Lp' "$1"; }

# digest_of <stage output>: the SHA-256 the stage printed after "Digest of the handoff file", as 64 hex characters.
printed_digest() {
  printf '%s\n' "$1" | awk '/^Digest of the handoff file/ { getline; gsub(/[ \t]/, ""); print; exit }'
}

sha256_of() { # <file>
  if command -v sha256sum > /dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# check_digest <what> <printed output> <file>: the digest the stage printed is the file's SHA-256, both ways round.
check_digest() {
  local printed own
  printed=$(printed_digest "$2")
  own=$(sha256_of "$3")
  [ "${#printed}" -eq 64 ] && [ "$printed" = "$own" ] || { echo "$1: the printed digest and the file's SHA-256 differ" >&2; return 1; }
  log "$1: the printed digest equals the file's SHA-256"
  digest_value=$printed
}

deploy_group() {
  log "deploy/public: the bundle against the regtest hub side, from the setup command's output"
  command -v jq > /dev/null || { echo "qualify.sh: jq is required for the deploy group" >&2; return 1; }
  deploy_root=$(mktemp -d)
  deploy_root=$(cd "$deploy_root" && pwd -P) # the wrapper mounts the physical path
  deploy_etc=$deploy_root/etc
  deploy_backups=$deploy_root/backups
  deploy_tmp=$(mktemp -d)
  deploy_tmp=$(cd "$deploy_tmp" && pwd -P)
  # Stage 1. The regtest hub side is in 172.30.78.0/24, which the command refuses as a private-link address (it is the
  # bundle's default Docker network): documentation addresses stand in, and three keys of satbooth.json are patched.
  # Refunds are on (the public key of RFC 8032's first test vector, the caps of TestDeployPublicConnect's budget), so the
  # bundle starts with the generated payout block; it makes no refund.
  jq -n --arg host "$deploy_root" '{network: "regtest", host_dir: $host, hub_private_addr: "198.51.100.20",
    public_private_addr: "198.51.100.10", checkout_base_url: "http://192.0.2.40:8080", webhook_url: "http://172.30.78.30:9100/hook",
    approval_keys: ["d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"],
    payout_budget_sat: 1000000, payout_max_sat: 100000, payout_daily_sat: 500000, payout_alert_sat: 0}' \
    > "$deploy_tmp/public-answers.json"
  log "setup: public init"
  local out digest_value=""
  out=$(deploy/setup.sh "$deploy_root" public init -answers "$deploy_tmp/public-answers.json")
  check_digest "setup public init" "$out" "$deploy_root/setup/public-to-hub.json"
  [ "$(LC_ALL=C ls -1A "$deploy_root/setup/api-keys" | tr '\n' ' ')" = 'orders-read.key orders-write.key refunds-read.key refunds-write.key ' ] \
    || { echo "deploy/public: stage 1 wrote other API key files than the orders and refunds pairs" >&2; return 1; }
  # The handoff (public values), the two plain API keys and the webhook secret go into the state volume for the Go phases
  # (root, to read this user's 0600 files; umask 077): nothing is printed.
  dc run --rm --no-deps -T --cap-add DAC_OVERRIDE -v "$deploy_root:/host:ro" --entrypoint sh volume-init -c \
    'set -eu; umask 077; mkdir -p /state/deploy/api-keys
    cp /host/setup/public-to-hub.json /host/etc/webhook.secret /state/deploy/
    cp /host/setup/api-keys/orders-read.key /host/setup/api-keys/orders-write.key /state/deploy/api-keys/'
  # The hub's side: the connection for the client key of stage 1 and its pair in the regtest relay, then the handoff.
  phase TestDeployPublicConnect
  recreate_relay hub-satbooth
  dc run --rm --no-deps -T --cap-add DAC_OVERRIDE -v "$deploy_tmp:/out" --entrypoint sh volume-init -c \
    'set -eu; cp /state/deploy/hub-to-public.json /state/deploy/hub-to-public.digest /out/; chmod 0644 /out/hub-to-public.json /out/hub-to-public.digest'
  local digest3
  digest3=$(tr -d ' \n' < "$deploy_tmp/hub-to-public.digest")
  log "setup: public finish"
  deploy/setup.sh "$deploy_root" public finish -handoff "$deploy_tmp/hub-to-public.json" -digest "$digest3" > /dev/null
  [ "$(mode_of "$deploy_root/compose.env")" = 600 ] || { echo "deploy/public: compose.env is not mode 0600" >&2; return 1; }
  docker compose -f deploy/public/compose.yml -f deploy/public/compose.build.yml --env-file "$deploy_root/compose.env" config -q
  log "deploy/public: the generated compose.env exists, is mode 0600 and is accepted by the bundle's compose.yml"
  # Every file the bundle mounts is the setup command's: the directory holds exactly the files stage 1 and stage 4 wrote.
  [ "$(LC_ALL=C ls -1A "$deploy_etc" | tr '\n' ' ')" = 'api-keys.json nwc-client.key payout.key satbooth.json webhook.secret ' ] \
    || { echo "deploy/public: etc/ does not hold exactly the files the setup command writes" >&2; return 1; }
  local f
  for f in "$deploy_etc"/*; do
    [ "$(mode_of "$f")" = 600 ] || { echo "deploy/public: $(basename "$f") is not mode 0600" >&2; return 1; }
  done
  # The regtest topology, in the generated satbooth.json and nothing else. First the generated values are asserted (what the
  # setup command must have written from the answers and the generated env), so that a drift in it fails here and is not
  # overwritten by the patch; then the three addresses and the two relay URLs are patched.
  local cfg=$deploy_etc/satbooth.json sbip
  sbip=$(sed -n 's/^SATBOOTH_IP=//p' "$deploy_root/compose.env")
  jq -e --arg pub "$sbip:8080" --arg prv "$sbip:8081" '.public_listen == $pub and .private_listen == $prv
    and .nwc.network == "regtest" and .payout.network == "regtest"
    and .nwc.relay_url == "ws://198.51.100.20:7448" and .payout.relay_url == "ws://198.51.100.20:7448"
    and (.webhooks | length) == 1 and .webhooks[0].name == "main" and .webhooks[0].url == "http://172.30.78.30:9100/hook"
    and (has("webhook") | not) and .checkout_base_url == "http://192.0.2.40:8080"
    and (.onchain | not) and (.alerts | not)' "$cfg" > /dev/null \
    || { echo "deploy/public: the generated satbooth.json is not what the answers and compose.env say" >&2; return 1; }
  jq --arg pub 192.0.2.40:8080 --arg prv 172.30.78.41:8081 --arg relay ws://172.30.78.10:7448 \
    '.public_listen = $pub | .private_listen = $prv | .nwc.relay_url = $relay | .payout.relay_url = $relay' "$cfg" > "$deploy_tmp/satbooth.json"
  local del='del(.public_listen, .private_listen, .nwc.relay_url, .payout.relay_url)'
  [ "$(jq -S "$del" "$cfg")" = "$(jq -S "$del" "$deploy_tmp/satbooth.json")" ] \
    || { echo "deploy/public: the patch changed more than its four keys" >&2; return 1; }
  cat "$deploy_tmp/satbooth.json" > "$cfg" # in place: the file keeps its owner and mode 0600
  log "deploy/public: patched satbooth.json: public_listen, private_listen, nwc.relay_url, payout.relay_url"
  phase TestDeployPublicPrepare
  log "up: deploy/public (volume-init first)"
  ddc up -d satbooth
  deploy_cid=$(ddc ps -a -q satbooth)
  phase_deploy TestDeployPublicStart

  # The hardening and the one published port, as the engine applied them.
  log "deploy/public: the container as the engine runs it"
  deploy_inspect() { docker inspect -f "$1" "$deploy_cid"; }
  want_eq() { # <what> <got> <want>
    [ "$2" = "$3" ] || { echo "deploy/public: $1 is '$2', want '$3'" >&2; return 1; }
    log "deploy/public: $1 = $2"
  }
  want_eq "read-only root" "$(deploy_inspect '{{.HostConfig.ReadonlyRootfs}}')" true
  want_eq "dropped capabilities" "$(deploy_inspect '{{json .HostConfig.CapDrop}}')" '["ALL"]'
  want_eq "added capabilities" "$(deploy_inspect '{{json .HostConfig.CapAdd}}')" null
  want_eq "security options" "$(deploy_inspect '{{json .HostConfig.SecurityOpt}}')" '["no-new-privileges:true"]'
  want_eq "memlock ulimit" "$(deploy_inspect '{{range .HostConfig.Ulimits}}{{.Name}}={{.Soft}}/{{.Hard}} {{end}}')" 'memlock=-1/-1 '
  want_eq "user" "$(deploy_inspect '{{.Config.User}}')" 65532:65532
  want_eq "restart policy" "$(deploy_inspect '{{.HostConfig.RestartPolicy.Name}}')" unless-stopped
  want_eq "restart count" "$(deploy_inspect '{{.RestartCount}}')" 0
  want_eq "port bindings" "$(deploy_inspect '{{json .HostConfig.PortBindings}}')" '{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"19101"}]}'
  want_eq "published ports" "$(docker port "$deploy_cid" | tr '\n' ' ')" '8080/tcp -> 127.0.0.1:19101 '
  # The published port serves the checkout page's stylesheet from this host, the way a reverse proxy reaches it.
  want_eq "GET /static/style.css on the published port" \
    "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 http://127.0.0.1:19101/static/style.css || true)" 200
  deploy_lines() { docker logs "$deploy_cid" 2>&1 | grep -cF -- "$1" || true; }
  deploy_check() { # <started lines expected> <stopped lines expected>; every start completes, so memory locked = started
    local what want got
    for what in "satbooth started:$1" "satbooth stopped:$2" "memory locked; core dumps disabled:$1" "startup failed:0" \
      "ALERT:0" "ended unexpectedly:0" "did not close cleanly:0"; do
      want=${what##*:}
      got=$(deploy_lines "${what%:*}")
      [ "$got" -eq "$want" ] || { echo "deploy/public log: $got lines with '${what%:*}', want $want" >&2; return 1; }
    done
    log "deploy/public log as expected: $1 started, $2 stopped, $1 memory locked, no startup failed, no ALERT"
  }
  deploy_stop() { # <started lines expected>: a graceful stop exits 0 by itself
    log "deploy/public: stop"
    ddc stop -t 60 satbooth > /dev/null
    want_eq "exit code on a graceful stop" "$(deploy_inspect '{{.State.ExitCode}}')" 0
    deploy_check "$1" "$1"
  }
  deploy_check 1 0

  phase_deploy TestDeployPublicPay
  want_eq "restart count" "$(deploy_inspect '{{.RestartCount}}')" 0

  deploy_stop 1
  # The bundle's one-off service, as deploy/README.md "Backups" runs it: a backup of the stopped service's data.
  rc=0
  out=$(ddc run --rm --no-deps -T satbooth-oneoff backup -config /etc/satbooth/satbooth.json -to /backups/d1 2>&1) || rc=$?
  printf '%s\n' "$out" | sed 's/^/  deploy backup: /'
  [ "$rc" -eq 0 ] || { echo "the bundle's satbooth backup exited $rc, expected 0" >&2; exit 1; }
  grep -qF '"msg":"backup written"' <<< "$out" || { echo "the bundle's satbooth backup wrote no success line" >&2; exit 1; }
  oneoff_clean "the bundle's satbooth backup" "$out"

  # One more start after the backup: an order the backup does not hold, so the restore must remove it.
  log "deploy/public: start after the backup"
  ddc start satbooth > /dev/null
  phase_deploy TestDeployPublicAfterBackup
  deploy_stop 2

  # The restore of deploy/README.md "Restore", with its commands (ddc is the README's dc with this run's files).
  log "deploy/public: restore d1 as deploy/README.md does"
  ddc stop satbooth
  ddc run --rm --no-deps volume-init
  deploy_restore d1
  ddc start satbooth
  phase_deploy TestDeployPublicRestored
  phase_deploy TestDeployPublicNoPayout
  deploy_stop 3
  deploy_down
  deploy_hub_group
}

# port_free <port>: nothing listens on 127.0.0.1:<port> of this host.
port_free() { ! (exec 3<> "/dev/tcp/127.0.0.1/$1") 2> /dev/null; }

# rpc_post <method>: bitcoind's JSON-RPC over the published port; stdin is a curl config with the login (user = "u:p"),
# which is never in argv. Prints the HTTP code only; the body is kept in $deploy_tmp/rpc.body (no secret in it).
rpc_post() {
  curl -s --max-time 20 -K - -o "$deploy_tmp/rpc.body" -w '%{http_code}' -H 'content-type: text/plain;' \
    --data-binary "{\"jsonrpc\":\"1.0\",\"id\":\"q\",\"method\":\"$1\",\"params\":[]}" "http://127.0.0.1:$hub_rpc_port/"
}
# hub_login: the hub user's curl config line, read from config/hub.env by a root helper (the file is the hub's, mode
# 0600, after volume-init). The line goes into a pipe to curl and nowhere else.
hub_login() {
  dc run --rm --no-deps -T --cap-add DAC_OVERRIDE -v "$hub_root/config:/c:ro" --entrypoint sh volume-init -c \
    "sed -n 's/^LDK_BITCOIND_RPC_PASSWORD=\\(.*\\)\$/user = \"hub:\\1\"/p' /c/hub.env" 2> /dev/null
}
sb_login() { # <password>
  printf 'user = "satbooth:%s"\n' "$1"
}
want_code() { # <what> <got> <want>
  [ "$2" = "$3" ] || { echo "deploy/hub: $1: got $2, want $3" >&2; return 1; }
  log "deploy/hub: $1: $2"
}

# hub_runs <when>: the hub container itself runs steadily (it is stage 2's whole point: the operator opens its page next).
# After 8 s, as the relay check waits: State.Running true and RestartCount 0; and its HTTP port answers on the published
# admin address (any HTTP status will do, a connection failure will not; only the status is printed). The admin address is
# the generated 127.0.0.3, or 127.0.0.1 where the run gives that (macOS) or the ports file is in use.
hub_runs() {
  local hid state code deadline host=127.0.0.3
  if [ "$(uname -s)" = Darwin ] || [ -n "$hub_override" ]; then host=127.0.0.1; fi
  sleep 8
  hid=$(hdc ps -a -q hub)
  state=$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$hid")
  [ "$state" = 'true 0' ] || { echo "deploy/hub: the hub is not running steadily $1 (running, restarts: $state)" >&2; docker logs --tail 20 "$hid" >&2 || true; return 1; }
  log "deploy/hub: the hub runs $1, restart count 0"
  deadline=$((SECONDS + 60))
  until code=$(curl -s -o /dev/null --max-time 5 -w '%{http_code}' "http://$host:$hub_admin_port/api/info") && [ "$code" != 000 ]; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "deploy/hub: the hub's HTTP port does not answer on $host:$hub_admin_port $1" >&2; docker logs --tail 20 "$hid" >&2 || true; return 1; }
    sleep 2
  done
  [ "$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$hid")" = 'true 0' ] || { echo "deploy/hub: the hub restarted while its port was polled $1" >&2; return 1; }
  log "deploy/hub: the hub's HTTP port answers $1 on $host:$hub_admin_port: HTTP $code"
}

# The hub host's bundle, from stages 1 to 3 of the setup command (plan 12): the first start (volume-init, the
# namespace holder, bitcoind and the hub), bitcoind's RPC users, then the relay once stage 3 wrote its configuration.
# Subnets: 172.30.79.0/25 (the hub), 172.30.79.128/25 (Electrum), apart from the regtest network (172.30.78.0/24) and the
# public bundle's (192.0.2.0/24). The private-link addresses are loopback ones (regtest only): a second stage 1 run, with
# Lightning, on-chain and a refund key, makes the handoff for them. Fulcrum and the Monero profile are not started.
deploy_hub_group() {
  log "deploy/hub: the bundle from stages 1 to 3 of the setup command"
  hub_pub_root=$(mktemp -d)
  hub_pub_root=$(cd "$hub_pub_root" && pwd -P)
  hub_root=$(mktemp -d)
  hub_root=$(cd "$hub_root" && pwd -P)
  local out digest_value=""
  # The Ed25519 public key of RFC 8032's first test vector: a refund approval key, so that stage 1 makes the payout key.
  jq -n --arg host "$hub_pub_root" '{network: "regtest", host_dir: $host, hub_private_addr: "127.0.0.1",
    public_private_addr: "127.0.0.2", checkout_base_url: "http://127.0.0.1:8080", webhook_url: "http://127.0.0.1:9/hook",
    descriptor: "wpkh(tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp/0/*)",
    approval_keys: ["d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"],
    payout_budget_sat: 1000000, payout_max_sat: 100000, payout_daily_sat: 500000, payout_alert_sat: 0}' \
    > "$deploy_tmp/hub-public-answers.json"
  log "setup: public init (the hub step's own)"
  out=$(deploy/setup.sh "$hub_pub_root" public init -answers "$deploy_tmp/hub-public-answers.json")
  check_digest "setup public init (hub step)" "$out" "$hub_pub_root/setup/public-to-hub.json"
  local handoff=$hub_pub_root/setup/public-to-hub.json digest2=$digest_value
  [ -f "$hub_pub_root/etc/bitcoind.pass" ] || { echo "deploy/hub: stage 1 wrote no bitcoind.pass" >&2; return 1; }

  local answers=$deploy_tmp/hub-answers.json
  jq -n --arg host "$hub_root" '{network: "regtest", host_dir: $host, hub_private_addr: "127.0.0.1",
    public_private_addr: "127.0.0.2", hub_admin_addr: "127.0.0.3", hub_subnet: "172.30.79.0/25", hub_ip: "172.30.79.10",
    electrum_subnet: "172.30.79.128/25"}' > "$answers"
  # A hub answers file whose hub_private_addr is not the public host's: refused, exit 1, nothing written.
  jq '.hub_private_addr = "127.0.0.9"' "$answers" > "$deploy_tmp/hub-answers-mismatch.json"
  local rc=0
  deploy/setup.sh "$hub_root" hub init -handoff "$handoff" -digest "$digest2" -answers "$deploy_tmp/hub-answers-mismatch.json" \
    > /dev/null 2> "$deploy_tmp/mismatch.err" || rc=$?
  [ "$rc" -eq 1 ] || { echo "deploy/hub: a different hub_private_addr: exit $rc, want 1" >&2; return 1; }
  grep -qF 'setup: the two hosts were given different values: hub_private_addr' "$deploy_tmp/mismatch.err" \
    || { echo "deploy/hub: the refusal is not the mismatch refusal naming hub_private_addr" >&2; return 1; }
  [ -z "$(ls -A "$hub_root")" ] || { echo "deploy/hub: the refused stage 2 left files behind" >&2; return 1; }
  log "deploy/hub: a different hub_private_addr is refused with exit 1 and nothing is written"

  log "setup: hub init"
  deploy/setup.sh "$hub_root" hub init -handoff "$handoff" -digest "$digest2" -answers "$answers" > /dev/null
  [ "$(mode_of "$hub_root/compose.env")" = 600 ] || { echo "deploy/hub: compose.env is not mode 0600" >&2; return 1; }
  grep -qx 'HUB_ADMIN_ADDR=127.0.0.3' "$hub_root/compose.env" || { echo "deploy/hub: compose.env does not carry the answered HUB_ADMIN_ADDR" >&2; return 1; }
  [ ! -e "$hub_root/config/relay.json" ] || { echo "deploy/hub: relay.json exists before stage 3" >&2; return 1; }

  # This host publishes on 127.0.0.1 only: a port already taken is not killed or shared; this run's ports are remapped
  # by a test-only file.
  local p taken=0 hub_admin_port=8080 hub_relay_port=7448 # (the two are only logged when remapped)
  hub_rpc_port=8332
  for p in 7448 8332 8080; do
    port_free "$p" || { log "deploy/hub: 127.0.0.1:$p is taken on this host"; taken=1; }
  done
  if [ "$taken" -eq 1 ]; then
    local ports
    ports=$(python3 -c 'import socket
for _ in range(3):
    s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
    set -- $ports
    export TEST_HUB_ADMIN_PORT=$1 TEST_HUB_RELAY_PORT=$2 TEST_HUB_RPC_PORT=$3
    hub_admin_port=$1 hub_relay_port=$2 hub_rpc_port=$3
    hub_override=dev/regtest/deploy-hub-ports.override.yml
    log "deploy/hub: published ports remapped to 127.0.0.1:$hub_admin_port (UI), :$hub_relay_port (relay), :$hub_rpc_port (RPC)"
  fi
  hdc config -q
  # The ports file is read on every run, so that it cannot rot; it is used for `up` only when a port is taken.
  TEST_HUB_ADMIN_PORT=1 TEST_HUB_RELAY_PORT=2 TEST_HUB_RPC_PORT=3 HUB_ADMIN_ADDR=127.0.0.1 docker compose -p "$hub_project" \
    -f deploy/hub/compose.yml -f deploy/hub/compose.build.yml -f dev/regtest/deploy-hub-ports.override.yml \
    --env-file "$hub_root/compose.env" config -q
  log "up: deploy/hub (volume-init, netns, bitcoind, hub)"
  hdc up -d --build hub
  local vi
  vi=$(hdc ps -a -q volume-init)
  want_code "volume-init exit code" "$(docker inspect -f '{{.State.ExitCode}}' "$vi")" 0
  local vi_finished
  vi_finished=$(docker inspect -f '{{.State.FinishedAt}}' "$vi")
  if [ "$(uname -s)" != Darwin ] && [ -z "$hub_override" ]; then
    [ "$(docker port "$(hdc ps -q netns)" 8080/tcp | head -1)" = 127.0.0.3:8080 ] \
      || { echo "deploy/hub: the admin UI is not published on the answered HUB_ADMIN_ADDR" >&2; return 1; }
    log "deploy/hub: the admin UI is published on 127.0.0.3:8080"
  fi
  local bc
  bc=$(hdc ps -q bitcoind)
  [ -n "$bc" ] && [ "$(docker inspect -f '{{.State.Running}}' "$bc")" = true ] || { echo "deploy/hub: bitcoind is not running" >&2; return 1; }
  [ "$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/etc/bitcoin/bitcoin.conf"}}{{.Source}}{{end}}{{end}}' "$bc")" = "$hub_root/config/bitcoin.conf" ] \
    || { echo "deploy/hub: bitcoind does not mount the generated bitcoin.conf" >&2; return 1; }
  log "deploy/hub: bitcoind runs with the generated bitcoin.conf"
  hub_runs "after its first start"

  # bitcoind's RPC over the published port. The hub's password is read by a root helper from config/hub.env, SatBooth's from
  # the public directory's bitcoind.pass: each goes to curl through a pipe, and only the HTTP codes are printed.
  local code deadline=$((SECONDS + 90))
  until code=$(hub_login | rpc_post getblockchaininfo) && [ "$code" = 200 ]; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "deploy/hub: the hub user's getblockchaininfo: HTTP $code after 90 s" >&2; return 1; }
    sleep 2
  done
  want_code "hub: getblockchaininfo, HTTP" "$code" 200
  [ "$(jq -r .result.chain "$deploy_tmp/rpc.body")" = regtest ] || { echo "deploy/hub: bitcoind's chain is not regtest" >&2; return 1; }
  want_code "hub: getblockcount, HTTP" "$(hub_login | rpc_post getblockcount)" 403
  local sbpass
  sbpass=$(< "$hub_pub_root/etc/bitcoind.pass")
  want_code "satbooth: getblockchaininfo, HTTP" "$(sb_login "$sbpass" | rpc_post getblockchaininfo)" 200
  want_code "satbooth: getblockcount, HTTP" "$(sb_login "$sbpass" | rpc_post getblockcount)" 403
  want_code "satbooth with a wrong password: getblockchaininfo, HTTP" \
    "$(sb_login 0000000000000000000000000000000000000000000000000000000000000000 | rpc_post getblockchaininfo)" 401
  sbpass=""

  # Stage 3 with two made-up Wallet Public Keys (valid, distinct, and not the client keys), then the relay.
  log "setup: hub pair"
  deploy/setup.sh "$hub_root" hub pair \
    -invoice-wallet 79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 \
    -payout-wallet c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5 > /dev/null
  hdc up -d --build relay
  sleep 8
  local rc_id
  rc_id=$(hdc ps -a -q relay)
  [ "$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$rc_id")" = 'true 0' ] \
    || { echo "deploy/hub: the relay is not running steadily after its start" >&2; return 1; }
  log "deploy/hub: the relay runs (its configuration is accepted), restart count 0"
  vi=$(hdc ps -a -q volume-init)
  want_code "volume-init exit code (second start)" "$(docker inspect -f '{{.State.ExitCode}}' "$vi")" 0
  [ "$(docker inspect -f '{{.State.FinishedAt}}' "$vi")" != "$vi_finished" ] \
    || { echo "deploy/hub: volume-init did not run again for the relay's start" >&2; return 1; }
  log "deploy/hub: volume-init ran again (FinishedAt moved)"
  local owner
  owner=$(dc run --rm --no-deps -T --cap-add DAC_OVERRIDE -v "$hub_root/config:/c:ro" --entrypoint sh volume-init -c \
    'stat -c %u:%g /c/relay.json' 2> /dev/null)
  [ "$owner" = 65532:65532 ] || { echo "deploy/hub: relay.json is owned by '$owner', want 65532:65532" >&2; return 1; }
  log "deploy/hub: relay.json is owned by 65532"
  hub_runs "after the relay's start"
  # Exactly these containers exist: bitcoind, the hub, the namespace holder and the relay run; volume-init exited 0; no
  # Fulcrum and nothing of the Monero profile was created.
  local running exited
  running=$(hdc ps --status running --format '{{.Service}}' | LC_ALL=C sort | tr '\n' ' ')
  exited=$(hdc ps -a --status exited --format '{{.Service}} {{.ExitCode}}' | LC_ALL=C sort | tr '\n' ',')
  [ "$running" = 'bitcoind hub netns relay ' ] && [ "$exited" = 'volume-init 0,' ] \
    || { echo "deploy/hub: running: '$running', exited: '$exited'" >&2; return 1; }
  [ "$(hdc ps -a -q | wc -l | tr -d ' ')" -eq 5 ] || { echo "deploy/hub: other containers were created" >&2; return 1; }
  log "deploy/hub: running: ${running}; exited: volume-init 0; nothing else created (Fulcrum and the Monero profile are not started)"
  hub_down
}

# deploy/README.md "Restore", steps 2 and 3, word for word: as 65532, without a network, in the bundle's volume-init
# image (the pinned builder: sh, cp, sha256sum), check both files of backup $1 against its manifest, remove the live
# databases with their -wal and -shm files, copy both files, and check the copies.
deploy_restore() {
  ddc run --rm --no-deps -T --user 65532:65532 --entrypoint sh volume-init -c '
  set -eu
  b=/backups/$1
  check() { # <file>: the manifest entry named after the file holds its SHA-256
    h=$(sha256sum "$1" | cut -d" " -f1)
    grep -q "{\"name\":\"$(basename "$1")\",\"bytes\":[0-9]*,\"sha256\":\"$h\"}" "$b/manifest.json"
  }
  check "$b/store.db"
  check "$b/ledger.db"
  rm -f /data/store.db* /data/ledger.db*
  cp -p "$b/store.db" "$b/ledger.db" /data/
  check /data/store.db
  check /data/ledger.db
  ls -ln /data' sh "$1"
}

# Waits up to 3 minutes for every hub to answer GET /api/info with running: true (service/start.go).
wait_running() {
  log "waiting for the hubs to run"
  local ip deadline
  for ip in "${hub_ips[@]}"; do
    deadline=$((SECONDS + 180))
    until dc run --rm --no-deps -T --entrypoint sh qualify -c \
      "curl -fsS --max-time 5 http://$ip:8080/api/info | grep -Eq '\"running\": *true'" > /dev/null 2>&1; do
      [ "$SECONDS" -lt "$deadline" ] || { echo "hub $ip is not running after 3 minutes" >&2; return 1; }
      sleep 3
    done
  done
}

# The relay joins its hub container's network namespace, and a restarted hub gets a new one: recreate it.
recreate_relay() {
  log "recreating relay-${1#hub-}"
  dc up -d --no-deps --force-recreate "relay-${1#hub-}"
}

container_id() { dc ps -a -q "$1"; }

# Graceful stop (60 s), then start. Records whether the hub exited by itself or was killed.
restart_hub() {
  local hub=$1 cid started code
  cid=$(container_id "$hub")
  started=$SECONDS
  dc stop -t 60 "$hub" > /dev/null
  code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
  log "$hub stopped after $((SECONDS - started)) s with exit code $code (137 = killed after the 60 s)"
  dc start "$hub" > /dev/null
  recreate_relay "$hub"
}

kill_hub() {
  local hub=$1 cid code
  cid=$(container_id "$hub")
  log "killing $hub (SIGKILL)"
  dc kill -s KILL "$hub" > /dev/null
  # kill returns once the signal is sent; the exit code is set when the container has stopped.
  until [ "$(docker inspect -f '{{.State.Running}}' "$cid")" = false ]; do sleep 0.2; done
  code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
  log "$hub killed with exit code $code (137 expected)"
  [ "$code" -eq 137 ] || { echo "$hub exit code $code after SIGKILL, expected 137" >&2; return 1; }
  dc start "$hub" > /dev/null
  recreate_relay "$hub"
}

# The release image (plan 10): SatBooth as the shipped satbooth image, driven over HTTP only by the TestImage phases. It
# runs last and alone: it uses the same invoice and payout connections and the same wallets as the in-process phases.
# Every stop, start and one-off run is done here, between the phases (the qualify container has no Docker socket):
#   satbooth         the service (172.30.78.40), read-only root, no capabilities, memlock -1, user 65532;
#   satbooth-oneoff  the same image and hardening without a network: a second process on the lock, satbooth backup;
#   volume-init      the pinned builder image: the volumes' ownership, and (as 65532, no network) a restore.
# The checks below read satbooth's own log lines (fixed messages without values) and its exit codes.
image_config=/state/image/satbooth-image.json
sb_started=0 # "satbooth started" lines expected so far (one per completed start)
sb_stopped=0 # "satbooth stopped" lines expected so far (one per clean stop)
sb_locked=0  # "memory locked" lines expected so far (one per start of the image, refused ones included)
sb_failed=0  # "startup failed" lines expected so far

sb_lines() {
  local n
  n=$(docker logs "$(container_id satbooth)" 2>&1 | grep -cF -- "$1" || true)
  printf '%s\n' "$n"
}

# Every count of satbooth's log lines matches what the script expects, and none says ALERT, a worker ended or a database
# did not close cleanly.
sb_check() {
  local what want got
  for what in "satbooth started:$sb_started" "satbooth stopped:$sb_stopped" "memory locked; core dumps disabled:$sb_locked" \
    "startup failed:$sb_failed" "ALERT:0" "ended unexpectedly:0" "did not close cleanly:0"; do
    want=${what##*:}
    got=$(sb_lines "${what%:*}")
    [ "$got" -eq "$want" ] || { echo "satbooth log: $got lines with '${what%:*}', want $want" >&2; return 1; }
  done
  log "satbooth log as expected: $sb_started started, $sb_stopped stopped, $sb_locked memory locked, $sb_failed startup failed, no ALERT"
}

# A start that must succeed: the next phase waits until the container serves.
sb_start() {
  log "starting satbooth"
  dc start satbooth > /dev/null
  sb_started=$((sb_started + 1))
  sb_locked=$((sb_locked + 1))
}

# A graceful stop (SIGTERM, 60 s before the kill, as the bundles' stop_grace_period): it must exit 0 by itself after
# closing both databases.
sb_stop() {
  local cid began code
  cid=$(container_id satbooth)
  # It must be running: a container that had already exited 0 by itself would otherwise pass as a graceful stop.
  [ "$(docker inspect -f '{{.State.Running}}' "$cid")" = true ] || { echo "satbooth is not running before its stop" >&2; return 1; }
  began=$SECONDS
  dc stop -t 60 satbooth > /dev/null
  code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
  log "satbooth stopped after $((SECONDS - began)) s with exit code $code (137 = killed after the 60 s)"
  [ "$code" -eq 0 ] || { echo "satbooth exit code $code on a graceful stop, expected 0" >&2; return 1; }
  sb_stopped=$((sb_stopped + 1))
  sb_check
}


# satbooth backup in a one-off container of the image (the data volume read-write for the lock, /backups for -to).
sb_backup() {
  local rc=0 out
  log "satbooth backup to /backups/$1"
  out=$(dc run --rm --no-deps -T satbooth-oneoff backup -config "$image_config" -to "/backups/$1" 2>&1) || rc=$?
  printf '%s\n' "$out" | sed 's/^/  backup: /'
  [ "$rc" -eq 0 ] || { echo "satbooth backup exited $rc, expected 0" >&2; return 1; }
  grep -qF '"msg":"backup written"' <<< "$out" || { echo "satbooth backup wrote no success line" >&2; return 1; }
  oneoff_clean "satbooth backup" "$out"
}

# The restore of docs/backup-and-restore.md: remove the data directory's store.db* and ledger.db* (their -wal and -shm
# files with them), then copy the store from backup $1 and the ledger from backup $2 (one backup for a real restore). Each
# file is checked against the entry of its backup's manifest that names it (so its hash cannot match the other file's
# entry) before the copy, and the live files again after it, so every round proves that the data directory holds the
# backup's bytes. The helper runs as 65532 without a network, so the copies stay the image user's files.
sb_restore() {
  log "restore: store.db from /backups/$1, ledger.db from /backups/$2"
  dc run --rm --no-deps -T --user 65532:65532 --entrypoint sh volume-init -c '
    set -eu
    check() { # <file> <manifest>: the manifest entry named after the file holds its SHA-256
      test -f "$2"
      h=$(sha256sum "$1" | cut -d" " -f1)
      n=$(basename "$1")
      grep -q "{\"name\":\"$n\",\"bytes\":[0-9]*,\"sha256\":\"$h\"}" "$2"
    }
    check "/backups/$1/store.db" "/backups/$1/manifest.json"
    check "/backups/$2/ledger.db" "/backups/$2/manifest.json"
    rm -f /data/store.db* /data/ledger.db*
    cp -p "/backups/$1/store.db" /data/store.db
    cp -p "/backups/$2/ledger.db" /data/ledger.db
    check /data/store.db "/backups/$1/manifest.json"
    check /data/ledger.db "/backups/$2/manifest.json"
    ls -ln /data' sh "$1" "$2"
}

# The image group: the TestImage phases and the stops, starts and one-off runs between them. It needs TestBootstrap,
# TestOnchainSetup and TestMoneroSetup (TestImagePrepare checks), and nothing else of the earlier phases.
image_group() {
  wait_running
  phase TestImagePrepare
  log "up: satbooth (volume-init first)"
  dc up -d satbooth
  sb_started=1
  sb_locked=1
  phase TestImageStart
  sb_check
  phase TestImageLightningPay
  phase TestImageOnchainPay
  phase TestImageMoneroPay
  phase TestImageRefund
  sb_check

  log "a second satbooth container on the same data volume"
  rc=0
  out=$(dc run --rm --no-deps -T satbooth-oneoff -config "$image_config" 2>&1) || rc=$?
  printf '%s\n' "$out" | sed 's/^/  second: /'
  [ "$rc" -eq 1 ] || { echo "the second container exited $rc, expected 1" >&2; exit 1; }
  grep -qF '"msg":"startup failed"' <<< "$out" && grep -qF '"reason":"data_dir_lock"' <<< "$out" &&
    grep -qF 'another SatBooth process holds the data directory' <<< "$out" || {
    echo "the second container did not refuse for the lock (startup failed, data_dir_lock, the lock message)" >&2
    exit 1
  }
  oneoff_clean "the second container" "$out"
  phase TestImageSecondContainerRefused

  phase TestImageRestartBefore
  sb_stop
  sb_start
  phase TestImageRestartAfter
  sb_check

  # Backup and restore: one backup's pair comes back with the same orders.
  phase TestImageBackupBefore
  sb_stop
  sb_backup b1
  sb_restore b1 b1
  sb_start
  phase TestImageBackupAfter
  sb_check

  # A mixed pair (the store from b1, the ledger from b2, one start later) is refused: exit 1 and the older-file refusal.
  sb_stop
  sb_backup b2
  sb_restore b1 b2
  older='the store is older than the ledger: restore both databases from one backup'
  older_before=$(sb_lines "$older")
  log "starting satbooth on the mixed pair"
  cid=$(container_id satbooth)
  dc start satbooth > /dev/null
  sb_locked=$((sb_locked + 1))
  deadline=$((SECONDS + 90))
  until [ "$(docker inspect -f '{{.State.Running}}' "$cid")" = false ]; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "satbooth still runs on a mixed pair after 90 s" >&2; exit 1; }
    sleep 1
  done
  code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
  older_after=$(sb_lines "$older")
  log "satbooth on the mixed pair exited $code; refusal lines $older_before before, $older_after after"
  [ "$code" -eq 1 ] || { echo "satbooth exited $code on a mixed pair, expected 1" >&2; exit 1; }
  [ "$older_after" -eq $((older_before + 1)) ] || { echo "expected one older-file refusal line" >&2; exit 1; }
  sb_failed=$((sb_failed + 1))
  sb_check
  # The remedy: b2's own pair starts and serves the same orders.
  sb_restore b2 b2
  sb_start
  phase TestImageBackupAfter
  sb_check
  sb_stop
}

# Lightning refunds (plan 9): the same service with the payout block, the bootstrap's payout connection and an approval
# key generated in the test. All refund phases but TestRefundBudget share one environment (/state/lightning/refund). Of
# the earlier phases they need only TestBootstrap (state.json: the hubs, the connections' keys, the channel); every
# other value they read is in their own phase file (refund.json), written by the phases of this group.
refund_group() {
  phase TestRefundPay
  phase TestRefundPartials
  phase TestRefundOwnInvoice
  phase TestRefundHubKnown
  # A real SIGKILL of a child SatBooth process between the send marker and the request, then a restart.
  phase TestRefundCrashAfterMarker
  # The payout connection cut right before the request: relay-satbooth takes the proxy's URL, as for
  # TestLightningRelayCut.
  phase TestLightningRelayCutPrepare
  recreate_relay hub-satbooth
  phase TestRefundCut
  phase TestLightningRelayCutRestore
  recreate_relay hub-satbooth
  # A payout the hub reports failed: the claimant's node (the merchant's hub, SatBooth's only channel peer) is down.
  phase TestRefundFailedBefore
  log "stopping hub-merchant"
  dc stop -t 60 hub-merchant > /dev/null
  phase TestRefundFailed
  log "starting hub-merchant"
  dc start hub-merchant > /dev/null
  recreate_relay hub-merchant
  wait_running
  phase TestRefundFailedAfter
  # Two claims over the budget left: one expires unsent, the other is voided while claimed (plan 11).
  phase TestRefundBudget
  # Last: the crash and cut refunds are voided once their send marker is 11 minutes old (the phase waits out the
  # rest).
  phase TestRefundVoidUnknown
}

# The hubs' bitcoind user (plan 11): the hubs reach the regtest bitcoind as the user hub, whose rpcwhitelist holds
# the eleven calls of their chain source (dev/regtest/bitcoind/bitcoin.conf, docs/dependencies.md §8). Bitcoin Core
# 32.1 logs every call a whitelist refuses, at warning level and so whatever the debug categories, and answers it HTTP
# 403 (src/httprpc.cpp:117 at v32.1: LogWarning("RPC User %s not allowed to call method %s", ...); :109 is the same
# text with "any methods" for a user without a list when rpcwhitelistdefault is on). This prefix matches both, and a
# "[*] " that Core puts first while it rate-limits other lines (its console output is never rate-limited).
hub_refused='RPC User hub not allowed to call '
hub_refused_lines() {
  local n
  n=$(dc logs --no-color bitcoind 2>&1 | grep -cF -- "$hub_refused" || true)
  printf '%s\n' "$n"
}

# The probe's request: one call outside the hub user's list, harmless.
hub_probe_body='{"jsonrpc":"1.0","id":"probe","method":"getblockcount","params":[]}'

# The end checks, in every mode, after the last phase. First, each hub container ran with LDK_BITCOIND_RPC_USER=hub
# (its environment as the engine applied it; matched without printing it), so the count that follows is about the
# confined user. Then bitcoind's log holds no refused call for the user hub: nothing the hubs did in this run was
# outside the list. Then the matcher is proven: one call outside the list as hub through the qualify container must be
# refused with 403 and add exactly one line. The credentials reach curl on its standard input (never in argv). The
# probe runs after the count, so its own line is never counted as the hubs'. The public bundle (deploy_group) runs no
# bitcoind of its own: this is the only one in every mode.
end_checks() {
  local hub hub_env before code after deadline
  for hub in "${hubs[@]}"; do
    # Captured first, then matched: grep -q on a pipe may exit before docker has written everything, and pipefail
    # would then read docker's SIGPIPE (141) as a failure. The value holds the hub's environment, its RPC password
    # included: it is matched only, never printed.
    hub_env=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(container_id "$hub")") \
      || { echo "$hub: docker inspect failed" >&2; return 1; }
    grep -qx 'LDK_BITCOIND_RPC_USER=hub' <<< "$hub_env" \
      || { echo "$hub does not run with LDK_BITCOIND_RPC_USER=hub" >&2; return 1; }
  done
  hub_env=""
  log "bitcoind: ${hubs[*]} run as the RPC user hub"
  before=$(hub_refused_lines)
  log "bitcoind: $before refused calls for the user hub over the run"
  [ "$before" -eq 0 ] || {
    dc logs --no-color bitcoind 2>&1 | grep -F -- "$hub_refused" | head -n 20 >&2 || true
    echo "bitcoind refused $before calls of the hubs' RPC user (above): the whitelist misses a method the hub uses" >&2
    return 1
  }
  code=$(dc run --rm --no-deps -T --entrypoint sh qualify -c \
    'printf "user = \"%s:%s\"\n" "$HUB_RPC_USER" "$HUB_RPC_PASSWORD" \
      | curl -K - -sS -o /dev/null -w "%{http_code}" --max-time 30 --data-binary "$1" http://172.30.78.2:18443/' \
    sh "$hub_probe_body" 2> /dev/null || true)
  deadline=$((SECONDS + 15))
  after=$(hub_refused_lines)
  until [ "$after" -ne "$before" ] || [ "$SECONDS" -ge "$deadline" ]; do
    sleep 1
    after=$(hub_refused_lines)
  done
  log "bitcoind: getblockcount as hub answered HTTP $code; refused-call lines $before before, $after after"
  [ "$code" = 403 ] || { echo "getblockcount as hub answered HTTP '$code', want 403" >&2; return 1; }
  [ "$after" -eq $((before + 1)) ] \
    || { echo "the refused probe added $((after - before)) log lines, want 1" >&2; return 1; }
}

log "start from a clean project"
ddc down -v --remove-orphans > /dev/null 2>&1 || true
docker compose -p "$hub_project" down -v --remove-orphans > /dev/null 2>&1 || true
dc down -v --remove-orphans > /dev/null 2>&1 || true

log "build"
# relay-satbooth and satbooth are the release images (deploy/image/Dockerfile); relay-merchant and satbooth-oneoff use them.
dc build bitcoind relay-satbooth monerod qualify satbooth
# The public bundle builds the same Dockerfile target under its own name (satbooth:local); the layers are cached.
ddc build satbooth

# The qualify binary runs the in-process phases: it is built with the release image's flags.
phase TestBuildFlags

log "up: bitcoind and hubs"
dc up -d bitcoind "${hubs[@]}"
# Monero (plan 8): wallet-satbooth waits for the view-only wallet's description, which TestMoneroSetup writes.
log "up: monerod and the two wallet-rpc services"
dc up -d monerod wallet-customer wallet-satbooth

phase TestBootstrap

# The hub logs this line when a request's author has no app (getAlby/hub 921003b nip47/event_handler.go:73-82, the
# text is the logger message there). TestQualify/UnknownKey sends exactly one such request, so the count of this line
# in hub-satbooth's logs must rise by exactly one over the TestQualify phase: that proves the hub received the
# stranger's request and dropped it for its unknown author, not for some other reason.
unknown_app_lines() {
  local n
  n=$(dc logs --no-color hub-satbooth 2>&1 | grep -c 'Failed to find app for nostr pubkey' || true)
  printf '%s\n' "$n"
}

log "up: relays, then restart both hubs"
dc up -d --no-deps relay-satbooth relay-merchant
for hub in "${hubs[@]}"; do
  restart_hub "$hub"
done
wait_running

# On-chain, end to end (plan 6): the same service, now with the on-chain rail on the regtest bitcoind through the
# whitelisted satbooth user. All on-chain phases share one environment (/state/onchain, one data directory).

# Shell snippet run inside the qualify container: curl with the credentials from the container's environment, given to
# curl on its standard input as a config line (printf is a builtin), so they are never in any argv. The JSON-RPC body is
# the script's first argument.
rpc_sh='crl() { printf "user = \"%s:%s\"\n" "$BITCOIND_RPC_USER" "$BITCOIND_RPC_PASSWORD" | curl -K - "$@"; }; '

# One JSON-RPC call to bitcoind as the full user through the qualify container. $1 method, $2 params (JSON), $3 an optional
# wallet path suffix; prints the whole answer.
btc_rpc() {
  local body
  body=$(printf '{"jsonrpc":"1.0","id":"k","method":"%s","params":%s}' "$1" "$2")
  dc run --rm --no-deps -T --entrypoint sh qualify -c "$rpc_sh"'crl -fsS --max-time 120 --data-binary "$1" "http://172.30.78.2:18443/$2"' sh "$body" "${3-}" 2> /dev/null
}

# Polls getblockchaininfo for up to 3 minutes, then loads the miner wallet again: plan 4's bootstrap created it without
# load_on_startup.
wait_bitcoind() {
  log "waiting for bitcoind"
  local deadline=$((SECONDS + 180))
  until btc_rpc getblockchaininfo '[]' | grep -q '"chain"'; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "bitcoind does not answer after 3 minutes" >&2; return 1; }
    sleep 2
  done
  # Core answers -35 when the wallet is already loaded; that is fine. (curl without -f: the error comes with HTTP 500.)
  dc run --rm --no-deps -T --entrypoint sh qualify -c "$rpc_sh"'crl -sS --max-time 30 --data-binary "$1" http://172.30.78.2:18443/' sh \
    '{"jsonrpc":"1.0","id":"w","method":"loadwallet","params":{"filename":"miner"}}' 2> /dev/null \
    | grep -Eq '"error":null|"code":-35' || { echo "loadwallet miner failed" >&2; return 1; }
}

# SIGKILL of bitcoind, then start and wait. Observed: a SIGKILL right after blocks were
# mined dropped them (the block index and chainstate are flushed on a timer), so the chain shrank under the service
# and the ledger. The kill experiment is about the wallet's index, not about bitcoind's chain flush, so the chain is
# flushed first and its height must not fall. The flush: gettxoutsetinfo calls ForceFlushStateToDisk
# (src/rpc/blockchain.cpp at v32.1, line 1114 in gettxoutsetinfo).
kill_bitcoind() {
  local h0 h1
  btc_rpc gettxoutsetinfo '{"hash_type":"none"}' > /dev/null || { echo "gettxoutsetinfo failed" >&2; return 1; }
  h0=$(btc_rpc getblockcount '[]' | grep -oE '"result":[0-9]+' | cut -d: -f2)
  log "killing bitcoind (SIGKILL) at height $h0"
  dc kill -s KILL bitcoind > /dev/null
  dc start bitcoind > /dev/null
  wait_bitcoind
  h1=$(btc_rpc getblockcount '[]' | grep -oE '"result":[0-9]+' | cut -d: -f2)
  log "bitcoind back at height $h1"
  [ -n "$h0" ] && [ "${h1:-0}" -ge "$h0" ] || { echo "the chain shrank over the kill ($h0 to ${h1:-?})" >&2; return 1; }
}

# The on-chain phases (plan 6) in the order of the full run, with the bitcoind kills between them: from TestOnchainSetup to the
# stuck pair, the last phase that uses an on-chain environment. They need only TestBootstrap (the hubs, wait_running) of the
# earlier phases: no Lightning phase leaves anything they read, as the image group, which runs TestOnchainSetup straight
# after the setup, shows.
onchain_group() {
  local i
  phase TestOnchainSetup
  phase TestOnchainPay
  # Plan 7: a priced order on both rails (the rates block added for this phase), paid on-chain.
  phase TestOnchainFiat
  phase TestOnchainRBF
  # Plan 6.1: the 0-confirmation tier (paid from the mempool, then replaced or bumped), and an RBF bump after the expiry
  # that inherits the original's first-seen (about 5 minutes of waiting for the expiry).
  phase TestOnchainZeroConf
  # The same bump and divert with the wallet held behind its node (a lagging wrapper around the real client).
  phase TestOnchainWalletBehind
  phase TestOnchainBumpAfterExpiry
  phase TestOnchainReorg
  phase TestOnchainPartial
  # The two 300 s orders (Partial's short one, LateBefore's) are read by LateAfter at the end.
  phase TestOnchainLateBefore
  phase TestOnchainRestartBefore
  phase TestOnchainRestartAfter
  phase TestOnchainIndexBefore
  for i in 1 2 3; do
    log "bitcoind kill $i of 3"
    kill_bitcoind
  done
  # The hubs' chain source went away three times; LDK reconnects by itself.
  wait_running
  phase TestOnchainIndexAfter
  phase TestOnchainLateAfter
  phase TestOnchainWhitelist
  # Plan 6.1: a payment stuck in the mempool past its watch window, in its own environment (/state/onchain-stuck) whose
  # After phase runs the service 7 days and 10 minutes ahead. Last: the stuck environment issues addresses from the shared
  # watch wallet above the shared environment's recorded index, so the onchain environment must not start after it.
  phase TestOnchainStuckBefore
  phase TestOnchainStuckAfter
}

# The hub suite: TestQualify, and the check that the stranger of its UnknownKey request reached the hub and was dropped
# for its unknown author (one line more than before, see unknown_app_lines).
hub_group() {
  unknown_before=$(unknown_app_lines)
  phase TestQualify
  unknown_after=$(unknown_app_lines)
  log "hub-satbooth 'Failed to find app for nostr pubkey' lines: $unknown_before before TestQualify, $unknown_after after"
  [ "$unknown_after" -eq $((unknown_before + 1)) ] || {
    echo "expected exactly one unknown-app line from UnknownKey (before $unknown_before, after $unknown_after)" >&2
    exit 1
  }
}

# --only: the named group on top of the setup, then the end checks.
if [ -n "$only" ]; then
  case "$only" in
    deploy) deploy_group ;;
    image)
      # The image group's prerequisites (TestBootstrap ran in the setup), the group, and TestImageBudget, whose figure is
      # the payout budget read by TestImagePrepare plus the image's refund and fee: it holds on a fresh stack.
      phase TestOnchainSetup
      phase TestMoneroSetup
      image_group
      phase TestImageBudget
      ;;
    refund) refund_group ;;
    hub) hub_group ;;
    onchain) onchain_group ;;
  esac
  end_checks
  finished=1
  log "setup and group $only passed (a targeted run, not the qualification)"
  exit 0
fi

hub_group

# The hub logs this line (Warn) when it receives a request event whose id it has already stored
# (getAlby/hub 921003b nip47/event_handler.go:57-66). The After phases of the crash pairs replay one get_info
# request, saved before the stop or kill, into the restarted hub: the count must rise by exactly one, which proves the
# hub received the replay and refused it by its own dedup.
dup_event_lines() {
  local n
  n=$(dc logs --no-color hub-satbooth 2>&1 | grep -c 'Event already processed' || true)
  printf '%s\n' "$n"
}

# Runs an After phase and requires the replayed request to have reached the hub exactly once.
phase_after() {
  local before after
  before=$(dup_event_lines)
  phase "$1"
  after=$(dup_event_lines)
  log "hub-satbooth 'Event already processed' lines: $before before $1, $after after"
  [ "$after" -eq $((before + 1)) ] || {
    echo "expected exactly one 'Event already processed' line from the replay in $1 (before $before, after $after)" >&2
    exit 1
  }
}

# What the hub's log says about duplicate payments and shutdown, for qualify/REPORT.md (message texts are LDK's and the
# hub's; the lines hold no secrets).
hub_log_notes() {
  log "hub-satbooth log lines about duplicates, shutdown and payments ($1)"
  dc logs --no-color --timestamps hub-satbooth 2>&1 \
    | grep -iE 'duplicate|already|shutting down|shutdown|payment (failed|successful)|Failed to send payment|pay_invoice' \
    | tail -n 30 || true
}

# QUALIFY_CRASH_REPEATS=N runs the stop pair and the kill pair N times each (default 1): a payout that the hub marks
# FAILED during shutdown is a race with the process exit, and repeating gives it more chances to show. Each round
# spends about 2,100 sat of the payout connection's 50,000 sat budget, which the refund phases share (they pay about
# 5,100 sat, and TestImageRefund 1,000 more): stay under about 12 rounds in total (an estimate from the budget arithmetic).
repeats=${QUALIFY_CRASH_REPEATS:-1}
for ((i = 1; i <= repeats; i++)); do
  log "graceful stop, round $i of $repeats"
  phase TestCrashBefore
  restart_hub hub-satbooth
  wait_running
  phase_after TestCrashAfterStop
  hub_log_notes "after the graceful stop"
done
for ((i = 1; i <= repeats; i++)); do
  log "kill, round $i of $repeats"
  phase TestKillBefore
  kill_hub hub-satbooth
  wait_running
  phase_after TestKillAfter
  hub_log_notes "after the kill"
done

# Relay loss: the relay stays down while the customer pays (the hub API needs no relay), and comes back with an
# empty window.
phase TestRelayLossBefore
log "stopping relay-satbooth"
dc stop relay-satbooth > /dev/null
phase TestRelayLossDuring
log "starting relay-satbooth"
dc start relay-satbooth > /dev/null
phase TestRelayLossAfter

# Lightning, end to end: the whole SatBooth service runs in-process in the qualify container against relay-satbooth
# and the invoice connection; the merchant's hub plays the customer. Each phase ends with its safety check.
phase TestLightningPay
phase TestLightningReplay
# Plan 6.1: the per-account cap on open invoices (its own environment, the default cap of 5).
phase TestLightningAccountCap
# Plan 7: fiat pricing against the fake rate server in the qualify process (no live provider is called): price, pay,
# the webhook's pricing and a restart that keeps the rate; then an outage, a disagreement and stale rates.
phase TestFiatPricing
phase TestFiatOutage
# The expiry order is made now and read at the very end: the invoice lives 15 minutes, plus 2 minutes of grace.
phase TestLightningExpireBefore

phase TestLightningRelayLossBefore
log "stopping relay-satbooth"
dc stop relay-satbooth > /dev/null
phase TestLightningRelayLossDuring
log "starting relay-satbooth"
dc start relay-satbooth > /dev/null
phase TestLightningRelayLossAfter

# SatBooth down while the customer pays: the Before phase stops the service, the customer pays, the After phase is a
# new process with a new service on the same data.
phase TestLightningRestartBefore
phase TestLightningRestartAfter

# Relay loss seen by a running service: relay-satbooth takes the qualify container's proxy URL as its client URL (its AUTH
# check is byte for byte), the service runs through the proxy, which the test cuts and restores. Then the relay gets its
# own URL back.
phase TestLightningRelayCutPrepare
recreate_relay hub-satbooth
phase TestLightningRelayCut
phase TestLightningRelayCutRestore
recreate_relay hub-satbooth

phase TestLightningExpireAfter

onchain_group

# Monero, end to end (plan 8): the same service with the Monero rail on the regtest monerod and SatBooth's view-only
# wallet-rpc (wallet-satbooth, Digest login). All Monero phases share one environment (/state/monero) except the stuck
# pair (/state/monero-stuck, a clock 7 days ahead).

# Polls wallet-satbooth's get_version with the Digest login for up to 3 minutes. The login comes from the qualify
# container's environment and reaches curl on its standard input as a config line (never in any argv).
wait_wallet_rpc() {
  log "waiting for wallet-satbooth"
  local deadline=$((SECONDS + 180))
  until dc run --rm --no-deps -T --entrypoint sh qualify -c \
    'printf "user = \"%s:%s\"\n" "$SATBOOTH_XMR_RPC_USER" "$SATBOOTH_XMR_RPC_PASSWORD" | curl -K - --digest -fsS --max-time 10 -H "Content-Type: application/json" --data-binary "{\"jsonrpc\":\"2.0\",\"id\":\"0\",\"method\":\"get_version\"}" http://172.30.78.5:18083/json_rpc' \
    2> /dev/null | grep -q '"version"'; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "wallet-satbooth does not answer after 3 minutes" >&2; return 1; }
    sleep 2
  done
}

# Runs a shell command on wallet-satbooth's wallet directory (its volume) in a throwaway container without a network,
# whether wallet-satbooth runs or not.
wallet_files() {
  docker run --rm --network none -v "${project}_wallet-satbooth-data:/data" --entrypoint sh satbooth-regtest-monero:dev -c "$1"
}

# The forgotten-subaddress case: the wallet files are copied aside before KillBefore issues three subaddresses; then
# wallet-satbooth is killed (SIGKILL), the copy is restored over the files it stored since, and it starts again on the
# older wallet, which lacks the three. KillAfter's service start re-creates them.
kill_wallet_restore() {
  local cid code
  cid=$(container_id wallet-satbooth)
  log "killing wallet-satbooth (SIGKILL)"
  dc kill -s KILL wallet-satbooth > /dev/null
  until [ "$(docker inspect -f '{{.State.Running}}' "$cid")" = false ]; do sleep 0.2; done
  code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
  log "wallet-satbooth killed with exit code $code (137 expected)"
  [ "$code" -eq 137 ] || { echo "wallet-satbooth exit code $code after SIGKILL, expected 137" >&2; return 1; }
  log "restoring the wallet files copied before KillBefore"
  wallet_files 'cp /data/aside/satbooth-view* /data/ && ls -l /data'
  dc start wallet-satbooth > /dev/null
  wait_wallet_rpc
}

phase TestMoneroSetup
phase TestMoneroPay
phase TestMoneroPartial
phase TestMoneroPoolDrop
phase TestMoneroReorg
phase TestMoneroUnlockTime
phase TestMoneroRestartBefore
phase TestMoneroRestartAfter
# wallet-satbooth stops for the copy, so the files cannot be torn: a graceful stop stores the wallet and closes it
# (wallet_rpc_server::stop, src/wallet/wallet_rpc_server.cpp:310-323 at v0.18.5.4, called on the signal at :5011-5029).
log "stopping wallet-satbooth to copy its wallet files aside"
dc stop -t 60 wallet-satbooth > /dev/null
ws_code=$(docker inspect -f '{{.State.ExitCode}}' "$(container_id wallet-satbooth)")
log "wallet-satbooth stopped with exit code $ws_code (0: stored and exited by itself)"
[ "$ws_code" -eq 0 ] || { echo "wallet-satbooth exit code $ws_code on a graceful stop, expected 0" >&2; exit 1; }
wallet_files 'rm -rf /data/aside && mkdir /data/aside && cp /data/satbooth-view* /data/aside/ && ls -l /data/aside'
dc start wallet-satbooth > /dev/null
wait_wallet_rpc
phase TestMoneroKillBefore
kill_wallet_restore
phase TestMoneroKillAfter
# The two 300 s orders (Partial's short one, LateBefore's) are read by LateAfter, after the stuck pair.
phase TestMoneroLateBefore
phase TestMoneroStuckBefore
phase TestMoneroStuckAfter
phase TestMoneroLateAfter
phase TestMoneroFiat
phase TestMoneroBothRails

# Lightning refunds (plan 9): the refund group (above).
refund_group

# The release image (the functions above): last and alone, then the public bundle, then the budget check.
image_group
deploy_group

phase TestImageBudget

end_checks
finished=1
log "all phases passed"
