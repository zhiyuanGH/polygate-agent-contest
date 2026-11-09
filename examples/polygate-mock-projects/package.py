"""Build deterministic ZIPs with project files at each archive root."""

from pathlib import Path
from shutil import copyfile
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo


ROOT = Path(__file__).resolve().parent
DIST = ROOT / "dist"
DIST.mkdir(exist_ok=True)
WEB_PUBLIC = ROOT.parents[1] / "web" / "public"
WEB_PUBLIC.mkdir(exist_ok=True)
for project in ("mock-agent-python", "mock-counter-node", "mock-inspector-dockerfile"):
    archive = DIST / (project + ".zip")
    with ZipFile(archive, "w", ZIP_DEFLATED, compresslevel=9) as output:
        for source in sorted((ROOT / project).iterdir()):
            if not source.is_file():
                continue
            info = ZipInfo(source.name, date_time=(2026, 11, 9, 0, 0, 0))
            info.compress_type = ZIP_DEFLATED
            info.external_attr = 0o644 << 16
            output.writestr(info, source.read_bytes(), compress_type=ZIP_DEFLATED, compresslevel=9)
    copyfile(archive, WEB_PUBLIC / archive.name)
    print(archive.relative_to(ROOT), archive.stat().st_size, "bytes")
