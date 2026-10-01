"""Error text from a model server never holds a credential.

The body of a failed reply is cut to 500 characters. A key that straddles the
cut used to survive: the cut left a prefix too short for the key pattern, and
the pattern ran after the cut. The text is cleaned first and cut second.
"""

from opendaisugi import llm_client

KEY = "sk-ABCD" + "MIDDLEPART1234" + "WXYZ"


def _wire():
    return llm_client.Wire(kind="chat", model="m", url="http://x/v1", headers=())


def test_a_key_that_straddles_the_500_char_cut_is_not_leaked():
    body = "x" * 492 + KEY + "tail"
    try:
        llm_client.read_reply(_wire(), 500, body)
    except llm_client.ModelCallError as exc:
        text = str(exc)
    else:
        raise AssertionError("a 500 must raise")
    assert "sk-ABCDM" not in text
    assert "MIDDLE" not in text


def test_unreadable_reply_is_cleaned_before_the_cut():
    body = "y" * 192 + KEY
    try:
        llm_client.read_reply(_wire(), 200, body)
    except llm_client.ModelCallError as exc:
        text = str(exc)
    else:
        raise AssertionError("not JSON must raise")
    assert "sk-ABCDM" not in text and "MIDDLE" not in text


def test_bearer_tokens_and_key_query_parameters_are_redacted():
    text = llm_client.clean(
        "Authorization: Bearer abcdef0123456789tokenvalue and "
        "http://h/v1/models?api_key=SECRETVALUE123&x=1 and ?key=OTHERSECRET99 "
        "and token=zzzzzzzz"
    )
    assert "abcdef0123456789tokenvalue" not in text
    assert "SECRETVALUE123" not in text
    assert "OTHERSECRET99" not in text
    assert "zzzzzzzz" not in text
    assert "Bearer ..." in text
    assert "x=1" in text
