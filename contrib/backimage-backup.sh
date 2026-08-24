#!/usr/bin/env bash
#
# backimage-backup.sh — cron wrapper around `backimage backup`.
#
# One invocation runs one backup job: it takes a lock so two runs never
# overlap, writes a timestamped log, runs the backup, optionally prunes old
# tags, and reports the outcome to a Slack or Google Chat incoming webhook.
#
# Configuration comes from BI_* variables, read from a config file
# (default /etc/backimage/backup.env) and/or from the environment; a variable
# already set in the environment always wins over the config file.
#
# Usage:
#   backimage-backup.sh [-c FILE] [--dry-run] [--test-notify] [--print-config]
#
# Exit status: the exit status of `backimage backup` (0 ok, 1 generic,
# 2 usage, 3 privileges, 4 passphrase, 5 integrity, 6 network, 7 interrupted),
# or 78 for a wrapper configuration error, 75 when another run holds the lock,
# 124 when the run hit BI_TIMEOUT.

set -Eeuo pipefail

readonly WRAPPER_VERSION='1.0.0'
readonly PROGNAME=${0##*/}

# cron gives a minimal environment: make it predictable.
export LC_ALL=C
case ":${PATH:-}:" in
  *:/usr/local/bin:*) ;;
  *) PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}" ;;
esac
export PATH
umask 077

# ---------------------------------------------------------------------------
# defaults (every one overridable by config file or environment)
# ---------------------------------------------------------------------------

set_defaults() {
  : "${BI_BIN:=backimage}"          # backimage executable (name or path)
  : "${BI_JOB_NAME:=backup}"        # short id: lock name, log name, message title
  : "${BI_DESCRIPTION:=}"           # free text sent with the notification
  : "${BI_WORKDIR:=/}"              # cwd of the run; keeps no mount busy

  # what to back up
  : "${BI_PATHS:=}"                 # one source path per line (required)
  : "${BI_REPO:=}"                  # target repository, no tag (required)
  : "${BI_TAG:=daily}"
  : "${BI_TIMESTAMP:=true}"         # append a UTC stamp: one tag per run
  : "${BI_TIMESTAMP_FORMAT:=}"
  : "${BI_EXCLUDES:=}"              # one glob per line

  # keys
  : "${BI_PASSPHRASE_FILE:=}"
  : "${BI_RECIPIENTS:=}"            # one age public key per line
  : "${BI_NO_ENCRYPT:=false}"       # only for data that needs no confidentiality
  : "${BI_AGE_IDENTITY:=}"
  : "${BI_AUTH_FILE:=}"             # registry credentials (BACKIMAGE_AUTH_FILE)
  : "${BI_REGISTRY_USER:=}"

  # pipeline knobs (anything else goes in BI_EXTRA_ARGS / BI_EXTRA_ARGV)
  : "${BI_COMPRESSION:=}"
  : "${BI_COMPRESSION_LEVEL:=}"
  : "${BI_JOBS:=}"
  : "${BI_MAX_LAYER_SIZE:=}"
  : "${BI_TEMP_DIR:=}"
  : "${BI_ONE_FILE_SYSTEM:=false}"
  : "${BI_ALLOW_DEGRADED:=false}"
  : "${BI_DEDUP:=false}"
  : "${BI_VERIFY_AFTER_PUSH:=}"     # quick|full|off
  : "${BI_OUTPUT:=}"                # registry|daemon|oci-layout|tar
  : "${BI_OUTPUT_PATH:=}"
  : "${BI_LOCAL_REPO:=false}"
  : "${BI_VERBOSE:=0}"              # 0, 1 (-v), 2 (-vv)
  : "${BI_EXTRA_ARGS:=}"            # extra flags, split on whitespace
  # BI_EXTRA_ARGV: bash array, for flags containing spaces

  # remote server (backimage listen-remote)
  : "${BI_REMOTE:=}"
  : "${BI_REMOTE_MODE:=}"
  : "${BI_TLS_PIN:=}"
  : "${BI_TLS_CA:=}"
  : "${BI_TLS_CERT:=}"
  : "${BI_TLS_KEY:=}"
  : "${BI_AUTH_TOKEN_FILE:=}"
  : "${BI_UDP:=false}"

  # execution
  : "${BI_TIMEOUT:=}"               # e.g. 6h; empty or 0 = no limit
  : "${BI_NICE:=}"                  # e.g. 10
  : "${BI_IONICE_CLASS:=}"          # e.g. 2
  : "${BI_IONICE_LEVEL:=}"          # e.g. 7
  : "${BI_LOCK_FILE:=}"             # default: /var/lock/backimage-<job>.lock
  : "${BI_LOCK_WAIT:=0}"            # seconds to wait for the lock
  : "${BI_ON_LOCKED:=fail}"         # fail|skip when another run holds it
  : "${BI_PRE_CMD:=}"               # shell command run before the backup
  : "${BI_POST_CMD:=}"              # shell command run after it, always

  # logging
  : "${BI_LOG_DIR:=/var/log/backimage}"
  : "${BI_LOG_KEEP:=30}"            # per-run logs to keep; 0 = keep all
  : "${BI_STDERR:=auto}"            # auto (warnings+errors) | always | never

  # retention on the registry, after a successful backup
  : "${BI_PRUNE:=false}"
  : "${BI_PRUNE_KEEP_LAST:=}"
  : "${BI_PRUNE_KEEP_WITHIN:=}"     # e.g. 30d
  : "${BI_PRUNE_KEEP_TAGS:=}"       # one glob per line
  : "${BI_PRUNE_TAG_REGEX:=}"
  : "${BI_PRUNE_EXTRA_ARGS:=}"

  # notification
  : "${BI_NOTIFY:=on-error}"        # always|on-error|never
  : "${BI_NOTIFY_TARGET:=auto}"     # auto|slack|google-chat
  : "${BI_WEBHOOK_URL:=}"
  : "${BI_WEBHOOK_URL_FILE:=}"      # preferred: keeps the URL out of the env
  : "${BI_NOTIFY_TIMEOUT:=15}"      # seconds per HTTP attempt
  : "${BI_NOTIFY_RETRIES:=3}"
  : "${BI_NOTIFY_TAIL:=20}"         # log lines attached to a failure
  : "${BI_NOTIFY_MENTION:=}"        # e.g. '<!channel>' (Slack), '<users/all>' (Chat)
  : "${BI_HOSTNAME:=}"              # defaults to the machine name
}

