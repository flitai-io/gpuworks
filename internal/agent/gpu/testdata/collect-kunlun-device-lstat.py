"""Read-only device metadata collector. Run on the target host with python3 -.

No credentials or host addresses are embedded here. Output is bounded JSON.
"""
import datetime
import glob
import json
import os
import stat

out = {
    "method": "os.lstat",
    "captured_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "devices": {},
    "sysfs": {},
}
for path in sorted(glob.glob("/dev/xpu*") + glob.glob("/dev/xdr*")):
    try:
        info = os.lstat(path)
        out["devices"][path] = {
            "mode_octal": oct(info.st_mode),
            "file_type": "char" if stat.S_ISCHR(info.st_mode) else "symlink" if stat.S_ISLNK(info.st_mode) else "other",
            "rdev": info.st_rdev,
        }
    except OSError as error:
        out["devices"][path] = {"error": str(error)}
path = "/sys/class/kunlun/xpuctrl/dev"
try:
    with open(path, encoding="utf-8") as source:
        out["sysfs"][path] = source.read(100).strip()
except OSError as error:
    out["sysfs"][path] = {"error": str(error)}
print(json.dumps(out, sort_keys=True))
