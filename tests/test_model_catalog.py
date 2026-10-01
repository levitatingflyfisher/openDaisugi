"""The model catalog for the garden: the curated list, the default by
hardware, the recorded choice, and the Hugging Face search."""

from __future__ import annotations

import json
import urllib.error

import pytest

from opendaisugi import model_catalog as mc
from opendaisugi.hardware import VoiceHardware

DESK = VoiceHardware(ram_gb=16.0, cpus=8, vram_gb=0.0)
SMALL = VoiceHardware(ram_gb=4.0, cpus=2, vram_gb=0.0)
GPU_SMALL_RAM = VoiceHardware(ram_gb=4.0, cpus=2, vram_gb=8.0)
UNKNOWN = VoiceHardware(ram_gb=None, cpus=1, vram_gb=0.0)


def test_every_entry_has_exactly_the_neutral_fields():
    cat = mc.load()
    assert cat["models"]
    for m in cat["models"]:
        assert tuple(m) == mc.ENTRY_KEYS
        assert isinstance(m["params"], int) and m["params"] > 0
        assert m["suits"] in ("capable", "weak")
        assert m["note"].endswith(".") and "\n" not in m["note"]


def test_the_defaults_are_in_the_list():
    cat = mc.load()
    ids = [m["id"] for m in cat["models"]]
    assert cat["defaults"]["capable"] == "ibm-granite/granite-4.1-3b"
    assert cat["defaults"]["weak"] == "ibm-granite/granite-4.0-1b"
    assert set(cat["defaults"].values()) <= set(ids)


def test_each_default_carries_a_permissive_licence():
    """The machine picks a default with no question to the user, so it must
    be a model under an OSI or permissive licence."""
    cat = mc.load()
    by_id = {m["id"]: m for m in cat["models"]}
    for model_id in cat["defaults"].values():
        assert by_id[model_id]["license"] in ("apache-2.0", "mit", "bsd-3-clause"), model_id


def test_a_custom_licence_shows_its_exact_name():
    """An entry under a custom licence shows that licence by its full name,
    as every entry shows its licence."""
    cat = mc.load()
    llama = [m for m in cat["models"] if m["id"].startswith("meta-llama/Llama-3.2-")]
    assert len(llama) == 2
    assert {m["license"] for m in llama} == {"Llama 3.2 Community License"}


def test_the_default_follows_the_hardware():
    assert mc.hardware_class(DESK) == "capable"
    assert mc.hardware_class(SMALL) == "weak"
    assert mc.hardware_class(GPU_SMALL_RAM) == "capable"
    assert mc.hardware_class(UNKNOWN) == "weak"
    assert mc.default_model(DESK) == "ibm-granite/granite-4.1-3b"
    assert mc.default_model(SMALL) == "ibm-granite/granite-4.0-1b"


def test_the_hardware_line_names_what_it_read():
    assert mc.hardware_line(DESK) == (
        "This box has 16 GB of RAM, so the default is ibm-granite/granite-4.1-3b."
    )
    assert mc.hardware_line(GPU_SMALL_RAM) == (
        "This box has 4 GB of RAM and a GPU with 8 GB, so the default is "
        "ibm-granite/granite-4.1-3b."
    )
    assert mc.hardware_line(UNKNOWN) == (
        "This box has an unknown amount of RAM, so the default is ibm-granite/granite-4.0-1b."
    )


def test_any_id_is_recorded_as_given(tmp_path):
    for mid in (
        "Qwen/Qwen2.5-1.5B-Instruct",
        "/models/mine.gguf",
        "./local/dir",
        "someone/unknown-model",
    ):
        path = mc.record_choice(tmp_path, mid)
        assert json.loads(path.read_text()) == {"model": mid}
        assert mc.in_use(tmp_path) == mid


def test_a_blank_id_is_not_recorded(tmp_path):
    with pytest.raises(ValueError):
        mc.record_choice(tmp_path, "  ")
    assert mc.in_use(tmp_path) is None


def test_an_unreadable_choice_counts_as_none(tmp_path):
    (tmp_path / mc.CHOICE_FILE).write_text("{bad")
    assert mc.in_use(tmp_path) is None
    (tmp_path / mc.CHOICE_FILE).write_text('{"model": 5}')
    assert mc.in_use(tmp_path) is None


def test_the_trainer_base_is_the_choice_else_the_default(tmp_path):
    assert mc.base_model(tmp_path, SMALL) == "ibm-granite/granite-4.0-1b"
    mc.record_choice(tmp_path, "Qwen/Qwen2.5-1.5B-Instruct")
    assert mc.base_model(tmp_path, SMALL) == "Qwen/Qwen2.5-1.5B-Instruct"


def test_params_print_in_billions():
    assert mc.fmt_params(3402836480) == "3.4B"
    assert mc.fmt_params(350000000) == "0.3B"
    assert mc.fmt_params(None) == "?"


def test_the_endpoint_comes_from_hf_endpoint():
    assert mc.endpoint({}) == "https://huggingface.co"
    assert mc.endpoint({"HF_ENDPOINT": ""}) == "https://huggingface.co"
    assert mc.endpoint({"HF_ENDPOINT": "http://127.0.0.1:9/"}) == "http://127.0.0.1:9"


def test_hf_hub_offline_is_read_as_huggingface_hub_reads_it():
    for v in ("1", "ON", "yes", "True"):
        assert mc.offline_env({"HF_HUB_OFFLINE": v})
    for v in ("", "0", "no", "off"):
        assert not mc.offline_env({"HF_HUB_OFFLINE": v})
    assert not mc.offline_env({})