# ---------------------------------------------------------------------------
# small helpers
# ---------------------------------------------------------------------------

CONFIG_FILE=''
DRY_RUN=false
TEST_NOTIFY=false
PRINT_CONFIG=false
LOG_FILE=''
JQ=''
STARTED_AT=''
START_EPOCH=0
CHILD_PID=''
NOTIFIED=false
PRESET_ENV=()   # BI_* variables that came from the real environment

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }

# snapshot of the BI_* variables set before anything else: they win over the
# config file, which is sourced on top of them.
capture_preset_env() {
  local kv
  while IFS= read -r -d '' kv; do
    [[ $kv == BI_* ]] && PRESET_ENV+=("$kv")
  done < <(env -0)
  return 0
}

log() {
  local level=$1; shift
  local line
  line="$(ts) [$level] $*"
  if [[ -n $LOG_FILE ]]; then
    printf '%s\n' "$line" >>"$LOG_FILE" 2>/dev/null || true
  fi
  case $BI_STDERR in
    always) printf '%s\n' "$line" >&2 ;;
    never) ;;
    *) [[ $level == ERROR || $level == WARN ]] && printf '%s\n' "$line" >&2 ;;
  esac
  return 0
}

is_true() {
  case ${1,,} in
    1 | true | yes | on) return 0 ;;
    *) return 1 ;;
  esac
}

# splits a multi-line list; a single line is split on whitespace instead, so
# both "a b c" and one-item-per-line work. Blank lines and # comments go away.
split_list() {
  local raw=$1 line
  if [[ $raw != *$'\n'* ]]; then
    read -r -a _SPLIT <<<"$raw"
    return 0
  fi
  _SPLIT=()
  while IFS= read -r line; do
    line=${line#"${line%%[![:space:]]*}"}
    line=${line%"${line##*[![:space:]]}"}
    [[ -z $line || $line == '#'* ]] && continue
    _SPLIT+=("$line")
  done <<<"$raw"
  return 0
}

human_bytes() {
  local b=${1:-0} unit=0 frac=0
  local -a units=(B KiB MiB GiB TiB PiB)
  [[ $b =~ ^[0-9]+$ ]] || { printf 'n/a'; return; }
  while ((b >= 1024 && unit < 5)); do
    frac=$(((b % 1024) * 10 / 1024))
    b=$((b / 1024))
    unit=$((unit + 1))
  done
  if ((unit == 0)); then
    printf '%d %s' "$b" "${units[$unit]}"
  else
    printf '%d.%d %s' "$b" "$frac" "${units[$unit]}"
  fi
}

human_duration() {
  local s=${1:-0}
  [[ $s =~ ^[0-9]+$ ]] || { printf 'n/a'; return; }
  printf '%02dh%02dm%02ds' $((s / 3600)) $(((s % 3600) / 60)) $((s % 60))
}

exit_meaning() {
  case $1 in
    0) printf 'success' ;;
    1) printf 'generic failure' ;;
    2) printf 'usage error' ;;
    3) printf 'insufficient privileges' ;;
    4) printf 'missing or wrong passphrase' ;;
    5) printf 'integrity failure' ;;
    6) printf 'network or registry error' ;;
    7) printf 'interrupted' ;;
    75) printf 'another run holds the lock' ;;
    78) printf 'wrapper configuration error' ;;
    124) printf 'timed out after %s' "${BI_TIMEOUT:-?}" ;;
    130 | 143) printf 'interrupted by a signal' ;;
    *) printf 'unknown' ;;
  esac
}

