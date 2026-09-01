"""The case harnesses give each role in a case its own loopback port."""

import socket
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients"))

from ports import PortPool, bind_clash  # noqa: E402


def test_ports_are_distinct_and_held():
    with socket.socket() as busy:
        busy.bind(("127.0.0.1", 0))
        busy.listen(1)
        taken = busy.getsockname()[1]
        with PortPool(avoid=[taken]) as pool:
            ports = [pool.take() for _ in range(40)]
            assert len(set(ports)) == 40
            assert taken not in ports
            # While the pool holds them, no later bind to port 0 gets one.
            others = []
            for _ in range(40):
                s = socket.socket()
                s.bind(("127.0.0.1", 0))
                others.append(s)
            try:
                assert not {s.getsockname()[1] for s in others} & set(ports)
            finally:
                for s in others:
                    s.close()


def test_release_frees_the_ports():
    pool = PortPool()
    port = pool.take()
    pool.release()
    with socket.socket() as s:
        s.bind(("127.0.0.1", port))


def test_bind_clash_names_the_errors_of_each_language():
    assert bind_clash({"stderr": "listen tcp 127.0.0.1:5: bind: address already in use"})
    assert bind_clash({"stderr": ["Address in use (os error 98)"]})
    assert bind_clash(
        {"stderr": "[Errno 98] error while attempting to bind: address already in use"}
    )
    assert not bind_clash({"stderr": "port 5 already answers"})


def test_a_number_inside_a_unicode_escape_is_not_replaced():
    """A PID of 2014 must not turn the escape of an em dash, \\u2014, in
    the dumped JSON into \\u{PID1}: that is not JSON and the harness
    crashed on it."""
    import json

    from ports import sub_number

    text = json.dumps({"s": "a — b, pid 2014, ਒ 12"})
    got = sub_number(sub_number(text, "2014", "{PID1}"), "12", "{PID2}")
    assert json.loads(got) == {"s": "a — b, pid {PID1}, ਒ {PID2}"}
    assert sub_number("x12345y", "2345", "{P}") == "x12345y"


def test_a_port_inside_a_digest_or_a_decimal_is_not_replaced():
    """The port substitution runs before the receipt check: a port number
    inside a hex digest or after a decimal point turned a good receipt
    into {HASH BAD} (the hash, or evidence JSON that no longer parsed)."""
    from ports import sub_number

    assert sub_number("ab41235cd", "41235", "{PORT}") == "ab41235cd"
    assert sub_number('{"duration_ms": 7.41235}', "41235", "{PORT}") == '{"duration_ms": 7.41235}'
    assert sub_number("41235.5", "41235", "{PORT}") == "41235.5"
    assert sub_number("x41235", "41235", "{PORT}") == "x41235"
    assert sub_number("41235_a", "41235", "{PORT}") == "41235_a"
    # A port that stands alone is still named.
    assert sub_number("http://127.0.0.1:41235/x", "41235", "{PORT}") == (
        "http://127.0.0.1:{PORT}/x"
    )
    assert sub_number('{"port": 41235}', "41235", "{PORT}") == '{"port": {PORT}}'
    assert sub_number("on port 41235.", "41235", "{PORT}") == "on port {PORT}."
    assert sub_number("-41235 and (41235)", "41235", "{PORT}") == "-{PORT} and ({PORT})"
