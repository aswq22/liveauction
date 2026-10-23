"""Deterministic grounding for user-authored lot draft facts.

The model may suggest optional copy, but identifiers and explicit commerce values are
derived outside the model so every provider produces the same executable contract.
"""

from __future__ import annotations

from decimal import Decimal
import hashlib
import re

from app.domain.models import CandidatePatch, RequiredInput, TurnResult


EXECUTABLE_LOT_FIELDS = frozenset(
    {
        "title",
        "startPriceAmount",
        "durationSeconds",
        "minIncrementAmount",
        "capPriceAmount",
        "category",
        "description",
        "tags",
        "stock",
        "antiSnipeWindowSeconds",
        "antiSnipeExtendSeconds",
        "maxExtendCount",
        "depositAmount",
    }
)
_INITIAL_MODEL_COPY_FIELDS = frozenset({"category", "description", "tags"})


def candidate_id_for_run(run_id: str) -> str:
    scope = run_id.strip()
    if not scope:
        return "candidate-1"
    digest = hashlib.sha256(f"lot-draft:{scope}".encode()).hexdigest()[:24]
    return f"candidate-{digest}"


def explicit_lot_fields(message: str) -> dict:
    fields: dict[str, object] = {}
    quoted_title = re.search(
        r"(?:名为|名称\s*(?:是|为)?)\s*[“\"']([^”\"']+)[”\"']", message
    )
    named_title = re.search(r"(?:名为|名称\s*(?:是|为)?)\s*([^，,。]+)", message)
    if quoted_title:
        title = quoted_title.group(1).strip()
        if title:
            fields["title"] = title
    elif named_title:
        raw_title = named_title.group(1).strip()
        title, separator, product_type = raw_title.rpartition("的")
        title = title.strip() if separator and product_type.strip() else raw_title
        if title:
            fields["title"] = title
    else:
        action_title = re.search(r"(?:拍卖|上架)\s*([^，,。]+)", message)
        if action_title:
            title = re.sub(
                r"的(?:宝石|拍品|商品|物品)$", "", action_title.group(1).strip()
            )
            if title:
                fields["title"] = title
        else:
            title_match = re.match(r"\s*([^，,。]+?)\s*(?:[，,。]|$)", message)
            if title_match and not _is_copy_edit_request(message) and not re.search(
                r"价格|起拍|时长|持续|封顶|"
                r"(?:今天|明天|后天|上午|下午|中午|晚上|凌晨)?\s*\d{1,2}\s*[点时]",
                title_match.group(1),
            ):
                title = re.sub(
                    r"^(?:我(?:要|想)?|请(?:帮我)?)?\s*(?:拍卖|上架)\s*",
                    "",
                    title_match.group(1).strip(),
                ).strip()
                if title:
                    fields["title"] = title
    start_price = _money_cents(message, r"(?:起拍价格|起拍价|起拍|价格)")
    if start_price is not None:
        fields["startPriceAmount"] = start_price
    duration_seconds = _duration_seconds(message)
    if duration_seconds is not None:
        fields["durationSeconds"] = duration_seconds
    cap_price = _money_cents(message, r"(?:封顶价格|封顶价|封顶)")
    if cap_price is not None:
        fields["capPriceAmount"] = cap_price
    min_increment = _money_cents(
        message, r"(?:最低(?:每次)?加价|每次最低加价|最低加价幅度|加价幅度)"
    )
    if min_increment is not None:
        fields["minIncrementAmount"] = min_increment
    deposit = _money_cents(message, r"(?:保证金)")
    if deposit is not None:
        fields["depositAmount"] = deposit
    category = re.search(
        r"(?:分类|品类)\s*(?:是|为|[:：])\s*([^，,。；;]+)", message
    )
    if category and category.group(1).strip():
        fields["category"] = category.group(1).strip()
    stock = re.search(r"库存\s*(?:为|是|[:：])?\s*(\d+)\s*(?:件|个|台|瓶|套)?", message)
    if stock is not None:
        fields["stock"] = int(stock.group(1))
    anti_snipe_window = re.search(
        r"(?:结束|截止)前?\s*(\d+)\s*秒[^，,。；;]*(?:出价|竞价)", message
    )
    if anti_snipe_window is not None:
        fields["antiSnipeWindowSeconds"] = int(anti_snipe_window.group(1))
    anti_snipe_extend = re.search(
        r"(?:就|则|自动)?\s*延长\s*(\d+)\s*秒", message
    )
    if anti_snipe_extend is not None:
        fields["antiSnipeExtendSeconds"] = int(anti_snipe_extend.group(1))
    max_extend_count = re.search(r"最多\s*延长\s*(\d+)\s*次", message)
    if max_extend_count is not None:
        fields["maxExtendCount"] = int(max_extend_count.group(1))
    return fields