def test_the_search_target_is_built_in_one_fixed_order():
    assert mc.search_target("granite 4") == (
        "/api/models?search=granite%204&limit=100&sort=downloads&direction=-1"
        "&expand%5B%5D=cardData&expand%5B%5D=gguf&expand%5B%5D=pipeline_tag"
        "&expand%5B%5D=safetensors&expand%5B%5D=tags"
    )
    assert mc.search_target("é/x~_.-").startswith("/api/models?search=%C3%A9%2Fx~_.-&")


def _row(mid, **kw):
    return {"id": mid, **kw}


def test_a_listing_reads_into_the_neutral_fields():
    raw = json.dumps(
        [
            _row(
                "a/st",
                cardData={"license": "apache-2.0"},
                safetensors={"total": 3000000000},
                pipeline_tag="text-generation",
                tags=["license:apache-2.0"],
            ),
            _row(
                "b/gguf",
                gguf={"total": 1000000000, "context_length": 32768},
                tags=["gguf", "license:mit"],
            ),
            _row("c/other", cardData={"license": "other", "license_name": "custom-x"}),
            _row("d/bare"),
            {"no": "id"},
            5,
        ]
    ).encode()
    rows = mc.parse_listing(raw)
    assert [r["id"] for r in rows] == ["a/st", "b/gguf", "c/other", "d/bare"]
    a, b, c, d = rows
    assert tuple(a) == mc.ENTRY_KEYS
    assert a == {
        "id": "a/st",
        "params": 3000000000,
        "license": "apache-2.0",
        "context_length": None,
        "gguf": None,
        "suits": "capable",
        "note": "text-generation",
    }
    assert b["params"] == 1000000000 and b["license"] == "mit"
    assert b["gguf"] == "b/gguf" and b["context_length"] == 32768 and b["suits"] == "weak"
    assert c["license"] == "custom-x"
    assert d["params"] is None and d["license"] is None and d["suits"] is None
    assert d["note"] == ""


def test_a_listing_that_is_not_a_list_is_a_bad_answer():
    for raw in (b"{}", b"not json", b'"x"'):
        with pytest.raises(mc.BadAnswer):
            mc.parse_listing(raw)


def test_filters_drop_by_size_and_license():
    rows = mc.parse_listing(
        json.dumps(
            [
                _row("a/big", safetensors={"total": 9000000000}, cardData={"license": "mit"}),
                _row("b/ok", safetensors={"total": 3000000000}, cardData={"license": "mit"}),
                _row("c/unknown", cardData={"license": "mit"}),
                _row("d/other", safetensors={"total": 1000000000}, cardData={"license": "x"}),
            ]
        ).encode()
    )
    ids = lambda rs: [r["id"] for r in rs]  # noqa: E731
    assert ids(mc.filter_rows(rows, 8.0, [], 20)) == ["b/ok", "d/other"]
    assert ids(mc.filter_rows(rows, 8.0, ["mit"], 20)) == ["b/ok"]
    assert ids(mc.filter_rows(rows, 0.0, [], 20)) == ["a/big", "b/ok", "c/unknown", "d/other"]
    assert ids(mc.filter_rows(rows, 0.0, [], 2)) == ["a/big", "b/ok"]


def test_search_asks_the_endpoint_and_shows_any_model():
    asked = []

    def fetch(url):
        asked.append(url)
        return json.dumps(
            [_row("Qwen/Qwen2.5-1.5B-Instruct", safetensors={"total": 1543714304})]
        ).encode()

    rows = mc.search("qwen", env={"HF_ENDPOINT": "http://h:1/"}, fetch=fetch)
    assert asked == ["http://h:1" + mc.search_target("qwen")]
    assert [r["id"] for r in rows] == ["Qwen/Qwen2.5-1.5B-Instruct"]
    assert tuple(rows[0]) == mc.ENTRY_KEYS


def test_search_offline_never_fetches():
    def fetch(url):
        raise AssertionError("fetched while offline")

    with pytest.raises(mc.Offline):
        mc.search("x", env={"HF_HUB_OFFLINE": "1"}, fetch=fetch)


def test_fetch_errors_split_into_offline_and_status(monkeypatch):
    def refused(req, timeout):
        raise urllib.error.URLError(ConnectionRefusedError(111, "refused"))

    monkeypatch.setattr(mc.urllib.request, "urlopen", refused)
    with pytest.raises(mc.Offline):
        mc.fetch("http://127.0.0.1:9/api/models")

    def status(req, timeout):
        raise urllib.error.HTTPError(req.full_url, 503, "busy", {}, None)

    monkeypatch.setattr(mc.urllib.request, "urlopen", status)
    with pytest.raises(mc.HTTPStatus) as exc:
        mc.fetch("http://127.0.0.1:9/api/models")
    assert exc.value.code == 503


def test_the_table_pads_columns_and_never_the_last():
    rows = mc.load()["models"][:2]
    lines = mc.table_lines(rows)
    assert lines[0].startswith("  ID")
    assert lines[0].endswith("GOOD AT")
    assert lines[1] == (
        "  ibm-granite/granite-4.1-3b  3.4B  apache-2.0  131072   yes   capable  "
        "Tool calling and following instructions."
    )
    assert all(not ln.endswith(" ") for ln in lines)
