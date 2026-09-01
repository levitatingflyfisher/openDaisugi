"""Predicate expression algebra for envelope invariants/postconditions (v0.9.0).

A small set of composable operators replaces the closed if/elif invariant
dispatcher. Expressions are Pydantic models forming a discriminated union on
the ``op`` field. A separate module (``predicate_z3``) compiles them to Z3.
"""

from __future__ import annotations

from typing import Annotated, Any, Literal, Union

from pydantic import BaseModel, Field, PrivateAttr, TypeAdapter


class _ExprBase(BaseModel):
    """Base class for all predicate expressions."""

    model_config = {"frozen": False}


class Equals(_ExprBase):
    op: Literal["equals"] = "equals"
    path: str
    value: Any


class NotEquals(_ExprBase):
    op: Literal["not_equals"] = "not_equals"
    path: str
    value: Any


class InSet(_ExprBase):
    op: Literal["in_set"] = "in_set"
    path: str
    values: list[Any]


class NotInSet(_ExprBase):
    op: Literal["not_in_set"] = "not_in_set"
    path: str
    values: list[Any]


class Matches(_ExprBase):
    op: Literal["matches"] = "matches"
    path: str
    regex: str


class NotMatches(_ExprBase):
    op: Literal["not_matches"] = "not_matches"
    path: str
    regex: str


class NumericRange(_ExprBase):
    op: Literal["numeric_range"] = "numeric_range"
    path: str
    min: float
    max: float


class LengthRange(_ExprBase):
    """Length bound on a string or collection at ``path``.

    Strings compile to ``z3.Length(var)`` for symbolic reasoning; lists
    and dicts fall back to concrete evaluation. ``max`` is optional
    (omit for open-ended upper bound).
    """

    op: Literal["length_range"] = "length_range"
    path: str
    min: int = 0
    max: int | None = None


class Exists(_ExprBase):
    op: Literal["exists"] = "exists"
    path: str


class IsEmpty(_ExprBase):
    op: Literal["is_empty"] = "is_empty"
    path: str


class And(_ExprBase):
    op: Literal["and"] = "and"
    children: list["Expression"]


class Or(_ExprBase):
    op: Literal["or"] = "or"
    children: list["Expression"]


class Not(_ExprBase):
    op: Literal["not"] = "not"
    child: "Expression"


class Implies(_ExprBase):
    op: Literal["implies"] = "implies"
    a: "Expression"
    b: "Expression"


class ForallSteps(_ExprBase):
    op: Literal["forall_steps"] = "forall_steps"
    pred: "Expression"


class ExistsStep(_ExprBase):
    op: Literal["exists_step"] = "exists_step"
    pred: "Expression"


class ForallOutputs(_ExprBase):
    op: Literal["forall_outputs"] = "forall_outputs"
    pred: "Expression"


class ForallWrites(_ExprBase):
    """For every write path of one step, ``pred`` holds.

    It sits inside ``forall_steps`` or ``exists_step``. ``pred`` sees one
    write path at a time as the field ``path``. A step's write paths are
    the path of a ``file_write`` step, and for a ``shell`` step every
    literal write redirect the shell decomposition finds and every file a
    known command writes through its operands (``cp``, ``mv``, ``tee``,
    ``sed -i``, ``rm`` and others), also inside ``sh -c`` and other
    command-taking wrappers (``write_paths.py``). The null device and the
    standard streams are not write paths. Each path is normalized
    (``posixpath.normpath``). Other step types have no write paths. A shell
    step whose write paths cannot be read makes the quantifier false.
    """

    op: Literal["forall_writes"] = "forall_writes"
    pred: "Expression"
    # The directory relative write paths resolve against. Only an unfolded
    # word sets it (dialect.unfold, from the call's cwd); it is never read
    # from JSON and never dumped.
    _base: str | None = PrivateAttr(default=None)


class DependsOn(_ExprBase):
    op: Literal["depends_on"] = "depends_on"
    step_id_a: str
    step_id_b: str


class Before(_ExprBase):
    op: Literal["before"] = "before"
    step_id_a: str
    step_id_b: str


class AliasRef(_ExprBase):
    op: Literal["alias"] = "alias"
    name: str
    args: dict[str, Any] = Field(default_factory=dict)


class LLMCheck(_ExprBase):
    op: Literal["llm_check"] = "llm_check"
    rule: str


Expression = Annotated[
    Union[
        Equals,
        NotEquals,
        InSet,
        NotInSet,
        Matches,
        NotMatches,
        NumericRange,
        LengthRange,
        Exists,
        IsEmpty,
        And,
        Or,
        Not,
        Implies,
        ForallSteps,
        ExistsStep,
        ForallOutputs,
        ForallWrites,
        DependsOn,
        Before,
        AliasRef,
        LLMCheck,
    ],
    Field(discriminator="op"),
]


_ADAPTER: TypeAdapter = TypeAdapter(Expression)


def parse_expression(data: Any) -> Any:
    """Parse a dict or Expression into a validated Expression instance."""
    return _ADAPTER.validate_python(data)


And.model_rebuild()
Or.model_rebuild()
Not.model_rebuild()
Implies.model_rebuild()
ForallSteps.model_rebuild()
ExistsStep.model_rebuild()
ForallOutputs.model_rebuild()
ForallWrites.model_rebuild()


__all__ = [
    "And",
    "AliasRef",
    "Before",
    "DependsOn",
    "Equals",
    "Exists",
    "ExistsStep",
    "Expression",
    "ForallOutputs",
    "ForallSteps",
    "ForallWrites",
    "Implies",
    "InSet",
    "IsEmpty",
    "LengthRange",
    "LLMCheck",
    "Matches",
    "Not",
    "NotEquals",
    "NotInSet",
    "NotMatches",
    "NumericRange",
    "Or",
    "parse_expression",
]
