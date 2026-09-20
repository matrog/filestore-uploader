#!/usr/bin/env bash
# filestore - carica file su FileStore.me tramite la sua API HTTP.
# Sostituisce l'upload via FTP, che il servizio ha dismesso.
#
#   filestore setup             salva la API key (una volta sola)
#   filestore check             verifica chiave e stato account
#   filestore up FILE [FILE...] carica e stampa i link da condividere
#   filestore debug             mostra la risposta grezza dell-API
#
# Protocollo: XFileSharing Pro API - https://xfilesharingpro.docs.apiary.io

set -uo pipefail

API="https://filestore.me/api"
SITE="https://filestore.me"
CONF="${FILESTORE_CONF:-$HOME/.filestore.conf}"
KEY=""

die()  { printf 'Errore: %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*" >&2; }

# Gli helper Python girano con -c, cosi stdin resta libero per il JSON.

PY_ACCOUNT='
import sys, json
raw = sys.stdin.read().strip()
if not raw:
    sys.stderr.write("nessuna risposta da filestore.me\n"); sys.exit(2)
try:
    d = json.loads(raw)
except Exception:
    sys.stderr.write("risposta non valida:\n" + raw[:300] + "\n"); sys.exit(2)
if d.get("status") != 200:
    sys.stderr.write("API key rifiutata: " + str(d.get("msg")) + "\n"); sys.exit(1)
r = d.get("result") or {}
def human(v):
    try:
        n = float(v)
    except (TypeError, ValueError):
        return v
    for u in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024:
            return "%.1f %s" % (n, u)
        n /= 1024
    return "%.1f PB" % n
print("Account OK")
rows = (("email", "email", False), ("premium", "premium", False),
        ("scadenza premium", "premium_expire", False),
        ("spazio usato", "storage_used", True),
        ("spazio disponibile", "storage_left", True),
        ("credito", "balance", False))
for label, k, is_size in rows:
    v = r.get(k)
    if v not in (None, ""):
        print("  %-20s %s" % (label + ":", human(v) if is_size else v))
'

PY_UPSRV='
import sys, json
from urllib.parse import urlsplit, parse_qs
raw = sys.stdin.read().strip()
if not raw:
    sys.stderr.write("nessuna risposta dal server\n"); sys.exit(2)
try:
    d = json.loads(raw)
except Exception:
    sys.stderr.write("risposta non valida dal server:\n" + raw[:300] + "\n"); sys.exit(2)
if not isinstance(d, dict):
    sys.stderr.write("struttura inattesa:\n" + raw[:300] + "\n"); sys.exit(2)
if d.get("status") not in (200, "200"):
    sys.stderr.write("API: " + str(d.get("msg")) + " (status " + str(d.get("status")) + ")\n")
    sys.exit(1)
res = d.get("result")
url = ""
sess = ""
if isinstance(res, str):
    url = res.strip()
elif isinstance(res, dict):
    for k in ("upload_url", "url", "server"):
        if isinstance(res.get(k), str) and res[k].startswith("http"):
            url = res[k].strip()
            break
    sess = res.get("sess_id") or res.get("sess") or ""
elif isinstance(res, list) and res and isinstance(res[0], str):
    url = res[0].strip()
# Il sess_id arriva come campo di primo livello nelle installazioni recenti.
if not sess:
    sess = d.get("sess_id") or d.get("sess") or ""
if not url.startswith("http"):
    sys.stderr.write("endpoint di upload non trovato. Risposta grezza:\n" + raw[:400] + "\n")
    sys.exit(2)
if not sess:
    q = parse_qs(urlsplit(url).query)
    sess = (q.get("sess_id") or q.get("sess") or [""])[0]
url = url.split("?")[0]
print(url)
print(sess)
'

PY_UPLOAD='
import sys, json, re
site = sys.argv[1]
raw = sys.stdin.read().strip()
if not raw:
    sys.stderr.write("il server di upload non ha risposto\n"); sys.exit(2)
items = None
try:
    d = json.loads(raw)
    items = d if isinstance(d, list) else [d]
except Exception:
    codes = re.findall(r"file_code[=:\"\s]+([0-9a-zA-Z]{6,30})", raw)
    if codes:
        items = [{"file_status": "OK", "file_code": c} for c in codes]
    else:
        sys.stderr.write("risposta di upload non interpretabile:\n" + raw[:400] + "\n")
        sys.exit(2)
rc = 0
for it in items:
    if not isinstance(it, dict):
        rc = 1
        sys.stderr.write("voce inattesa: " + str(it) + "\n")
        continue
    st = str(it.get("file_status", "")).upper()
    if it.get("file_code") and st in ("OK", ""):
        print(site + "/" + it["file_code"])
    else:
        rc = 1
        sys.stderr.write("non caricato: " + str(it.get("file_status") or it) + "\n")
sys.exit(rc)
'

load_key() {
  if [ -n "${FILESTORE_API_KEY:-}" ]; then
    KEY="$FILESTORE_API_KEY"
  elif [ -f "$CONF" ]; then
    KEY=$(sed -n 's/^[[:space:]]*FILESTORE_API_KEY[[:space:]]*=[[:space:]]*//p' "$CONF" \
          | tr -d '\042\047' | head -1)
  fi
  [ -n "$KEY" ] || die "API key non configurata. Esegui:  $(basename "$0") setup"
}

api_get() {
  local ep="$1"; shift
  curl -sS -m 60 -G "$API/$ep" --data-urlencode "key=$KEY" "$@"
}

cmd_setup() {
  info "API key dal pannello account: $SITE/?op=my_account"
  printf 'Incolla la API key: ' >&2
  local k; read -r k
  k=$(printf '%s' "$k" | tr -d '[:space:]')
  [ -n "$k" ] || die "nessuna chiave inserita."
  umask 077
  printf 'FILESTORE_API_KEY=%s\n' "$k" > "$CONF"
  chmod 600 "$CONF"
  info "Salvata in $CONF (permessi 600: leggibile solo da te)."
  info ""
  KEY="$k"
  cmd_check
}

cmd_check() {
  [ -n "$KEY" ] || load_key
  api_get account/info | python3 -c "$PY_ACCOUNT"
}

cmd_debug() {
  load_key
  info "Risposta di /api/upload/server (la key e' mascherata):"
  api_get upload/server | sed "s/$KEY/<LA-TUA-KEY>/g"
  printf '\n'
}

upload_one() {
  local f="$1" srv url sess out rc size
  srv=$(api_get upload/server | python3 -c "$PY_UPSRV") || return 1
  url=$(printf '%s\n' "$srv" | sed -n '1p')
  sess=$(printf '%s\n' "$srv" | sed -n '2p')
  # Se l-installazione non manda un sess_id, il blueprint XFS usa la API key.
  [ -n "$sess" ] || sess="$KEY"

  size=$(du -h "$f" | cut -f1 | tr -d '[:space:]')
  info "Carico $(basename "$f") ($size) -> ${url#http*://}"

  out=$(curl -sS --progress-bar -m 7200 -F "sess_id=$sess" -F "file=@$f" "$url")
  rc=$?
  [ $rc -eq 0 ] || { info "upload fallito (errore di rete, curl $rc)."; return 1; }

  printf '%s' "$out" | python3 -c "$PY_UPLOAD" "$SITE"
}

cmd_up() {
  [ $# -ge 1 ] || die "uso: $(basename "$0") up FILE [FILE...]"
  load_key
  local f rc=0
  for f in "$@"; do
    if [ ! -f "$f" ]; then
      info "salto '$f': non e' un file."
      rc=1
      continue
    fi
    upload_one "$f" || rc=1
  done
  return $rc
}

usage() { sed -n '2,10p' "$0" | sed 's/^#[[:space:]]\{0,1\}//'; }

case "${1:-}" in
  setup)             shift; cmd_setup "$@" ;;
  check)             shift; cmd_check "$@" ;;
  debug)             shift; cmd_debug "$@" ;;
  up|upload)         shift; cmd_up "$@" ;;
  ""|-h|--help|help) usage ;;
  *)                 die "comando sconosciuto: $1 (prova: $(basename "$0") help)" ;;
esac
