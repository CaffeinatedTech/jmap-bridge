#!/bin/bash
# Cooldown watcher: probe Gmail's throttle with one tiny session per
# cycle; when a CREATE sticks, run the M4 gate and record the result.
RESULT="$(dirname "$0")/m4gate-result.txt"
: > "$RESULT"
for i in $(seq 1 36); do
  echo "$(date +%H:%M:%S) probe $i" >> /tmp/opencode/m4-watch.log
  python3 - <<'PY' >> /tmp/opencode/m4-watch.log 2>&1
import sys, time
sys.argv=['x']
exec(open('/tmp/opencode/m4gate.py').read().split('def main()')[0])
try:
    M = imap()
    typ, data = M.create('"jmapprobe-w"')
    time.sleep(4)
    ok = 'jmapprobe-w' in gmail_labels(M)
    try: M.delete('"jmapprobe-w"')
    except Exception: pass
    try: M.logout()
    except Exception: pass
    print("create-stuck:", ok)
    sys.exit(0 if ok else 3)
except Exception as e:
    print("probe error:", type(e).__name__, str(e)[:100])
    sys.exit(3)
PY
  if [ $? -eq 0 ]; then
    echo "$(date +%H:%M:%S) throttle lifted; starting bridge + gate" >> /tmp/opencode/m4-watch.log
    bash "$(dirname "$0")/gmail-start.sh"
    sleep 20
    python3 /tmp/opencode/m4gate.py >> "$RESULT" 2>&1
    echo "GATE-EXIT:$?" >> "$RESULT"
    exit 0
  fi
  sleep 600
done
echo "EXHAUSTED after 36 cycles" >> "$RESULT"
