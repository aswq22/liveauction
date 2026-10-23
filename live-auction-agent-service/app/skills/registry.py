"""Progressively discover and load versioned Auction Agent Skills."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import re


_REQUIRED_KEYS = frozenset({"name", "description", "version", "allowed_tools"})
_SKILL_NAME_PATTERN = re.compile(r"[a-z][a-z0-9_]*\Z")
_VERSION_PATTERN = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\Z")
_TOOL_PATTERN = re.compile(r"[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+\Z")
_PHASE_ONE_READ_ONLY_TOOLS = (
    "inventory.query",
    "auction.history.query",
    "session.snapshot.get",
)


class SkillNotFound(LookupError):
    """Raised when a requested Skill name cannot safely be resolved."""


class SkillValidationError(ValueError):
    """Raised when a Skill package does not meet registry requirements."""


@dataclass(frozen=True, slots=True)
class SkillMetadata:
    name: str
    description: str
    version: str
    allowed_tools: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class SkillContent:
    """Loaded text together with its package-relative source path."""

    source: Path
    content: str


@dataclass(frozen=True, slots=True)
class LoadedSkill:
    """A fully loaded Skill package, including source provenance."""

    metadata: SkillMetadata
    instructions: SkillContent
    references: tuple[SkillContent, ...]
    examples: tuple[SkillContent, ...]

    @property
    def name(self) -> str:
        return self.metadata.name

    @property
    def description(self) -> str:
        return self.metadata.description

    @property
    def version(self) -> str:
        return self.metadata.version

    @property
    def allowed_tools(self) -> tuple[str, ...]:
        """Declared capability scope; this property grants no authorization."""
        return self.metadata.allowed_tools


@dataclass(frozen=True, slots=True)
class _DiscoveredSkill:
    metadata: SkillMetadata
    package_dir: Path


class SkillRegistry:
    """Discover metadata first and read full package content only on ``load``."""

    def __init__(self, skill_root: Path | str) -> None:
        root = Path(skill_root)
        if not root.is_dir():
            raise SkillValidationError(f"skill root is not a directory: {root}")
        self._skill_root = root.resolve(strict=True)

    def discover(self) -> list[SkillMetadata]:
        """Return metadata without reading Skill bodies, references, or examples."""
        return [entry.metadata for entry in self._discover_entries()]

    def load(self, name: str) -> LoadedSkill:
        """Load one validated package and preserve each content file's provenance."""
        self._validate_requested_name(name)
        entries = {entry.metadata.name: entry for entry in self._discover_entries()}
        entry = entries.get(name)
        if entry is None:
            raise SkillNotFound(f"skill not found: {name}")

        skill_file = self._safe_path(entry.package_dir / "SKILL.md")
        full_skill_text = self._read_text(skill_file)
        instructions = SkillContent(
            source=Path("SKILL.md"), content=_strip_front_matter(full_skill_text)
        )
        return LoadedSkill(
            metadata=entry.metadata,
            instructions=instructions,
            references=self._load_content_group(entry.package_dir, "references"),
            examples=self._load_content_group(entry.package_dir, "examples"),
        )

    def _discover_entries(self) -> list[_DiscoveredSkill]:
        entries: list[_DiscoveredSkill] = []
        names: set[str] = set()
        for candidate in sorted(self._skill_root.iterdir(), key=lambda item: item.name):
            if candidate.is_symlink():
                package_dir = self._safe_path(candidate)
            elif not candidate.is_dir():
                continue
            else:
                package_dir = self._safe_path(candidate)
            skill_file = package_dir / "SKILL.md"
            if not skill_file.is_file():
                raise SkillValidationError(f"skill package is missing SKILL.md: {candidate.name}")
            metadata = _parse_metadata(self._read_front_matter(self._safe_path(skill_file)))
            if metadata.name in names:
                raise SkillValidationError(f"duplicate skill name: {metadata.name}")
            names.add(metadata.name)
            entries.append(_DiscoveredSkill(metadata=metadata, package_dir=package_dir))
        return entries

    def _load_content_group(self, package_dir: Path, directory: str) -> tuple[SkillContent, ...]:
        group_dir = package_dir / directory
        if not group_dir.exists():
            return ()
        group_dir = self._safe_package_path(group_dir, package_dir)
        if not group_dir.is_dir():
            raise SkillValidationError(f"skill {directory} path is not a directory: {group_dir}")

        content: list[SkillContent] = []
        for candidate in sorted(group_dir.rglob("*"), key=lambda item: item.as_posix()):
            if candidate.is_symlink():
                safe_file = self._safe_package_path(candidate, package_dir)
                if safe_file.is_dir():
                    continue
            elif candidate.is_dir():
                continue
            else:
                safe_file = self._safe_package_path(candidate, package_dir)
            content.append(
                SkillContent(
                    source=safe_file.relative_to(package_dir),
                    content=self._read_text(safe_file),
                )
            )
        return tuple(content)

    def _safe_path(self, path: Path) -> Path:
        try:
            resolved = path.resolve(strict=True)
            resolved.relative_to(self._skill_root)
        except (FileNotFoundError, OSError, ValueError) as error:
            raise SkillValidationError(f"skill path escapes skill root: {path}") from error
        return resolved

    def _safe_package_path(self, path: Path, package_dir: Path) -> Path:
        resolved = self._safe_path(path)
        try:
            resolved.relative_to(package_dir)
        except ValueError as error:
            raise SkillValidationError(f"skill path escapes skill package: {path}") from error
        return resolved

    @staticmethod
    def _read_front_matter(path: Path) -> list[str]:
        """Read only front-matter lines; bodies remain unread until ``load``."""
        lines: list[str] = []
        with path.open("rb", buffering=0) as skill_file:
            first_line = _decode_line(skill_file.readline(), path)
            if first_line != "---":
                raise SkillValidationError(f"SKILL.md must start with front matter: {path}")
            while True:
                raw_line = skill_file.readline()
                if raw_line == b"":
                    raise SkillValidationError(f"SKILL.md front matter is not closed: {path}")
                line = _decode_line(raw_line, path)
                if line == "---":
                    return lines
                lines.append(line)

    @staticmethod
    def _read_text(path: Path) -> str:
        try:
            return path.read_text(encoding="utf-8")
        except UnicodeDecodeError as error:
            raise SkillValidationError(f"skill content is not UTF-8: {path}") from error

    @staticmethod
    def _validate_requested_name(name: str) -> None:
        if not isinstance(name, str) or not _SKILL_NAME_PATTERN.fullmatch(name):
            raise SkillNotFound(f"unsafe skill name: {name!r}")


