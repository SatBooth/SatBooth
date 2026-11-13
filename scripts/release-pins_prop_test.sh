#!/usr/bin/env bash
# Seeded property test of scripts/release-pins.sh. It builds the three pin sites and a digests.txt in a temporary
# directory, applies random mutations to them (line endings, separators, quoting, comments, a second mention of the
# needle, spaces, letter case, digest lengths, namespaces, missing and repeated lines, final newlines, versions), and
# requires `check-format`, `version` and `check` to accept a variant exactly when a short, separate reading of the
# format accepts it (the oracle below shares no code with the script). A failure prints the seed, the case and the
# mutations. The seed is fixed; RELEASE_PINS_SEED=<number> overrides it, RELEASE_PINS_CASES=<n> sets the case count.
# Usage: scripts/release-pins_prop_test.sh
# The model's variables are set and read through eval (bash 3 has no namerefs).
# shellcheck disable=SC2034,SC2154
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/.."
pins=$PWD/scripts/release-pins.sh

seed=${RELEASE_PINS_SEED:-20261113}
cases=${RELEASE_PINS_CASES:-80}
echo "release-pins_prop_test: seed $seed, $cases cases"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "release-pins_prop_test: FAIL: $*" >&2; exit 1; }

# A portable pseudo-random generator (a linear congruential one), so that a seed gives the same cases everywhere.
state=$seed
# roll <n> sets RND to a number in 0..n-1 (no subshell, which would lose the state).
RND=0
roll() {
  state=$(((state * 1103515245 + 12345) & 0x7fffffff))
  RND=$(((state >> 16) % $1))
}

hexdigits=0123456789abcdef
rand_hex() { # <count>: sets HEX
  local i out=''
  for ((i = 0; i < $1; i++)); do roll 16; out+=${hexdigits:RND:1}; done
  HEX=$out
}
upper_char() { # <lowercase hex letter>
  case $1 in a) echo A ;; b) echo B ;; c) echo C ;; d) echo D ;; e) echo E ;; f) echo F ;; *) echo "$1" ;; esac
}

ns=ghcr.io/satbooth
versions_ok=(v0.1.0 v0.1.0-rc.1 v1.2.3 v10.20.30 v1.2.3-rc.12)
versions_bad=(v1.2 v1.2.3-rc. v1.2.3-rc v1.2.3-beta.1 1.2.3 latest v1.2.3.4 V1.2.3 v1.2.3-rc.1.2)