json_escape() {
  local s=${1//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\r'/\\r}
  s=${s//$'\t'/\\t}
  printf '%s' "$s" | tr -d '\000-\010\013\014\016-\037'
}

# json_field <json> <key> — jq when available, a tolerant fallback otherwise.
json_field() {
  local json=$1 key=$2
  if [[ -n $JQ ]]; then
    # shellcheck disable=SC2016  # the $k is a jq variable, not a shell one
    printf '%s' "$json" | "$JQ" -r --arg k "$key" '.[$k] // empty' 2>/dev/null || true
    return 0
  fi
  printf '%s' "$json" | tr -d '\n' |
    grep -o "\"$key\"[[:space:]]*:[[:space:]]*\(\"[^\"]*\"\|[^,}]*\)" |
    head -n1 |
    sed -e "s/^\"$key\"[[:space:]]*:[[:space:]]*//" -e 's/^"//' -e 's/"$//' || true
}

die() { # wrapper configuration error: nothing ran, so report it as a failure
  log ERROR "$*"
  finish 78 "$*"
}

# ---------------------------------------------------------------------------
# configuration
# ---------------------------------------------------------------------------

usage() {
  cat <<EOF
$PROGNAME $WRAPPER_VERSION — cron wrapper for backimage backup

  -c, --config FILE   config file to source (default: \$BI_CONFIG, then
                      /etc/backimage/backup.env)
  -n, --dry-run       run backimage with --dry-run; no push, no prune,
                      no notification
      --test-notify   send a test notification and exit
      --print-config  print the resolved configuration (secrets redacted)
  -h, --help          this text
      --version       wrapper version

Configuration is read from BI_* variables; the environment wins over the
config file. See backimage-backup.env.example for the full list.
EOF
}

parse_args() {
  while (($# > 0)); do
    case $1 in
      -c | --config)
        [[ $# -ge 2 ]] || { echo "$PROGNAME: --config needs a value" >&2; exit 78; }
        CONFIG_FILE=$2
        shift 2
        ;;
      --config=*)
        CONFIG_FILE=${1#*=}
        shift
        ;;
      -n | --dry-run)
        DRY_RUN=true
        shift
        ;;
      --test-notify)
        TEST_NOTIFY=true
        shift
        ;;
      --print-config)
        PRINT_CONFIG=true
        shift
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      --version)
        printf '%s %s\n' "$PROGNAME" "$WRAPPER_VERSION"
        exit 0
        ;;
      *)
        echo "$PROGNAME: unknown argument '$1'" >&2
        usage >&2
        exit 78
        ;;
    esac
  done
}

load_config() {
  local file=${CONFIG_FILE:-${BI_CONFIG:-/etc/backimage/backup.env}}
  if [[ ! -f $file ]]; then
    if [[ -n $CONFIG_FILE ]]; then
      echo "$PROGNAME: config file '$file' not found" >&2
      exit 78
    fi
    return 0
  fi
  [[ -r $file ]] || { echo "$PROGNAME: config file '$file' not readable" >&2; exit 78; }

  set -a
  # shellcheck source=/dev/null
  source "$file"
  set +a

  # the environment wins over the file: put the snapshot back.
  local kv
  for kv in "${PRESET_ENV[@]}"; do
    export "${kv?}"
  done
  CONFIG_FILE=$file
}

validate_config() {
  command -v "$BI_BIN" >/dev/null 2>&1 || [[ -x $BI_BIN ]] ||
    die "backimage executable '$BI_BIN' not found (set BI_BIN)"
  command -v flock >/dev/null 2>&1 || die "flock not found (install util-linux)"
  [[ -n $BI_PATHS ]] || die "BI_PATHS is empty: nothing to back up"
  [[ -n $BI_REPO ]] || die "BI_REPO is empty: no target repository"

  local p
  split_list "$BI_PATHS"
  for p in "${_SPLIT[@]}"; do
    [[ -e $p ]] || die "source path '$p' does not exist"
  done

  if [[ -n $BI_PASSPHRASE_FILE ]]; then
    [[ -r $BI_PASSPHRASE_FILE ]] || die "passphrase file '$BI_PASSPHRASE_FILE' not readable"
    local mode
    mode=$(stat -c '%a' "$BI_PASSPHRASE_FILE" 2>/dev/null || echo '')
    [[ -n $mode && ${mode: -2} == 00 ]] || log WARN "passphrase file '$BI_PASSPHRASE_FILE' is group/world readable (mode ${mode:-?})"
  elif [[ -n $BI_RECIPIENTS ]]; then
    :
  elif is_true "$BI_NO_ENCRYPT"; then
    log WARN 'BI_NO_ENCRYPT is set: the backup will not be encrypted'
  else
    die 'no key material: set BI_PASSPHRASE_FILE, BI_RECIPIENTS, or BI_NO_ENCRYPT=true'
  fi

  case $BI_NOTIFY in
    always | on-error | never) ;;
    *) die "BI_NOTIFY must be always|on-error|never (got '$BI_NOTIFY')" ;;
  esac
  case $BI_NOTIFY_TARGET in
    auto | slack | google-chat) ;;
    *) die "BI_NOTIFY_TARGET must be auto|slack|google-chat (got '$BI_NOTIFY_TARGET')" ;;
  esac
  case $BI_ON_LOCKED in
    fail | skip) ;;
    *) die "BI_ON_LOCKED must be fail|skip (got '$BI_ON_LOCKED')" ;;
  esac

  if [[ $BI_NOTIFY != never ]]; then
    [[ -n $(webhook_url) ]] || die 'BI_NOTIFY is enabled but no webhook: set BI_WEBHOOK_URL or BI_WEBHOOK_URL_FILE'
    command -v curl >/dev/null 2>&1 || die 'curl not found, required to send notifications'
    [[ $(notify_target) != unknown ]] ||
      die "cannot infer the webhook flavour from the URL: set BI_NOTIFY_TARGET to slack or google-chat"
  fi

  if is_true "$BI_PRUNE"; then
    [[ -n $BI_PRUNE_KEEP_LAST || -n $BI_PRUNE_KEEP_WITHIN || -n $BI_PRUNE_EXTRA_ARGS ]] ||
      die 'BI_PRUNE is on but no retention rule: set BI_PRUNE_KEEP_LAST and/or BI_PRUNE_KEEP_WITHIN'
  fi
}