def _decode_line(raw_line: bytes, path: Path) -> str:
    try:
        return raw_line.decode("utf-8").rstrip("\r\n")
    except UnicodeDecodeError as error:
        raise SkillValidationError(f"SKILL.md front matter is not UTF-8: {path}") from error


def _parse_metadata(lines: list[str]) -> SkillMetadata:
    values: dict[str, str] = {}
    tools: list[str] = []
    reading_tools = False

    for line in lines:
        if line.startswith("  - "):
            if not reading_tools:
                raise SkillValidationError("allowed_tools entries must follow allowed_tools")
            tool = line[4:]
            if not tool or not _TOOL_PATTERN.fullmatch(tool):
                raise SkillValidationError(f"invalid allowed_tools entry: {tool!r}")
            tools.append(tool)
            continue

        if line.startswith((" ", "\t", "-")) or ":" not in line:
            raise SkillValidationError(f"invalid front matter line: {line!r}")
        key, value = line.split(":", 1)
        if key not in _REQUIRED_KEYS:
            raise SkillValidationError(f"unknown front matter key: {key!r}")
        if key in values:
            raise SkillValidationError(f"duplicate front matter key: {key}")
        if key == "allowed_tools":
            if value:
                raise SkillValidationError("allowed_tools must be a block list")
            values[key] = ""
            reading_tools = True
            continue
        if not value.startswith(" ") or not value.strip():
            raise SkillValidationError(f"invalid value for {key}")
        values[key] = value.strip()
        reading_tools = False

    missing = _REQUIRED_KEYS.difference(values)
    if missing:
        raise SkillValidationError(f"missing required front matter keys: {', '.join(sorted(missing))}")
    if not _SKILL_NAME_PATTERN.fullmatch(values["name"]):
        raise SkillValidationError(f"invalid skill name: {values['name']!r}")
    if not _VERSION_PATTERN.fullmatch(values["version"]):
        raise SkillValidationError(f"invalid skill version: {values['version']!r}")
    if len(tools) != len(set(tools)) or set(tools) != set(_PHASE_ONE_READ_ONLY_TOOLS):
        raise SkillValidationError(
            "allowed_tools must contain exactly the phase-1 read-only tools"
        )
    return SkillMetadata(
        name=values["name"],
        description=values["description"],
        version=values["version"],
        allowed_tools=tuple(tools),
    )


def _strip_front_matter(full_text: str) -> str:
    lines = full_text.splitlines(keepends=True)
    if not lines or lines[0].rstrip("\r\n") != "---":
        raise SkillValidationError("SKILL.md must start with front matter")
    for index, line in enumerate(lines[1:], start=1):
        if line.rstrip("\r\n") == "---":
            return "".join(lines[index + 1 :])
    raise SkillValidationError("SKILL.md front matter is not closed")
