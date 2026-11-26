#!/bin/sh
python3 - <<'PY'
from pathlib import Path
e = Path("/proc/1/environ").read_bytes().split(b"\0")
keys = [x.decode(errors="replace") for x in e if b"MEDIA" in x or b"BENCH" in x]
print("pid1_media_keys:", keys or ["NO_MEDIA_KEYS"])
# also kamailio workers
import os, glob
for p in glob.glob("/proc/[0-9]*/comm"):
    try:
        if open(p).read().strip() != "kamailio":
            continue
        pid = p.split("/")[2]
        env = Path(f"/proc/{pid}/environ").read_bytes().split(b"\0")
        m = [x.decode(errors="replace") for x in env if b"MEDIA" in x]
        print(f"kamailio:{pid}", m or ["UNSET(good)"])
    except Exception as ex:
        pass
PY