setup_runtime() {
  JOB_SLUG=${BI_JOB_NAME//[^A-Za-z0-9._-]/-}
  [[ -n $JOB_SLUG ]] || JOB_SLUG=backup
  [[ -n $BI_HOSTNAME ]] || BI_HOSTNAME=${HOSTNAME:-$(uname -n)}
  command -v jq >/dev/null 2>&1 && JQ=$(command -v jq)
  [[ -n $BI_AUTH_FILE ]] && export BACKIMAGE_AUTH_FILE=$BI_AUTH_FILE

  if ! mkdir -p "$BI_LOG_DIR" 2>/dev/null; then
    BI_LOG_DIR="${TMPDIR:-/tmp}/backimage-logs"
    mkdir -p "$BI_LOG_DIR" || { echo "$PROGNAME: cannot create a log directory" >&2; exit 78; }
    echo "$PROGNAME: falling back to log dir $BI_LOG_DIR" >&2
  fi
  LOG_FILE="$BI_LOG_DIR/${JOB_SLUG}-$(date -u '+%Y%m%dT%H%M%SZ').log"
  : >"$LOG_FILE" || { echo "$PROGNAME: cannot write $LOG_FILE" >&2; exit 78; }

  if [[ -z $BI_LOCK_FILE ]]; then
    if [[ -d /var/lock && -w /var/lock ]]; then
      BI_LOCK_FILE="/var/lock/backimage-${JOB_SLUG}.lock"
    else
      BI_LOCK_FILE="${TMPDIR:-/tmp}/backimage-${JOB_SLUG}.lock"
    fi
  fi
  [[ -n $BI_DESCRIPTION ]] || BI_DESCRIPTION="backup of ${BI_PATHS//$'\n'/, } to $BI_REPO"
}

print_config() {
  local v
  for v in $(compgen -v BI_ | sort); do
    case $v in
      BI_WEBHOOK_URL) [[ -n ${!v} ]] && printf '%s=%s\n' "$v" '<redacted>' || printf '%s=\n' "$v" ;;
      *) printf '%s=%s\n' "$v" "${!v}" ;;
    esac
  done
  printf 'CONFIG_FILE=%s\nLOG_FILE=%s\nNOTIFY_TARGET=%s\n' \
    "${CONFIG_FILE:-<none>}" "$LOG_FILE" "$(notify_target)"
}

# ---------------------------------------------------------------------------
# notification
# ---------------------------------------------------------------------------

webhook_url() {
  if [[ -n $BI_WEBHOOK_URL_FILE && -r $BI_WEBHOOK_URL_FILE ]]; then
    head -n1 "$BI_WEBHOOK_URL_FILE" | tr -d '[:space:]'
    return 0
  fi
  printf '%s' "$BI_WEBHOOK_URL"
}

notify_target() {
  if [[ $BI_NOTIFY_TARGET != auto ]]; then
    printf '%s' "$BI_NOTIFY_TARGET"
    return 0
  fi
  local url
  url=$(webhook_url)
  case $url in
    *hooks.slack.com*) printf 'slack' ;;
    *chat.googleapis.com* | *googleapis.com/v1/spaces*) printf 'google-chat' ;;
    *) printf 'unknown' ;;
  esac
}