# The model of a file: its lines, the index of its pin line (-1 for digests.txt), the end-of-line and a final newline.
# Files are f0 f1 f2 (the sites, in the order of names) and f3 (digests.txt). A line array is lines_<f>.
reset_model() {
  local i
  for i in 0 1 2 3; do eol[i]=$'\n'; final[i]=1; done
  rand_hex 64; dig0=$HEX
  rand_hex 64; dig1=$HEX
  rand_hex 64; dig2=$HEX
  roll ${#versions_ok[@]}; ver=${versions_ok[RND]}
  ref0=$ns/satbooth:$ver@sha256:$dig0
  ref1=$ns/satbooth-relay:$ver@sha256:$dig1
  ref2=$ns/satbooth-tools:$ver@sha256:$dig2
  lines0=('services:' '  satbooth:' "    image: $ref0" '    restart: always')
  lines1=('services:' '  relay:' "    image: $ref1" '    restart: always')
  lines2=('#!/usr/bin/env bash' 'x=1' "tools_image=$ref2" 'echo hi')
  lines3=("satbooth sha256:$dig0" "satbooth-relay sha256:$dig1" "satbooth-tools sha256:$dig2")
  pin0=2 pin1=2 pin2=2 pin3=-1
  log=
}

# get / set the line array of a file by index (bash 3 has no namerefs).
get_lines() { eval "cur=(\"\${lines$1[@]}\")"; }
put_lines() { eval "lines$1=(\"\${cur[@]}\")"; }
get_pin() { eval "pin=\$pin$1"; }
has_pin() { get_pin "$1"; [ "$pin" -ge 0 ]; }

# write_file <index> <path>: the model's bytes.
write_file() {
  local i=$1 n k out=''
  get_lines "$i"
  n=${#cur[@]}
  for ((k = 0; k < n; k++)); do
    out+=${cur[k]}
    if [ "$k" -lt $((n - 1)) ] || [ "${final[i]}" = 1 ]; then out+=${eol[i]}; fi
  done
  # A NUL is written as \001 in the model; the oracle learns from nul<i> whether the file holds one.
  if [[ $out == *$'\001'* ]]; then
    printf '%s' "$out" | tr '\001' '\000' > "$2"
    eval "nul$i=1"
  else
    printf '%s' "$out" > "$2"
    eval "nul$i=0"
  fi
}

# Mutations. Each takes the file index; it changes the model and appends to the log. A mutation that does not apply
# to the file does nothing.
mut_crlf() { eol[$1]=$'\r\n'; }
mut_no_final_newline() { final[$1]=0; }
mut_trailing_blank() { get_lines "$1"; cur+=(''); put_lines "$1"; }
mut_tab_separator() {
  get_lines "$1"; get_pin "$1"
  if [ "$pin" -ge 0 ]; then cur[pin]=${cur[pin]/image: /image:$'\t'}; cur[pin]=${cur[pin]/tools_image=/tools_image=$'\t'}
  else cur[0]=${cur[0]/ /$'\t'}; fi
  put_lines "$1"
}
mut_extra_spaces() {
  get_lines "$1"; get_pin "$1"
  roll 4
  if [ "$pin" -ge 0 ]; then
    case $RND in
      0) cur[pin]="   ${cur[pin]}" ;;
      1) cur[pin]=${cur[pin]/image: /image:   } ;;
      2) cur[pin]="${cur[pin]}   " ;;
      3) cur[pin]=${cur[pin]/tools_image=/tools_image= } ;;
    esac
  else
    case $RND in
      0) cur[0]=" ${cur[0]}" ;;
      1) cur[0]=${cur[0]/ /  } ;;
      2) cur[0]="${cur[0]} " ;;
      3) cur[0]=${cur[0]/ / } ;;
    esac
  fi
  put_lines "$1"
}
mut_quote() {
  get_lines "$1"; get_pin "$1"
  [ "$pin" -ge 0 ] || return 0
  local l=${cur[pin]} head ref q
  roll 2; if [ "$RND" = 0 ]; then q=\"; else q=\'; fi
  head=${l%%"$ns"*}; ref=${l#"$head"}
  cur[pin]=$head$q$ref$q
  put_lines "$1"
}
mut_comment() {
  get_lines "$1"; get_pin "$1"
  if [ "$pin" -ge 0 ]; then cur[pin]="${cur[pin]} # pinned"; else cur[0]="${cur[0]} # pinned"; fi
  put_lines "$1"
}
mut_needle_elsewhere() {
  get_lines "$1"; get_pin "$1"
  [ "$pin" -ge 0 ] || return 0
  roll 4
  case $RND in
    0) cur+=("# was $ns/satbooth:v0.0.1@sha256:$dig0") ;;
    1) cur+=("    labels: $ns/x") ;;
    2) cur+=("  tools_image=other") ;;
    3) cur+=("echo \"tools_image=\$x\"") ;;
  esac
  put_lines "$1"
}
mut_uppercase() {
  get_lines "$1"
  local l k c out='' i
  if has_pin "$1"; then i=$pin; else roll ${#cur[@]}; i=$RND; fi
  l=${cur[i]}
  # Uppercase the letters after the last "sha256:", or the whole name in a digests line.
  local tail=${l##*sha256:} pre up=''
  pre=${l%"$tail"}
  for ((k = 0; k < ${#tail}; k++)); do c=${tail:k:1}; roll 2; if [ "$RND" = 0 ]; then c=$(upper_char "$c"); fi; up+=$c; done
  cur[i]=$pre$up
  put_lines "$1"
}
mut_digest_length() {
  get_lines "$1"
  local i l tail pre
  if has_pin "$1"; then i=$pin; else roll ${#cur[@]}; i=$RND; fi
  l=${cur[i]}; tail=${l##*sha256:}; pre=${l%"$tail"}
  roll 2
  if [ "$RND" = 0 ]; then tail=${tail%?}; else tail=${tail}a; fi
  cur[i]=$pre$tail
  put_lines "$1"
}
mut_namespace() {
  get_lines "$1"; get_pin "$1"
  [ "$pin" -ge 0 ] || return 0
  roll 4
  case $RND in
    0) cur[pin]=${cur[pin]/ghcr.io\/satbooth/ghcr.io\/other} ;;
    1) cur[pin]=${cur[pin]/ghcr.io\/satbooth/ghcrXio\/satbooth} ;;
    2) cur[pin]=${cur[pin]/ghcr.io\/satbooth/ghcr.io\/satbooth2} ;;
    3) cur[pin]=${cur[pin]/ghcr.io\/satbooth\//docker.io\/satbooth\/} ;;
  esac
  put_lines "$1"
}
mut_drop_line() {
  get_lines "$1"
  local i out=() k
  if has_pin "$1"; then i=$pin; else roll ${#cur[@]}; i=$RND; fi
  for ((k = 0; k < ${#cur[@]}; k++)); do [ "$k" = "$i" ] || out+=("${cur[k]}"); done
  cur=("${out[@]+"${out[@]}"}")
  put_lines "$1"
  if has_pin "$1"; then eval "pin$1=-1"; fi
}
mut_duplicate_line() {
  get_lines "$1"
  local i k out=()
  if has_pin "$1"; then i=$pin; else roll ${#cur[@]}; i=$RND; fi
  for ((k = 0; k < ${#cur[@]}; k++)); do out+=("${cur[k]}"); if [ "$k" = "$i" ]; then out+=("${cur[k]}"); fi; done
  cur=("${out[@]}")
  put_lines "$1"
}
mut_extra_name() {
  get_lines "$1"; ! has_pin "$1" || return 0
  rand_hex 64
  roll 3
  case $RND in
    0) cur+=("other sha256:$HEX") ;;
    1) cur+=("satbooth-tools sha256:$HEX") ;;
    2) cur+=("garbage") ;;
  esac
  put_lines "$1"
}
mut_rename() {
  get_lines "$1"; ! has_pin "$1" || return 0
  [ "${#cur[@]}" -ge 3 ] || return 0
  roll 3
  cur[RND]=${cur[RND]/satbooth/satbooth-x}
  put_lines "$1"
}
mut_reorder() {
  get_lines "$1"; ! has_pin "$1" || return 0
  [ "${#cur[@]}" -ge 3 ] || return 0
  cur=("${cur[2]}" "${cur[0]}" "${cur[1]}")
  put_lines "$1"
}
mut_other_digest() { # a valid digest that differs from the other file's
  get_lines "$1"
  local i l tail pre
  if has_pin "$1"; then i=$pin; else roll ${#cur[@]}; i=$RND; fi
  l=${cur[i]}; tail=${l##*sha256:}; pre=${l%"$tail"}
  rand_hex 64
  cur[i]=$pre$HEX
  put_lines "$1"
}
mut_version() {
  get_lines "$1"; get_pin "$1"
  [ "$pin" -ge 0 ] || return 0
  local v l
  roll 3
  if [ "$RND" = 0 ]; then roll ${#versions_bad[@]}; v=${versions_bad[RND]}; else roll ${#versions_ok[@]}; v=${versions_ok[RND]}; fi
  l=${cur[pin]}
  l=${l/:$ver@/:$v@}
  cur[pin]=$l
  put_lines "$1"
}

# Odd bytes, put at a random place of the pin line (of a random line of digests.txt, and sometimes of a random line of a
# site). A NUL is written as \001 and turned into a NUL when the file is written, as a bash string cannot hold it.
insert_bytes() { # <file index> <bytes>
  get_lines "$1"
  local i l at
  if has_pin "$1"; then
    i=$pin
    roll 4; if [ "$RND" = 0 ]; then roll ${#cur[@]}; i=$RND; fi
  else roll ${#cur[@]}; i=$RND; fi
  [ "${#cur[@]}" -gt 0 ] || return 0
  l=${cur[i]}
  roll $((${#l} + 1)); at=$RND
  cur[i]=${l:0:at}$2${l:at}
  put_lines "$1"
}
mut_nul() { insert_bytes "$1" $'\001'; }
mut_vt() { insert_bytes "$1" $'\v'; }
mut_ff() { insert_bytes "$1" $'\f'; }
mut_nbsp() { insert_bytes "$1" $'\xc2\xa0'; }
mut_extra_line() { # a line of no interest, or a stray blank
  get_lines "$1"
  roll 2
  if [ "$RND" = 0 ]; then cur+=('# note'); else cur=('# note' "${cur[@]}"); fi
  if has_pin "$1" && [ "${cur[0]}" = '# note' ]; then eval "pin$1=$((pin + 1))"; fi
  put_lines "$1"
}

site_mutations=(crlf no_final_newline trailing_blank tab_separator extra_spaces quote comment needle_elsewhere uppercase
  digest_length namespace drop_line duplicate_line other_digest version nul vt ff nbsp extra_line)
digests_mutations=(crlf no_final_newline trailing_blank tab_separator extra_spaces comment uppercase digest_length
  drop_line duplicate_line extra_name rename reorder other_digest nul vt ff nbsp extra_line)

# The oracle: a separate, short reading of the format, sharing no code with the script.
# no_nul <file>: the file holds no NUL byte, as write_file recorded it for the file of the case.
no_nul() {
  case $1 in
    */deploy/public/compose.yml) [ "$nul0" = 0 ] ;;
    */deploy/hub/compose.yml) [ "$nul1" = 0 ] ;;
    */deploy/setup.sh) [ "$nul2" = 0 ] ;;
    *) [ "$nul3" = 0 ] ;;
  esac
}

# oracle_find <file> <kind>: succeeds iff the file has no NUL and exactly one line holding the site's needle
# (`tools_image=` for the tools site, `ghcr.io/satbooth/` for a compose file). Sets HIT to that line.
oracle_find() {
  local file=$1 needle count=0 line
  HIT=
  if [ "$2" = tools ]; then needle='tools_image='; else needle='ghcr.io/satbooth/'; fi
  no_nul "$file" || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in *"$needle"*) count=$((count + 1)); HIT=$line ;; esac
  done < "$file"
  [ "$count" -eq 1 ]
}

# oracle_site <file> <kind> <name>: sets REF_VER and REF_DIG when the file's one needle line is `[indent]image: <ref>`
# (or `[indent]tools_image=<ref>`, no space after the =), the indent and the blanks around the key being spaces and tabs
# only, trailing spaces, tabs and CRs ignored, and <ref> exactly
# ghcr.io/satbooth/<name>:v<n>.<n>.<n>[-rc.<n>]@sha256:<64 lowercase hex>.
oracle_site() {
  local hit ref
  oracle_find "$1" "$2" || return 1
  hit=$HIT
  while [[ $hit == *[$' \t\r'] ]]; do hit=${hit%?}; done
  if [ "$2" = tools ]; then
    [[ $hit =~ ^[\ $'\t']*tools_image=(.*)$ ]] || return 1
  else
    [[ $hit =~ ^[\ $'\t']*image:[\ $'\t']*(.*)$ ]] || return 1
  fi
  ref=${BASH_REMATCH[1]}
  [[ $ref =~ ^ghcr\.io/satbooth/$3:(v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?)@sha256:([0-9a-f]{64})$ ]] || return 1
  REF_VER=${BASH_REMATCH[1]}
  REF_DIG=sha256:${BASH_REMATCH[3]}
}

# oracle_replaceable <file> <kind>: `set` can rewrite the file: one needle line, no NUL, and the white space that leads
# that line (which set keeps) holds only spaces and tabs.
oracle_replaceable() {
  local lead=''
  oracle_find "$1" "$2" || return 1
  local rest=$HIT
  while [[ $rest == [$' \t\r\v\f']* ]]; do lead+=${rest:0:1}; rest=${rest:1}; done
  [[ $lead != *[$'\r\v\f']* ]]
}

# oracle_digests <file>: sets DIG_satbooth, DIG_relay and DIG_tools when the file has no NUL and is exactly three lines
# "<name> sha256:<64 lowercase hex>", one per name, in any order.
oracle_digests() {
  local line count=0 name
  DIG_satbooth='' DIG_relay='' DIG_tools=''
  no_nul "$1" || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    [[ $line =~ ^(satbooth|satbooth-relay|satbooth-tools)\ (sha256:[0-9a-f]{64})$ ]] || return 1
    name=${BASH_REMATCH[1]}
    count=$((count + 1))
    case $name in
      satbooth) [ -z "$DIG_satbooth" ] || return 1; DIG_satbooth=${BASH_REMATCH[2]} ;;
      satbooth-relay) [ -z "$DIG_relay" ] || return 1; DIG_relay=${BASH_REMATCH[2]} ;;
      satbooth-tools) [ -z "$DIG_tools" ] || return 1; DIG_tools=${BASH_REMATCH[2]} ;;
    esac
  done < "$1"
  [ "$count" -eq 3 ]
}

d=$tmp/root
digests=$tmp/digests.txt
accepted_format=0 accepted_check=0 accepted_set=0 refused_set=0 c=0

report() {
  echo "seed $seed, case $c, mutations:$log" >&2
  for f in deploy/public/compose.yml deploy/hub/compose.yml deploy/setup.sh; do echo "--- $f" >&2; sed -n l < "$d/$f" >&2; done
  echo "--- digests.txt" >&2
  sed -n l < "$digests" >&2
}

# run_case <file:mutation>...: builds the files, applies the mutations in order, and compares the script with the oracle
# for check-format, version, check (with a tag) and set.
run_case() {
  local spec f mut set_ m
  c=$((c + 1))
  reset_model
  for spec in "$@"; do
    f=${spec%%:*} mut=${spec#*:}
    log+=" f$f:$mut"
    "mut_$mut" "$f"
  done
  rm -rf "$d" "$tmp/before"
  mkdir -p "$d/deploy/public" "$d/deploy/hub"
  write_file 0 "$d/deploy/public/compose.yml"
  write_file 1 "$d/deploy/hub/compose.yml"
  write_file 2 "$d/deploy/setup.sh"
  write_file 3 "$digests"

  # The oracle's verdict.
  local want_format=0 want_check=0 want_ver='' want_set=0 v0 v1 v2 g0 g1 g2
  if oracle_site "$d/deploy/public/compose.yml" compose satbooth && v0=$REF_VER g0=$REF_DIG &&
    oracle_site "$d/deploy/hub/compose.yml" compose satbooth-relay && v1=$REF_VER g1=$REF_DIG &&
    oracle_site "$d/deploy/setup.sh" tools satbooth-tools && v2=$REF_VER g2=$REF_DIG &&
    [ "$v0" = "$v1" ] && [ "$v1" = "$v2" ]; then
    want_format=1 want_ver=$v0
    if oracle_digests "$digests" && [ "$g0" = "$DIG_satbooth" ] && [ "$g1" = "$DIG_relay" ] && [ "$g2" = "$DIG_tools" ]; then
      want_check=1
    fi
  fi
  if oracle_digests "$digests" && oracle_replaceable "$d/deploy/public/compose.yml" compose &&
    oracle_replaceable "$d/deploy/hub/compose.yml" compose && oracle_replaceable "$d/deploy/setup.sh" tools; then
    want_set=1
  fi

  local got_format=0 got_check=0 ver_out
  if RELEASE_PINS_ROOT=$d "$pins" check-format > /dev/null 2>&1; then got_format=1; fi
  [ "$got_format" = "$want_format" ] || { report; fail "check-format: got $got_format, the oracle says $want_format"; }
  if [ "$want_format" = 1 ]; then
    accepted_format=$((accepted_format + 1))
  fi
  # check runs with the pinned version as its tag when the oracle reads one, so the tag passes too.
  if RELEASE_PINS_ROOT=$d "$pins" check "$digests" ${want_ver:+"$want_ver"} > /dev/null 2>&1; then got_check=1; fi
  [ "$got_check" = "$want_check" ] || { report; fail "check: got $got_check, the oracle says $want_check"; }
  if [ "$want_check" = 1 ]; then
    accepted_check=$((accepted_check + 1))
    # Another tag does not pass.
    if RELEASE_PINS_ROOT=$d "$pins" check "$digests" "${want_ver}x" > /dev/null 2>&1; then report; fail "check with another tag accepted"; fi
  fi

  # set: when the oracle accepts the digests file and the sites, set then show yields exactly those digests and
  # check-format passes; otherwise set fails and the tree is byte for byte what it was.
  local setver shown expect got_set=0
  roll ${#versions_ok[@]}; setver=${versions_ok[RND]}
  cp -R "$d" "$tmp/before"
  if RELEASE_PINS_ROOT=$d "$pins" set "$setver" "$digests" > /dev/null 2>&1; then got_set=1; fi
  [ "$got_set" = "$want_set" ] || { report; fail "set $setver: got $got_set, the oracle says $want_set"; }
  if [ "$want_set" = 1 ]; then
    accepted_set=$((accepted_set + 1))
    expect="satbooth $ns/satbooth:$setver@$DIG_satbooth
satbooth-relay $ns/satbooth-relay:$setver@$DIG_relay
satbooth-tools $ns/satbooth-tools:$setver@$DIG_tools"
    shown=$(RELEASE_PINS_ROOT=$d "$pins" show) || { report; fail "show failed after set"; }
    [ "$shown" = "$expect" ] || { report; fail "show after set: got
$shown
want
$expect"; }
    ver_out=$(RELEASE_PINS_ROOT=$d "$pins" version) || { report; fail "version (that is check-format) refused the result of set"; }
    [ "$ver_out" = "$setver" ] || { report; fail "version after set: got $ver_out, want $setver"; }
    RELEASE_PINS_ROOT=$d "$pins" check "$digests" "$setver" > /dev/null 2>&1 || { report; fail "check refused the result of set"; }
  else
    refused_set=$((refused_set + 1))
    diff -r "$d" "$tmp/before" > /dev/null 2>&1 || { report; fail "a refused set changed the tree or left a file"; }
  fi
}

# The fixed cases come first, in every run: each odd input that once mattered, on the digests file, on a compose site and
# on the tools site, before any random one. A mutation that does not apply to a file leaves it valid.
fixed=(no_final_newline crlf tab_separator nul vt ff nbsp trailing_blank duplicate_line extra_line drop_line extra_name)
for mut in "${fixed[@]}"; do
  for f in 3 0 2; do run_case "$f:$mut"; done
done
fixed_cases=$c

# Then the random ones: one to three mutations, each on a random file.
while [ "$c" -lt $((fixed_cases + cases)) ]; do
  roll 3; nmut=$((RND + 1))
  specs=()
  for ((m = 0; m < nmut; m++)); do
    roll 4; f=$RND
    if [ "$f" = 3 ]; then set_=("${digests_mutations[@]}"); else set_=("${site_mutations[@]}"); fi
    roll ${#set_[@]}; specs+=("$f:${set_[RND]}")
  done
  run_case "${specs[@]}"
done

# check-tag: random strings built from the pieces of a tag, in random order and number, against the oracle's reading:
# v, three numbers with dots, and optionally -rc. and a number.
pieces=(v V 0 1 12 007 . .. - -rc rc .rc. -rc. ' ' $'\n' x '')
t=0
while [ "$t" -lt 80 ]; do
  t=$((t + 1))
  if [ "$t" -le 20 ]; then
    # Shaped like a tag, then mutated by one piece.
    roll 5; cand=v$((RND + 1)).$((RND * 3)).$((RND + 7))
    roll 3; if [ "$RND" = 0 ]; then cand+=-rc.$RND; fi
    roll 4; if [ "$RND" = 0 ]; then roll ${#pieces[@]}; cand+=${pieces[RND]}; fi
  else
    cand=''; roll 12
    for ((k = 0; k <= RND; k++)); do roll ${#pieces[@]}; cand+=${pieces[RND]}; done
  fi
  want=0
  if [[ $cand =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] && [[ $cand != *$'\n'* ]]; then want=1; fi
  got=0
  if "$pins" check-tag "$cand" > /dev/null 2>&1; then got=1; fi
  [ "$got" = "$want" ] || fail "seed $seed: check-tag \"$cand\": got $got, the oracle says $want"
done

# Every case must have been able to go either way: some were accepted, and most were refused.
[ "$accepted_format" -gt 0 ] && [ "$accepted_check" -gt 0 ] || fail "no case was accepted: the mutations are too strong"
[ "$accepted_check" -lt "$c" ] || fail "every case was accepted: the mutations change nothing"
[ "$accepted_set" -gt 0 ] && [ "$refused_set" -gt 0 ] || fail "set went one way only: accepted $accepted_set, refused $refused_set"
echo "release-pins_prop_test: $c cases ($fixed_cases fixed), accepted by check-format $accepted_format, by check $accepted_check, by set $accepted_set: ok"
