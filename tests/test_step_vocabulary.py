"""Tests for the step metadata vocabulary loader."""

from __future__ import annotations

import pytest

from opendaisugi.step_vocabulary import (
    CanonicalKeys,
    assert_step_matches_vocabulary,
    load_canonical_keys,
)


def test_loader_returns_email_send_keys():
    keys = load_canonical_keys()
    assert "email_send" in keys
    assert "signature" in keys["email_send"]
    assert "body" in keys["email_send"]
    assert "to" in keys["email_send"]


def test_vocabulary_assertion_passes_on_canonical_step():
    step = {
        "id": "s1",
        "type": "email_send",
        "metadata": {"to": "x@y.z", "signature": "Robin", "body": "hi"},
    }
    assert_step_matches_vocabulary(step)


def test_vocabulary_assertion_warns_on_non_canonical_keys():
    step = {
        "id": "s1",
        "type": "email_send",
        "metadata": {"to": "x", "signature": "Robin", "body": "hi", "sender_nickname": "C"},
    }
    with pytest.warns(UserWarning, match="non-canonical"):
        assert_step_matches_vocabulary(step)


def test_unknown_step_type_is_accepted():
    """New step types without vocabulary entries are allowed."""
    step = {
        "id": "s1",
        "type": "brand_new_type",
        "metadata": {"foo": "bar"},
    }
    assert_step_matches_vocabulary(step)