# http_post <url> <payload> — retries on 429/5xx/transport errors.
http_post() {
  local url=$1 payload=$2 attempt=1 code body
  while :; do
    body=$(printf '%s' "$payload" |
      curl -sS -o /dev/null -w '%{http_code}' \
        --max-time "$BI_NOTIFY_TIMEOUT" \
        -X POST -H 'Content-Type: application/json; charset=utf-8' \
        --data-binary @- "$url" 2>&1) || body=''
    code=${body##*$'\n'}
    [[ $code =~ ^[0-9]{3}$ ]] || code=000
    if [[ $code == 2?? ]]; then
      log INFO "notification sent (HTTP $code, attempt $attempt)"
      return 0
    fi
    if [[ $code != 000 && $code != 429 && $code != 5?? ]]; then
      log ERROR "notification refused (HTTP $code), giving up"
      return 1
    fi
    if ((attempt >= BI_NOTIFY_RETRIES)); then
      log ERROR "notification failed after $attempt attempts (HTTP $code)"
      return 1
    fi
    log WARN "notification attempt $attempt failed (HTTP $code), retrying"
    sleep $((attempt * 5))
    attempt=$((attempt + 1))
  done
}

# build_body <status> <detail> — one text for both flavours: * bold * and
# ``` fences render in Slack mrkdwn and in Google Chat alike.
build_body() {
  local status=$1 detail=$2 icon head body
  case $status in
    OK) icon='✅'; head='BACKUP OK' ;;
    SKIPPED) icon='⏭️'; head='BACKUP SKIPPED' ;;
    TEST) icon='🔔'; head='BACKIMAGE TEST' ;;
    *) icon='🚨'; head='BACKUP FAILED' ;;
  esac

  body="$icon *${head}* — ${BI_JOB_NAME}"
  [[ $status != OK && -n $BI_NOTIFY_MENTION ]] && body+=" ${BI_NOTIFY_MENTION}"
  body+=$'\n'"*Description:* ${BI_DESCRIPTION}"
  body+=$'\n'"*Host:* ${BI_HOSTNAME}    *Started:* ${STARTED_AT:-$(ts)}"
  body+=$'\n'"*Repository:* ${BI_REPO}"
  [[ -n ${RESULT_REF:-} ]] && body+=$'\n'"*Image:* \`${RESULT_REF}\`"
  [[ -n ${RESULT_DIGEST:-} ]] && body+=$'\n'"*Digest:* \`${RESULT_DIGEST}\`"

  if [[ $status == OK ]]; then
    body+=$'\n'"*Files:* ${RESULT_FILES:-n/a}    *Source:* $(human_bytes "${RESULT_BYTES_RAW:-0}") → *Stored:* $(human_bytes "${RESULT_BYTES_STORED:-0}")"
    body+=$'\n'"*Uploaded:* $(human_bytes "${RESULT_UPLOADED:-0}")    *Reused:* $(human_bytes "${RESULT_SKIPPED:-0}")"
  fi
  body+=$'\n'"*Duration:* $(human_duration "${ELAPSED:-0}")"
  [[ -n ${PRUNE_NOTE:-} ]] && body+=$'\n'"*Retention:* ${PRUNE_NOTE}"
  [[ -n $detail ]] && body+=$'\n'"*Detail:* ${detail}"
  body+=$'\n'"*Log:* \`${LOG_FILE}\`"

  if [[ $status == FAILED && ${BI_NOTIFY_TAIL:-0} -gt 0 && -s $LOG_FILE ]]; then
    local tail_txt
    tail_txt=$(tail -n "$BI_NOTIFY_TAIL" "$LOG_FILE" | cut -c1-300)
    tail_txt=${tail_txt:0:2200}
    [[ -n $tail_txt ]] && body+=$'\n'"\`\`\`"$'\n'"${tail_txt}"$'\n'"\`\`\`"
  fi
  printf '%s' "$body"
}

# notify <status> [detail]
notify() {
  local status=$1 detail=${2:-}
  [[ $BI_NOTIFY == never ]] && return 0
  [[ $BI_NOTIFY == on-error && $status == OK ]] && return 0
  $NOTIFIED && return 0

  local url target body payload color
  url=$(webhook_url)
  [[ -n $url ]] || { log ERROR 'no webhook URL: notification skipped'; return 1; }
  target=$(notify_target)
  body=$(build_body "$status" "$detail")

  case $target in
    slack)
      case $status in
        OK) color='#2eb886' ;;
        SKIPPED) color='#daa038' ;;
        TEST) color='#3aa3e3' ;;
        *) color='#d0342c' ;;
      esac
      payload=$(printf '{"text":"%s","attachments":[{"color":"%s","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"%s"}}]}]}' \
        "$(json_escape "backimage ${BI_JOB_NAME}: ${status} on ${BI_HOSTNAME}")" \
        "$color" \
        "$(json_escape "$body")")
      ;;
    google-chat)
      payload=$(printf '{"text":"%s"}' "$(json_escape "$body")")
      ;;
    *)
      log ERROR "unknown notification target '$target': notification skipped"
      return 1
      ;;
  esac

  if http_post "$url" "$payload"; then
    NOTIFIED=true
    return 0
  fi
  return 1
}

# ---------------------------------------------------------------------------
# the run
# ---------------------------------------------------------------------------

