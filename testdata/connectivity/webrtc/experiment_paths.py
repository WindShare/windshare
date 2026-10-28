"""Resolve experiment scratch directories that Syncthing must not replicate.

The Syncthing folder that contains this repository ignores every basename
matching ``*_stignore`` or ``stignore_*``.  Experiment runs write multi-GB
snapshots, fixtures and traces, so their directories carry the suffix from the
moment they are created.  Tooling and configuration under ``.tmp`` deliberately
stay on ordinary names and remain synced.
"""
from pathlib import Path

IGNORED_DIRECTORY_SUFFIX = "_stignore"


def experiment_name(name: str) -> str:
    """Return the on-disk basename that Syncthing treats as ignored."""
    if not name:
        raise ValueError("experiment name is required")
    return name if name.endswith(IGNORED_DIRECTORY_SUFFIX) else name + IGNORED_DIRECTORY_SUFFIX


def experiment_label(name: str) -> str:
    """Return the logical experiment name recorded in exported evidence."""
    return name[:-len(IGNORED_DIRECTORY_SUFFIX)] if name.endswith(IGNORED_DIRECTORY_SUFFIX) else name


def experiment_directory(root: Path, name: str) -> Path:
    """Return ``<repo>/.tmp/<name>_stignore`` for a logical experiment name."""
    return root / ".tmp" / experiment_name(name)
