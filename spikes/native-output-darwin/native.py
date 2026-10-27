"""Darwin ABI observations for the isolated output experiment, not production ports."""

import ctypes
import errno
import os
import platform
import struct
import uuid

# Values and packed layouts come from XNU sys/attr.h and getattrlist(2).
ATTR_RETURNED = 0x80000000
ATTR_FILE_ID = 0x02000000
ATTR_CREATED = 0x00000200
ATTR_WRITE_GENERATION = 0x00080000
ATTR_VOLUME_INFO = 0x80000000
ATTR_VOLUME_UUID = 0x00040000
ATTR_VOLUME_CAPABILITIES = 0x00020000
EXTENDED_ATTRIBUTES = 0x20
RENAME_EXCLUSIVE = 0x4
RENAME_NOFOLLOW_ANY = 0x10
FULL_FSYNC = 51
ATTRIBUTE_HEADER_BYTES = 24
ATTRIBUTE_BUFFER_BYTES = 256
PRIVATE_FILE_MODE = 0o600
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC

FORMAT_BITS = {
    "persistent_object_ids": 0x1,
    "hard_links": 0x4,
    "case_sensitive": 0x100,
    "path_from_id_nonrecycled": 0x4000,
    "object_ids_64bit": 0x20000,
    "write_generation_count": 0x100000,
}
INTERFACE_BITS = {"flock": 0x200, "exclusive_rename": 0x80000}


class AttributeList(ctypes.Structure):
    _fields_ = [
        ("bitmapcount", ctypes.c_uint16), ("reserved", ctypes.c_uint16),
        ("common", ctypes.c_uint32), ("volume", ctypes.c_uint32),
        ("directory", ctypes.c_uint32), ("file", ctypes.c_uint32),
        ("fork", ctypes.c_uint32),
    ]


libc = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
libc.fgetattrlist.argtypes = [
    ctypes.c_int, ctypes.POINTER(AttributeList), ctypes.c_void_p,
    ctypes.c_size_t, ctypes.c_ulong,
]
libc.fgetattrlist.restype = ctypes.c_int
libc.renameatx_np.argtypes = [
    ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint,
]
libc.renameatx_np.restype = ctypes.c_int


def checked(result):
    if result != 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code))


def attributes(fd, *, common=0, volume=0, extended=0):
    request = AttributeList(5, 0, common | ATTR_RETURNED, volume, 0, 0, extended)
    buffer = ctypes.create_string_buffer(ATTRIBUTE_BUFFER_BYTES)
    checked(libc.fgetattrlist(
        fd, ctypes.byref(request), buffer, len(buffer),
        EXTENDED_ATTRIBUTES if extended else 0,
    ))
    size, got_common, got_volume, _, _, got_extended = struct.unpack_from("=6I", buffer)
    if not ATTRIBUTE_HEADER_BYTES <= size <= len(buffer):
        raise ValueError("invalid attribute response length")
    if common & got_common != common or extended & got_extended != extended:
        raise OSError(errno.ENOTSUP, "requested object attribute was not returned")
    wanted_volume = volume & ~ATTR_VOLUME_INFO
    if wanted_volume & got_volume != wanted_volume:
        raise OSError(errno.ENOTSUP, "requested volume attribute was not returned")
    return buffer.raw[ATTRIBUTE_HEADER_BYTES:size]


class FilesystemStat(ctypes.Structure):
    # Mirrors the statfs64 ABI used by the pinned Go Darwin bindings.
    _fields_ = [
        ("block_size", ctypes.c_uint32), ("io_size", ctypes.c_int32),
        ("blocks", ctypes.c_uint64), ("free", ctypes.c_uint64),
        ("available", ctypes.c_uint64), ("files", ctypes.c_uint64),
        ("free_files", ctypes.c_uint64), ("fsid", ctypes.c_int32 * 2),
        ("owner", ctypes.c_uint32), ("type", ctypes.c_uint32),
        ("flags", ctypes.c_uint32), ("subtype", ctypes.c_uint32),
        ("type_name", ctypes.c_char * 16), ("mount", ctypes.c_char * 1024),
        ("device", ctypes.c_char * 1024), ("extended_flags", ctypes.c_uint32),
        ("reserved", ctypes.c_uint32 * 7),
    ]