build_backup_args() {
  BACKUP_ARGS=(backup)
  split_list "$BI_PATHS"
  BACKUP_ARGS+=("${_SPLIT[@]}")
  BACKUP_ARGS+=(--repo "$BI_REPO" --tag "$BI_TAG" --json)

  is_true "$BI_TIMESTAMP" && BACKUP_ARGS+=(--timestamp)
  [[ -n $BI_TIMESTAMP_FORMAT ]] && BACKUP_ARGS+=(--timestamp-format "$BI_TIMESTAMP_FORMAT")

  if [[ -n $BI_EXCLUDES ]]; then
    local x
    split_list "$BI_EXCLUDES"
    for x in "${_SPLIT[@]}"; do BACKUP_ARGS+=(--exclude "$x"); done
  fi

  if [[ -n $BI_PASSPHRASE_FILE ]]; then
    BACKUP_ARGS+=(--passphrase-file "$BI_PASSPHRASE_FILE")
  fi
  if [[ -n $BI_RECIPIENTS ]]; then
    local r
    split_list "$BI_RECIPIENTS"
    for r in "${_SPLIT[@]}"; do BACKUP_ARGS+=(--recipient "$r"); done
  fi
  if [[ -z $BI_PASSPHRASE_FILE && -z $BI_RECIPIENTS ]] && is_true "$BI_NO_ENCRYPT"; then
    BACKUP_ARGS+=(--no-encrypt)
  fi
  [[ -n $BI_AGE_IDENTITY ]] && BACKUP_ARGS+=(--age-identity "$BI_AGE_IDENTITY")

  [[ -n $BI_COMPRESSION ]] && BACKUP_ARGS+=(--compression "$BI_COMPRESSION")
  [[ -n $BI_COMPRESSION_LEVEL ]] && BACKUP_ARGS+=(--compression-level "$BI_COMPRESSION_LEVEL")
  [[ -n $BI_JOBS ]] && BACKUP_ARGS+=(--jobs "$BI_JOBS")
  [[ -n $BI_MAX_LAYER_SIZE ]] && BACKUP_ARGS+=(--max-layer-size "$BI_MAX_LAYER_SIZE")
  [[ -n $BI_TEMP_DIR ]] && BACKUP_ARGS+=(--temp-dir "$BI_TEMP_DIR")
  is_true "$BI_ONE_FILE_SYSTEM" && BACKUP_ARGS+=(--one-file-system)
  is_true "$BI_ALLOW_DEGRADED" && BACKUP_ARGS+=(--allow-degraded)
  is_true "$BI_DEDUP" && BACKUP_ARGS+=(--dedup)
  [[ -n $BI_VERIFY_AFTER_PUSH ]] && BACKUP_ARGS+=(--verify-after-push "$BI_VERIFY_AFTER_PUSH")
  [[ -n $BI_OUTPUT ]] && BACKUP_ARGS+=(--output "$BI_OUTPUT")
  [[ -n $BI_OUTPUT_PATH ]] && BACKUP_ARGS+=(--output-path "$BI_OUTPUT_PATH")
  is_true "$BI_LOCAL_REPO" && BACKUP_ARGS+=(--local-repo)
  [[ -n $BI_REGISTRY_USER ]] && BACKUP_ARGS+=(--registry-user "$BI_REGISTRY_USER")

  [[ -n $BI_REMOTE ]] && BACKUP_ARGS+=(--remote "$BI_REMOTE")
  [[ -n $BI_REMOTE_MODE ]] && BACKUP_ARGS+=(--remote-mode "$BI_REMOTE_MODE")
  [[ -n $BI_TLS_PIN ]] && BACKUP_ARGS+=(--tls-pin "$BI_TLS_PIN")
  [[ -n $BI_TLS_CA ]] && BACKUP_ARGS+=(--tls-ca "$BI_TLS_CA")
  [[ -n $BI_TLS_CERT ]] && BACKUP_ARGS+=(--tls-cert "$BI_TLS_CERT")
  [[ -n $BI_TLS_KEY ]] && BACKUP_ARGS+=(--tls-key "$BI_TLS_KEY")
  [[ -n $BI_AUTH_TOKEN_FILE ]] && BACKUP_ARGS+=(--auth-token-file "$BI_AUTH_TOKEN_FILE")
  is_true "$BI_UDP" && BACKUP_ARGS+=(--udp)

  case $BI_VERBOSE in
    1) BACKUP_ARGS+=(-v) ;;
    2) BACKUP_ARGS+=(-vv) ;;
  esac

  if [[ -n $BI_EXTRA_ARGS ]]; then
    local -a extra=()
    read -r -a extra <<<"$BI_EXTRA_ARGS"
    BACKUP_ARGS+=("${extra[@]}")
  fi
  if [[ -n ${BI_EXTRA_ARGV+x} ]]; then
    BACKUP_ARGS+=("${BI_EXTRA_ARGV[@]}")
  fi
  $DRY_RUN && BACKUP_ARGS+=(--dry-run)
  return 0
}

