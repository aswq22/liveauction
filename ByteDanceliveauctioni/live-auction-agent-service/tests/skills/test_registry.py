from __future__ import annotations

import os
from pathlib import Path
import subprocess

import pytest

from app.domain.models import AGENT_DECISION_ADAPTER, FinalProposalDecision
from app.skills.registry import SkillNotFound, SkillRegistry, SkillValidationError


PHASE_ONE_TOOLS = (
    "inventory.query",
    "auction.history.query",
    "session.snapshot.get",
)


@pytest.fixture
def skill_root() -> Path:
    return Path(__file__).parents[2] / "app" / "skills" / "packages"


def write_skill(root: Path, package: str, front_matter: str) -> Path:
    skill_dir = root / package
    skill_dir.mkdir(parents=True)
    skill_file = skill_dir / "SKILL.md"
    skill_file.write_text(f"---\n{front_matter}\n---\n\nInstruction body.\n", encoding="utf-8")
    return skill_file


def valid_front_matter(**overrides: str) -> str:
    fields = {
        "name": "test_skill",
        "description": "A test skill",
        "version": "1.2.3",
        "allowed_tools": "  - inventory.query\n  - auction.history.query\n  - session.snapshot.get",
    }
    fields.update(overrides)
    return "\n".join(
        (
            f"name: {fields['name']}",
            f"description: {fields['description']}",
            f"version: {fields['version']}",
            "allowed_tools:",
            fields["allowed_tools"],
        )
    )


def test_registry_reads_metadata_before_full_body(skill_root: Path):
    registry = SkillRegistry(skill_root)

    metadata = registry.discover()[0]

    assert metadata.name == "plan_live_session"
    assert "pricing-policy.md" not in metadata.description

    loaded = registry.load(metadata.name)

    assert loaded.allowed_tools == PHASE_ONE_TOOLS


def test_registry_defers_invalid_instruction_body_decoding_until_load(tmp_path: Path):
    skill_file = write_skill(tmp_path, "test_skill", valid_front_matter())
    skill_file.write_bytes(
        b"---\n"
        b"name: test_skill\n"
        b"description: A test skill\n"
        b"version: 1.2.3\n"
        b"allowed_tools:\n"
        b"  - inventory.query\n"
        b"  - auction.history.query\n"
        b"  - session.snapshot.get\n"
        b"---\n"
        b"\xff"
    )
    registry = SkillRegistry(tmp_path)

    assert registry.discover()[0].name == "test_skill"
    with pytest.raises(SkillValidationError, match="content is not UTF-8"):
        registry.load("test_skill")


def test_registry_load_preserves_instruction_reference_and_example_provenance(skill_root: Path):
    loaded = SkillRegistry(skill_root).load("plan_live_session")

    assert loaded.instructions.source == Path("SKILL.md")
    assert "只引用工具返回的 lot ID" in loaded.instructions.content
    assert [(item.source, item.content) for item in loaded.references] == [
        (Path("references/pricing-policy.md"), loaded.references[0].content)
    ]
    assert "建议的起拍价" in loaded.references[0].content
    assert [(item.source, item.content) for item in loaded.examples] == [
        (Path("examples/balanced.json"), loaded.examples[0].content)
    ]
    example = AGENT_DECISION_ADAPTER.validate_json(loaded.examples[0].content)
    assert isinstance(example, FinalProposalDecision)
    assert example.proposal.strategy == "balanced"
    assert example.proposal.evidence
    assert example.proposal.risks


@pytest.mark.parametrize("unsafe_name", ("../secret", "/secret", "\\secret", "plan/live", "plan\\live", ".."))
def test_registry_rejects_path_traversal(skill_root: Path, unsafe_name: str):
    with pytest.raises(SkillNotFound, match="unsafe"):
        SkillRegistry(skill_root).load(unsafe_name)


def test_registry_rejects_unknown_name(skill_root: Path):
    with pytest.raises(SkillNotFound, match="not found"):
        SkillRegistry(skill_root).load("missing_skill")


@pytest.mark.parametrize(
    ("front_matter", "message"),
    (
        ("name: test_skill\ndescription: A test skill\nversion: 1.2.3", "allowed_tools"),
        (
            "name: test_skill\nname: duplicate\ndescription: A test skill\nversion: 1.2.3\nallowed_tools:\n  - inventory.query\n  - auction.history.query\n  - session.snapshot.get",
            "duplicate",
        ),
        (valid_front_matter(version="1.2"), "version"),
        (valid_front_matter(allowed_tools="  - inventory.query"), "allowed_tools"),
        (valid_front_matter(allowed_tools="  - malformed"), "invalid allowed_tools"),
        (valid_front_matter(name="../test_skill"), "name"),
    ),
)
def test_registry_rejects_malformed_required_metadata(
    tmp_path: Path, front_matter: str, message: str
):
    write_skill(tmp_path, "test_skill", front_matter)

    with pytest.raises(SkillValidationError, match=message):
        SkillRegistry(tmp_path).discover()


def test_registry_rejects_duplicate_skill_names(tmp_path: Path):
    write_skill(tmp_path, "first", valid_front_matter(name="same_name"))
    write_skill(tmp_path, "second", valid_front_matter(name="same_name"))

    with pytest.raises(SkillValidationError, match="duplicate skill name"):
        SkillRegistry(tmp_path).discover()


def test_registry_refuses_package_symlink_outside_skill_root(tmp_path: Path):
    outside = tmp_path.parent / "outside-skill"
    write_skill(outside, "external", valid_front_matter(name="external"))
    link = tmp_path / "linked_skill"
    try:
        os.symlink(outside / "external", link, target_is_directory=True)
    except OSError as error:
        if os.name != "nt":
            pytest.skip(f"symlinks unavailable: {error}")
        junction = subprocess.run(
            ["cmd", "/c", "mklink", "/J", str(link), str(outside / "external")],
            capture_output=True,
            text=True,
            check=False,
        )
        if junction.returncode:
            pytest.skip(f"symlinks and junctions unavailable: {error}")

    with pytest.raises(SkillValidationError, match="escapes skill root"):
        SkillRegistry(tmp_path).discover()


def test_registry_refuses_linked_reference_outside_owning_package(tmp_path: Path):
    owner = write_skill(tmp_path, "owner", valid_front_matter(name="owner"))
    other = write_skill(tmp_path, "other", valid_front_matter(name="other"))
    references = owner.parent / "references"
    try:
        os.symlink(other.parent, references, target_is_directory=True)
    except OSError as error:
        if os.name != "nt":
            pytest.skip(f"symlinks unavailable: {error}")
        junction = subprocess.run(
            ["cmd", "/c", "mklink", "/J", str(references), str(other.parent)],
            capture_output=True,
            text=True,
            check=False,
        )
        if junction.returncode:
            pytest.skip(f"symlinks and junctions unavailable: {error}")

    with pytest.raises(SkillValidationError, match="escapes skill package"):
        SkillRegistry(tmp_path).load("owner")
