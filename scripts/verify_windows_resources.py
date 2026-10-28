"""Verify the icon, version and manifest embedded in a Windows executable."""

import argparse
import struct
import sys
import xml.etree.ElementTree as ET
from pathlib import Path


def verify(path: Path, version: str) -> None:
    """Check that the built PE contains the expected application resources."""
    data = path.read_bytes()
    pe = struct.unpack_from("<I", data, 0x3C)[0]
    if data[:2] != b"MZ" or data[pe : pe + 4] != b"PE\0\0":
        raise ValueError("not a Windows PE executable")
    sections = struct.unpack_from("<H", data, pe + 6)[0]
    optional_size = struct.unpack_from("<H", data, pe + 20)[0]
    optional = pe + 24
    magic = struct.unpack_from("<H", data, optional)[0]
    if magic not in (0x10B, 0x20B):
        raise ValueError("unsupported PE optional header")
    directories = optional + (112 if magic == 0x20B else 96)
    resource_rva, resource_size = struct.unpack_from("<II", data, directories + 16)
    if not resource_rva or not resource_size:
        raise ValueError("executable has no resource directory")

    def file_offset(rva: int) -> int:
        for index in range(sections):
            section = optional + optional_size + index * 40
            _, address, size, offset = struct.unpack_from("<IIII", data, section + 8)
            if address <= rva < address + size:
                return offset + rva - address
        raise ValueError(f"resource RVA {rva:#x} is outside file sections")

    root = file_offset(resource_rva)

    def entries(relative: int) -> list[tuple[int, int]]:
        named, ids = struct.unpack_from("<HH", data, root + relative + 12)
        return [
            struct.unpack_from("<II", data, root + relative + 16 + index * 8)
            for index in range(named + ids)
        ]

    def contents(relative: int, depth: int = 0) -> list[bytes]:
        if depth > 3:
            raise ValueError("resource directory nesting exceeds PE resource layout")
        result = []
        for _, target in entries(relative):
            if target & 0x80000000:
                result.extend(contents(target & 0x7FFFFFFF, depth + 1))
            else:
                rva, size = struct.unpack_from("<II", data, root + target)
                offset = file_offset(rva)
                if not size or offset + size > len(data):
                    raise ValueError("empty or truncated resource")
                result.append(data[offset : offset + size])
        return result

    resources = {}
    for kind, target in entries(0):
        if target & 0x80000000:
            resources[kind] = contents(target & 0x7FFFFFFF)
    for kind, name in ((3, "icon"), (14, "group icon"), (16, "version"), (24, "manifest")):
        if not resources.get(kind):
            raise ValueError(f"missing embedded {name}")

    version_data = resources[16][0]
    fixed = version_data.find(struct.pack("<I", 0xFEEF04BD))
    if fixed < 0:
        raise ValueError("missing fixed file version")
    expected = tuple(int(part) for part in version.split("."))
    if not 1 <= len(expected) <= 4 or any(part < 0 or part > 0xFFFF for part in expected):
        raise ValueError("version must contain one to four unsigned 16-bit components")
    expected += (0,) * (4 - len(expected))
    major_minor, patch_build = struct.unpack_from("<II", version_data, fixed + 8)
    actual = (
        major_minor >> 16, major_minor & 0xFFFF,
        patch_build >> 16, patch_build & 0xFFFF,
    )
    if actual != expected or (version + "\0").encode("utf-16le") not in version_data:
        raise ValueError(f"embedded file/product version does not match {version}")
    manifest = ET.fromstring(resources[24][0])
    identity = manifest.find("{urn:schemas-microsoft-com:asm.v1}assemblyIdentity")
    # The Windows generator expands semantic versions to four components.
    manifest_version = ".".join(str(part) for part in expected)
    if identity is None or identity.get("version") != manifest_version:
        raise ValueError(f"embedded manifest version does not match {version}")
    print(f"Windows resources verified: icon, version {version}, manifest")


def main() -> int:
    """Return a failing exit code when executable metadata is missing or stale."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("executable", type=Path)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    try:
        verify(args.executable, args.version)
    except (OSError, ValueError, struct.error, ET.ParseError) as error:
        print(f"Windows resource verification failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