# runner prefix: timeout / nice / ionice, in that order
build_runner() {
  RUNNER=()
  if [[ -n $BI_TIMEOUT && $BI_TIMEOUT != 0 ]] && command -v timeout >/dev/null 2>&1; then
    RUNNER+=(timeout --signal=TERM --kill-after=60s "$BI_TIMEOUT")
  elif [[ -n $BI_TIMEOUT && $BI_TIMEOUT != 0 ]]; then
    log WARN 'timeout(1) not found: BI_TIMEOUT ignored'
  fi
  if [[ -n $BI_NICE ]] && command -v nice >/dev/null 2>&1; then
    RUNNER+=(nice -n "$BI_NICE")
  fi
  if [[ -n $BI_IONICE_CLASS ]] && command -v ionice >/dev/null 2>&1; then
    RUNNER+=(ionice -c "$BI_IONICE_CLASS")
    [[ -n $BI_IONICE_LEVEL ]] && RUNNER+=(-n "$BI_IONICE_LEVEL")
  fi
  return 0
}

acquire_lock() {
  exec 9>"$BI_LOCK_FILE" || die "cannot open lock file '$BI_LOCK_FILE'"
  if flock -w "$BI_LOCK_WAIT" -x 9; then
    return 0
  fi
  if [[ $BI_ON_LOCKED == skip ]]; then
    log WARN "another run holds $BI_LOCK_FILE: skipping"
    ELAPSED=0
    notify SKIPPED 'another run of this job is still in progress' || true
    exit 0
  fi
  log ERROR "another run holds $BI_LOCK_FILE after ${BI_LOCK_WAIT}s"
  ELAPSED=0
  notify FAILED "another run of this job is still in progress (lock $BI_LOCK_FILE)" || true
  exit 75
}

rotate_logs() {
  [[ ${BI_LOG_KEEP:-0} -gt 0 && -n ${JOB_SLUG:-} && -d ${BI_LOG_DIR:-} ]] || return 0
  local f
  while IFS= read -r f; do
    [[ -n $f ]] && rm -f -- "$f"
  done < <(find "$BI_LOG_DIR" -maxdepth 1 -type f -name "${JOB_SLUG}-*.log" 2>/dev/null |
    sort -r | tail -n +$((BI_LOG_KEEP + 1)))
  return 0
}

run_hook() { # run_hook <label> <command>
  local label=$1 cmd=$2
  [[ -n $cmd ]] || return 0
  log INFO "$label hook: $cmd"
  if bash -c "$cmd" >>"$LOG_FILE" 2>&1; then
    return 0
  fi
  local rc=$?
  log ERROR "$label hook failed with exit code $rc"
  return $rc
}

run_prune() {
  is_true "$BI_PRUNE" || return 0
  $DRY_RUN && return 0
  local -a args=(repo prune "$BI_REPO" --yes --json)
  [[ -n $BI_PRUNE_KEEP_LAST ]] && args+=(--keep-last "$BI_PRUNE_KEEP_LAST")
  [[ -n $BI_PRUNE_KEEP_WITHIN ]] && args+=(--keep-within "$BI_PRUNE_KEEP_WITHIN")
  [[ -n $BI_PRUNE_TAG_REGEX ]] && args+=(--tag-regex "$BI_PRUNE_TAG_REGEX")
  if [[ -n $BI_PRUNE_KEEP_TAGS ]]; then
    local t
    split_list "$BI_PRUNE_KEEP_TAGS"
    for t in "${_SPLIT[@]}"; do args+=(--keep-tag "$t"); done
  fi
  if [[ -n $BI_PRUNE_EXTRA_ARGS ]]; then
    local -a extra=()
    read -r -a extra <<<"$BI_PRUNE_EXTRA_ARGS"
    args+=("${extra[@]}")
  fi
  [[ -n $BI_REGISTRY_USER ]] && args+=(--registry-user "$BI_REGISTRY_USER")

  log INFO "retention: $BI_BIN ${args[*]}"
  local out rc=0
  out=$("$BI_BIN" "${args[@]}" 2>>"$LOG_FILE") || rc=$?
  printf '%s\n' "$out" >>"$LOG_FILE"
  if ((rc == 0)); then
    local removed
    removed=$(json_field "$out" 'removed')
    [[ -n $removed ]] || removed=$(json_field "$out" 'deleted')
    PRUNE_NOTE="ok${removed:+, ${removed} tag(s) removed}"
    log INFO "retention done ($PRUNE_NOTE)"
  else
    PRUNE_NOTE="FAILED (exit $rc) — the backup itself succeeded"
    log WARN "retention failed with exit code $rc; the backup is unaffected"
  fi
  return 0
}

parse_result() {
  local json=$1
  RESULT_REF=$(json_field "$json" 'ref')
  RESULT_DIGEST=$(json_field "$json" 'digest')
  RESULT_FILES=$(json_field "$json" 'files')
  RESULT_BYTES_RAW=$(json_field "$json" 'bytesRaw')
  RESULT_BYTES_STORED=$(json_field "$json" 'bytesStored')
  RESULT_UPLOADED=$(json_field "$json" 'uploadedBytes')
  RESULT_SKIPPED=$(json_field "$json" 'skippedBytes')
  local d
  d=$(json_field "$json" 'durationSeconds')
  [[ $d =~ ^[0-9]+$ ]] && ELAPSED=$d
  return 0
}