def filesystem(fd):
    call = libc.fstatfs if platform.machine() == "arm64" else libc.fstatfs64
    call.argtypes = [ctypes.c_int, ctypes.POINTER(FilesystemStat)]
    call.restype = ctypes.c_int
    stat = FilesystemStat()
    checked(call(fd, ctypes.byref(stat)))
    return {"filesystem": stat.type_name.decode(), "mount_point": os.fsdecode(stat.mount),
            "runtime_fsid": list(stat.fsid)}


def volume_uuid(fd):
    value = attributes(fd, volume=ATTR_VOLUME_INFO | ATTR_VOLUME_UUID)
    if len(value) != 16 or not any(value):
        raise ValueError("missing or malformed volume UUID")
    return str(uuid.UUID(bytes=value))


def identity(fd):
    file_id, = struct.unpack("=Q", attributes(fd, common=ATTR_FILE_ID))
    created = list(struct.unpack("=qq", attributes(fd, common=ATTR_CREATED)))
    stat = os.fstat(fd)
    if file_id != stat.st_ino:
        raise ValueError("attribute file ID disagrees with the open handle")
    return {"volume_uuid": volume_uuid(fd), "file_id": file_id, "created": created}


def volume_capabilities(fd):
    values = struct.unpack("=8I", attributes(
        fd, volume=ATTR_VOLUME_INFO | ATTR_VOLUME_CAPABILITIES,
    ))
    facts = {}
    for index, bits in enumerate((FORMAT_BITS, INTERFACE_BITS)):
        for name, bit in bits.items():
            facts[name] = {
                "valid": bool(values[index + 4] & bit),
                "supported": bool(values[index] & bit),
            }
    return facts


def mutation_observation(fd):
    stat = os.fstat(fd)
    try:
        generation, = struct.unpack("=I", attributes(fd, extended=ATTR_WRITE_GENERATION))
        generation_error = None
    except OSError as error:
        generation, generation_error = None, error.errno
    # st_gen and write-generation have different meanings; neither is silently
    # promoted into an incarnation identifier by this experiment.
    return {
        "stat_generation": getattr(stat, "st_gen", None),
        "write_generation": generation,
        "write_generation_errno": generation_error,
        "mtime_ns": stat.st_mtime_ns, "ctime_ns": stat.st_ctime_ns,
    }


def rename_exclusive(root, source, destination):
    checked(libc.renameatx_np(
        root, os.fsencode(source), root, os.fsencode(destination),
        RENAME_EXCLUSIVE | RENAME_NOFOLLOW_ANY,
    ))


def open_directory(path):
    return os.open(path, DIRECTORY_FLAGS)


def open_file(root, name, flags=os.O_RDWR):
    return os.open(
        name, flags | os.O_NOFOLLOW | os.O_CLOEXEC,
        PRIVATE_FILE_MODE, dir_fd=root,
    )


def write_at(fd, data, offset=0):
    while data:
        count = os.pwrite(fd, data, offset)
        if count <= 0:
            raise OSError(errno.EIO, "write made no progress")
        offset += count
        data = data[count:]


def write_record(root, record):
    import json
    fd = open_file(root, "checkpoint.tmp", os.O_WRONLY | os.O_CREAT | os.O_TRUNC)
    try:
        write_at(fd, json.dumps(record, sort_keys=True).encode())
        os.fsync(fd)
    finally:
        os.close(fd)


def install_record(root):
    os.replace("checkpoint.tmp", "checkpoint.json", src_dir_fd=root, dst_dir_fd=root)
    os.fsync(root)