def new_lot_defaults(explicit_fields: dict) -> dict:
    start_price = explicit_fields.get("startPriceAmount")
    increment = max(100, start_price // 100) if isinstance(start_price, int) else 100
    title = explicit_fields.get("title")
    description = (
        f"{title}竞拍拍品，具体品相以主图与现场说明为准。"
        if isinstance(title, str) and title.strip()
        else "竞拍拍品，具体品相以主图与现场说明为准。"
    )
    return {
        "minIncrementAmount": increment,
        "category": "其他",
        "description": description,
        "tags": ["AI草稿"],
        "stock": 1,
        "antiSnipeWindowSeconds": 30,
        "antiSnipeExtendSeconds": 15,
        "maxExtendCount": 5,
        "depositAmount": 0,
    }


def ground_provider_turn(
    current: list[CandidatePatch],
    proposed: TurnResult,
    *,
    message: str,
    run_id: str,
) -> TurnResult:
    """Close the provider boundary before merge/persistence.

    Explicit user values always win and receive USER provenance. On the initial
    turn numeric auction defaults are deterministic; the model may only improve
    descriptive copy. Unknown provider field names are dropped with a warning.
    """

    if len(current) > 1:
        return proposed
    if RequiredInput.AMBIGUOUS_CANDIDATE in proposed.required_inputs:
        return proposed
    if not current and _is_copy_edit_request(message):
        return TurnResult(
            assistant_message="请说明要修改哪一件候选拍品。",
            candidate_patches=[],
            required_inputs=[RequiredInput.AMBIGUOUS_CANDIDATE],
            review_readiness="NEEDS_INPUT",
            warnings=list(proposed.warnings),
        )
    patch = proposed.candidate_patches[0] if proposed.candidate_patches else None
    if patch is not None and patch.source_type != "NEW" and not current:
        return proposed
    if current and patch is not None and patch.source_type != current[0].source_type:
        return proposed

    explicit = explicit_lot_fields(message)
    if patch is None and not explicit:
        return proposed

    warnings = list(proposed.warnings)
    provider_fields = dict(patch.fields) if patch is not None else {}
    unknown = sorted(set(provider_fields) - EXECUTABLE_LOT_FIELDS)
    if unknown:
        warnings.append("Ignored unsupported provider fields: " + ", ".join(unknown))

    if current:
        fields = {
            name: value
            for name, value in provider_fields.items()
            if name in EXECUTABLE_LOT_FIELDS
        }
        sources = {
            name: (patch.field_sources.get(name, "AI") if patch is not None else "AI")
            for name in fields
        }
        candidate_id = current[0].candidate_id
        source_type = current[0].source_type
    else:
        fields = new_lot_defaults(explicit)
        sources = {name: "AI" for name in fields}
        for name in _INITIAL_MODEL_COPY_FIELDS:
            if name in provider_fields:
                fields[name] = provider_fields[name]
                sources[name] = "AI"
        candidate_id = candidate_id_for_run(run_id)
        source_type = "NEW"

    fields.update(explicit)
    sources.update({name: "USER" for name in explicit})
    grounded = CandidatePatch(
        candidate_id=candidate_id,
        source_type=source_type,
        fields=fields,
        field_sources=sources,
    )
    return TurnResult(
        assistant_message=proposed.assistant_message,
        candidate_patches=[grounded],
        required_inputs=proposed.required_inputs,
        review_readiness=proposed.review_readiness,
        warnings=warnings,
    )


def _is_copy_edit_request(message: str) -> bool:
    return bool(
        re.search(r"(?:生成|撰写|写|改写|润色|优化|修改).*(?:介绍|描述|文案|标签|分类)", message)
        or re.search(r"(?:介绍|描述|文案|标签|分类).*(?:生成|撰写|改写|润色|优化|修改)", message)
    )


def _money_cents(message: str, label_pattern: str) -> int | None:
    amount_pattern = (
        r"(?P<amount>\d+(?:\.\d{1,2})?)\s*(?P<scale>万|千)?\s*(?:元|块钱|块)?"
    )
    label_first = re.search(
        label_pattern + r"\s*(?:为|是|[:：])?\s*" + amount_pattern,
        message,
    )
    if label_first is not None:
        return _amount_match_cents(label_first)
    amount_first = re.search(
        r"(?:以\s*)?" + amount_pattern + r"\s*(?:作为\s*)?" + label_pattern,
        message,
    )
    if amount_first is not None:
        return _amount_match_cents(amount_first)
    return None


def _amount_match_cents(match: re.Match[str]) -> int:
    multiplier = {None: 1, "千": 1_000, "万": 10_000}[match.group("scale")]
    return int(Decimal(match.group("amount")) * multiplier * 100)


def _duration_seconds(message: str) -> int | None:
    label = r"(?:竞拍(?:时长|时间)?|拍卖时长|时长|持续|拍)"
    numeric = re.search(
        label
        + r"\s*(?:为|是|[:：])?\s*(?P<amount>\d+(?:\.\d+)?)\s*"
        + r"(?P<unit>小时|分钟|秒)",
        message,
    )
    if numeric is not None:
        multipliers = {"小时": 3_600, "分钟": 60, "秒": 1}
        return int(Decimal(numeric.group("amount")) * multipliers[numeric.group("unit")])
    half_hour = re.search(label + r"\s*(?:为|是|[:：])?\s*半(?:个)?(?:小时|钟头)", message)
    if half_hour is not None:
        return 1_800
    return None