# finish <rc> [detail] — the single exit path: hook, notification, rotation.
finish() {
  local rc=$1 detail=${2:-}
  ELAPSED=${ELAPSED:-$(( $(date +%s) - ${START_EPOCH:-$(date +%s)} ))}

  if [[ -n ${BI_POST_CMD:-} && -n $LOG_FILE ]]; then
    BI_STATUS=$([[ $rc -eq 0 ]] && echo OK || echo FAILED) \
      BI_EXIT_CODE=$rc BI_REF=${RESULT_REF:-} BI_LOG_FILE=$LOG_FILE \
      run_hook post "$BI_POST_CMD" || true
  fi

  if ! $DRY_RUN && ! $TEST_NOTIFY; then
    if ((rc == 0)); then
      notify OK "$detail" || true
    else
      notify FAILED "${detail:-exit code $rc: $(exit_meaning "$rc")}" || true
    fi
  fi

  [[ -n $LOG_FILE ]] && rotate_logs
  if ((rc != 0)) && [[ -n $LOG_FILE && $BI_STDERR != never ]]; then
    printf '%s: job %s FAILED (exit %d: %s); log: %s\n' \
      "$PROGNAME" "${BI_JOB_NAME:-backup}" "$rc" "$(exit_meaning "$rc")" "$LOG_FILE" >&2
    tail -n "${BI_NOTIFY_TAIL:-20}" "$LOG_FILE" >&2 || true
  fi
  exit "$rc"
}

# shellcheck disable=SC2317  # invoked from a trap
on_signal() {
  local sig=$1
  log ERROR "received SIG${sig}: aborting"
  if [[ -n $CHILD_PID ]] && kill -0 "$CHILD_PID" 2>/dev/null; then
    kill -TERM "$CHILD_PID" 2>/dev/null || true
    wait "$CHILD_PID" 2>/dev/null || true
  fi
  finish 7 "interrupted by SIG${sig}"
}

main() {
  local rc=0
  capture_preset_env
  parse_args "$@"
  load_config
  set_defaults
  setup_runtime

  START_EPOCH=$(date +%s)
  STARTED_AT=$(ts)
  trap 'on_signal INT' INT
  trap 'on_signal TERM' TERM

  if $PRINT_CONFIG; then
    print_config
    exit 0
  fi

  # --test-notify exists to prove the webhook works: validate it as if
  # notifications were on, whatever the policy says.
  $TEST_NOTIFY && BI_NOTIFY=always
  validate_config

  if $TEST_NOTIFY; then
    ELAPSED=0
    rc=0
    notify TEST 'test notification from backimage-backup.sh' || rc=1
    exit "$rc"
  fi

  log INFO "$PROGNAME $WRAPPER_VERSION starting job '$BI_JOB_NAME' (config: ${CONFIG_FILE:-<none>})"
  log INFO "description: $BI_DESCRIPTION"
  acquire_lock
  cd "$BI_WORKDIR" || die "cannot cd to BI_WORKDIR '$BI_WORKDIR'"

  build_backup_args
  build_runner

  if [[ -n $BI_PRE_CMD ]]; then
    run_hook pre "$BI_PRE_CMD" || finish 1 'pre-command failed: the backup did not run'
  fi

  log INFO "running: ${RUNNER[*]:-} $BI_BIN ${BACKUP_ARGS[*]}"
  local stdout_file json=''
  rc=0
  stdout_file=$(mktemp "${TMPDIR:-/tmp}/backimage-summary.XXXXXX")
  # shellcheck disable=SC2064
  trap "rm -f '$stdout_file'" EXIT

  set +e
  if ((${#RUNNER[@]} > 0)); then
    "${RUNNER[@]}" "$BI_BIN" "${BACKUP_ARGS[@]}" >"$stdout_file" 2>>"$LOG_FILE" &
  else
    "$BI_BIN" "${BACKUP_ARGS[@]}" >"$stdout_file" 2>>"$LOG_FILE" &
  fi
  CHILD_PID=$!
  wait "$CHILD_PID"
  rc=$?
  set -e
  CHILD_PID=''

  json=$(cat "$stdout_file" 2>/dev/null || true)
  [[ -n $json ]] && printf '%s\n' "$json" >>"$LOG_FILE"

  if ((rc == 0)); then
    parse_result "$json"
    if $DRY_RUN; then
      log INFO 'dry run complete: nothing was written'
      printf '%s\n' "$json"
      finish 0 'dry run'
    fi
    log INFO "backup ok: ${RESULT_REF:-$BI_REPO:$BI_TAG} ($(human_bytes "${RESULT_BYTES_STORED:-0}") stored, $(human_duration "${ELAPSED:-0}"))"
    run_prune
    finish 0
  fi

  log ERROR "backup failed with exit code $rc ($(exit_meaning "$rc"))"
  finish "$rc"
}

main "$@"
