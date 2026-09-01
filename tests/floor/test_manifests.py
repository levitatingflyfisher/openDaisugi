"""The Python manifest evaluator: a port of harness/coppice/internal/detect.

Manifests are the fallback path (master spec 3.1). A manifest may never
report ``done``, and a screen nothing matches is ``unknown``, never a
confident ``idle``. This module's evaluator reads the same TOML files coppice's
Go evaluator reads, with the same matcher semantics, gates, priority, and
region slicing, so the shared conformance fixtures at the bottom of this
file must produce the same verdict in both languages.
"""

from __future__ import annotations

import re

import pytest

from opendaisugi.floor import manifests as mod
from opendaisugi.floor.manifests import (
    ManifestSchemaMismatch,
    classify,
    default_manifest_dirs,
    load_manifests,
    translate_rust_regex,
    valid_region,
)
from tests.floor import hostfacts
from tests.floor.conftest import write_manifest

# --- region validity, mirroring manifest.go's ValidRegion ------------------


def test_valid_region_accepts_the_documented_names():
    for name in [
        "whole_recent",
        "after_last_prompt_marker",
        "before_current_prompt_marker",
        "whole_recent_without_current_prompt_marker",
        "current_prompt_block_marker",
        "after_current_prompt_block_marker",
        "prompt_box_body",
        "above_prompt_box",
        "last_non_empty_above_prompt_box",
        "after_last_horizontal_rule",
        "osc_title",
        "osc_progress",
        "bottom_lines(5)",
        "bottom_non_empty_lines(12)",
        "top_non_empty_lines(3)",
        "bottom_lines(0)",
    ]:
        assert valid_region(name), name


def test_valid_region_rejects_malformed_names():
    for name in [
        "",
        "bottom_lines()",
        "top_non_empty_lines(0)",
        "top_non_empty_lines(007)",
        "bottom_lines(x)",
        "bottom_lines(-3)",
        "bottom_non_empty_lines(-1)",
        "bottom_lines(+3)",
        "bottom_lines(3.5)",
        "bottom_lines( 3)",
        "nonsense",
    ]:
        assert not valid_region(name), name


# --- region slicing, mirroring regions.go -----------------------------------

_RULE = "─" * 20
_BOX = "line one\nline two\n" + _RULE + "\n❯ type here\n" + _RULE + "\n  ? for shortcuts\n"


def test_bottom_non_empty_lines_takes_the_bottom_occurrence():
    assert mod._region_text("bottom_non_empty_lines(2)", "a\n\nb\n\nc\n", "", "") == "b\n\nc"


def test_bottom_lines_counts_blank_lines_too():
    assert mod._region_text("bottom_lines(2)", "a\nb\nc\n", "", "") == "b\nc"


def test_bottom_lines_keeps_trailing_blank_lines():
    assert mod._region_text("bottom_lines(2)", "a\nb\nc\n\n\n", "", "") == "\n"


def test_top_non_empty_lines_takes_the_top_occurrence():
    assert mod._region_text("top_non_empty_lines(2)", "\na\nb\nc\n", "", "") == "\na\nb"


def test_prompt_box_body_is_between_the_last_two_rules():
    assert mod._region_text("prompt_box_body", _BOX, "", "") == "❯ type here"


def test_above_prompt_box_stops_at_the_box_top():
    assert mod._region_text("above_prompt_box", _BOX, "", "") == "line one\nline two"


def test_last_non_empty_above_prompt_box():
    assert mod._region_text("last_non_empty_above_prompt_box", _BOX, "", "") == "line two"


def test_after_last_horizontal_rule():
    assert mod._region_text("after_last_horizontal_rule", _BOX, "", "") == "  ? for shortcuts"


def test_codex_prompt_marker_regions():
    screen = "• ran a tool\nsome output\n› \n"
    assert mod._region_text("after_last_prompt_marker", screen, "", "") == ""
    assert (
        mod._region_text("before_current_prompt_marker", screen, "", "")
        == "• ran a tool\nsome output"
    )
    assert mod._region_text("whole_recent_without_current_prompt_marker", screen, "", "") == ""
    assert mod._region_text("current_prompt_block_marker", screen, "", "") == "• ran a tool"
    assert (
        mod._region_text("after_current_prompt_block_marker", screen, "", "")
        == "• ran a tool\nsome output\n› "
    )


def test_osc_regions_come_from_their_own_fields():
    assert mod._region_text("osc_title", "ignored", "✳ idle", "4;0") == "✳ idle"
    assert mod._region_text("osc_progress", "ignored", "✳ idle", "4;0") == "4;0"


# --- translate_rust_regex ----------------------------------------------------


def test_bare_backslash_u_is_left_untouched_python_reads_it_natively():
    # Python's re already understands \uXXXX; Go's does not, so only Go's own
    # port has to touch this form.
    assert translate_rust_regex(r"[⠀-⣿]") == r"[⠀-⣿]"
    assert re.compile(translate_rust_regex(r"[⠀-⣿]")).match(chr(0x2801))


def test_braced_rust_unicode_escape_is_translated():
    assert translate_rust_regex(r"\u{fe0e}") == r"\U0000fe0e"
    assert re.compile(translate_rust_regex(r"\u{fe0e}")).match(chr(0xFE0E))


def test_braced_go_hex_escape_is_also_translated():
    # Several vendored manifests already write \x{XXXX} directly (valid
    # syntax in both Rust and Go). Python's re only accepts a 2-digit \x, so
    # this needs translating too, unlike Go's own translateRustRegex, which
    # leaves \x{XXXX} untouched because Go already understands it.
    assert translate_rust_regex(r"\x{2733}") == r"\U00002733"
    assert re.compile(translate_rust_regex(r"^\x{2733} ")).match(chr(0x2733) + " ")


def test_alphabetic_property_is_translated_to_a_letter_class():
    pattern = re.compile(translate_rust_regex(r"^\s*(◔|◑|◕|●)\s+\p{Alphabetic}"))
    assert pattern.search("◔ Thinking")
    assert not pattern.search("◔ 123")


def test_the_letter_class_matches_go_pl_not_rust_alphabetic_at_the_boundary():
    """Go's own translateRustRegex narrows Rust's wider Alphabetic property
    to its own general category \\pL. This evaluator has to match Go's
    choice, since the conformance test measures agreement with Go, not
    with Rust's original semantics. Three codepoints sit right at the
    boundary between the two: a modifier letter is in the Letter general
    category and in Rust's Alphabetic; a letter-number and a combining
    mark are in Rust's Alphabetic but not in the Letter general category.
    """
    pattern = re.compile(translate_rust_regex(r"\p{Alphabetic}"))
    assert pattern.fullmatch(chr(0x02B0)), "Lm modifier letter must match \\pL"
    assert not pattern.fullmatch(chr(0x2160)), "Nl letter-number is not in \\pL"
    assert not pattern.fullmatch(chr(0x0301)), "Mn combining mark is not in \\pL"


def test_an_escaped_backslash_before_the_construct_is_left_alone():
    cases = {
        r"⠀": r"⠀",  # native, untouched regardless
        r"\\u{2800}": r"\\u{2800}",  # even run: an escaped literal backslash
        r"\\\u{2800}": r"\\\U00002800",  # odd run: one literal pair, one live escape
        r"\\p{Alphabetic}": r"\\p{Alphabetic}",  # even run: left alone
    }
    for pattern, want in cases.items():
        assert translate_rust_regex(pattern) == want, pattern


def test_unsupported_construct_fails_closed_not_silently():
    with pytest.raises(re.error):
        re.compile(translate_rust_regex(r"\p{Emoji}"))


# --- Go's Perl classes are ASCII; Python's are Unicode by default ----------
#
# Reproduced against a real vendored rule and confirmed against Go directly:
# Go's \s is [\t\n\f\r ], \d is [0-9], \w is [0-9A-Za-z_], and \b/\B use that
# same ASCII word set. Python's re on str patterns defines all four over
# Unicode. Every row below is the Go verdict.
_PERL_CLASS_CASES = [
    (r"^\s*yes\b", chr(0x00A0) + "yes", False),  # non-breaking space is not Go's \s
    (r"^\s*yes\b", chr(0x000B) + "yes", False),  # vertical tab is not Go's \s either
    (r"step \d+ done", "step " + chr(0x0669) + " done", False),  # Arabic-indic 9 isn't \d
    (r"\bpi\b", "épié", True),  # \xe9 is not a Go \w char, so \b fires around "pi"
]


def test_perl_classes_match_go_ascii_semantics_not_python_unicode_semantics():
    for pattern, text, want in _PERL_CLASS_CASES:
        compiled = mod._compile_pattern(pattern, "test", "regex")
        got = bool(compiled.search(text))
        assert got == want, f"{pattern!r} against {text!r}: want {want}, got {got}"


def test_nbsp_does_not_fool_the_trust_directory_regex_like_go():
    """The end-to-end reproduction: one non-breaking space inside codex's
    real trust_directory rule must stop the match, exactly as it does
    against Go's own evaluator."""
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    manifests = load_manifests([hostfacts.MANIFEST_DIR])
    plain = "> You are in /home/user/project\nDo you trust the contents of this directory?\n"
    nbsp = plain.replace("Do you trust", "Do" + chr(0x00A0) + "you trust")
    result = classify(plain, osc_progress=None, agent_hint="codex", manifests=manifests)
    assert (result.state, result.rule) == ("blocked", "trust_directory")
    result = classify(nbsp, osc_progress=None, agent_hint="codex", manifests=manifests)
    assert result.state == "unknown"


def test_p_alphabetic_inside_a_character_class_is_a_load_error(manifest_dir):
    manifests = _load_one(manifest_dir, "[[rules]]\nid = \"r\"\nregex = ['[\\p{Alphabetic}0-9]']\n")
    assert manifests == {}


def test_s_inside_a_character_class_uses_go_ascii_members():
    compiled = mod._compile_pattern(r"[\s]", "test", "regex")
    assert compiled.search(" ")
    assert not compiled.search(chr(0x00A0))


def test_negated_s_inside_a_character_class_uses_go_ascii_members():
    compiled = mod._compile_pattern(r"[\S]", "test", "regex")
    assert compiled.search(chr(0x00A0))
    assert not compiled.search(" ")


def test_d_inside_a_character_class_uses_go_ascii_members():
    compiled = mod._compile_pattern(r"[\d]", "test", "regex")
    assert compiled.search("5")
    assert not compiled.search(chr(0x0669))


def test_w_inside_a_character_class_uses_go_ascii_members():
    compiled = mod._compile_pattern(r"[\w]", "test", "regex")
    assert compiled.search("_")
    assert not compiled.search("é")


def test_dollar_anchor_does_not_match_before_a_trailing_newline():
    """Go's $ without (?m) means end of text, like \\z, not end of text or
    just before a final newline the way Python's default $ does."""
    compiled = mod._compile_pattern(r"^\s*>\s*(?:type\s*)?.*$", "test", "regex")
    assert not compiled.search("> You are in /home/user/project\n")
    assert compiled.search("> You are in /home/user/project")


def test_dollar_anchor_under_multiline_still_matches_end_of_line():
    """Under (?m), Python and Go already agree: $ matches before each
    line's own terminator, so this one is left untouched."""
    compiled = mod._compile_pattern(r"(?m)^b$", "test", "regex")
    assert compiled.search("a\nb\nc")


# Go applies the m flag from any real flag group that carries it, alone or
# combined with other flags, and from the scoped form (?m:...). A raw
# substring test for the literal four characters "(?m)" misses every one
# of these and also fires on a literal "(?m)" inside a character class,
# which is not a flag group at all. Every row here is the Go verdict.
_MULTILINE_FLAG_CASES = [
    (r"(?im)a$", "a\n", True),
    (r"(?mi)a$", "a\n", True),
    (r"(?sm)a$", "a\n", True),
    (r"(?m:a$)", "a\n", True),
    (r"(?im)^b$", "b\n", True),
    (r"[(?m)]$", "(?m)\n", False),
]


def test_multiline_flag_detection_matches_go_not_a_raw_substring_scan():
    for pattern, text, want in _MULTILINE_FLAG_CASES:
        compiled = mod._compile_pattern(pattern, "test", "regex")
        got = bool(compiled.search(text))
        assert got == want, f"{pattern!r} against {text!r}: want {want}, got {got}"


def test_an_even_backslash_run_before_a_class_still_opens_it():
    """Two backslashes are one escaped literal backslash; the [ right
    after it is a real, unescaped class opener, not more escaped text."""
    compiled = mod._compile_pattern(r"\\[\s]", "test", "regex")
    assert compiled.search("\\ ")
    assert compiled.search("a\\ b")


def test_an_even_backslash_run_before_a_digit_class_still_opens_it():
    compiled = mod._compile_pattern(r"\\[\d]", "test", "regex")
    assert compiled.search("\\5")


# --- manifest loading: whole-file load errors, mirroring manifest.go --------


def _load_one(directory, rules_toml, schema_agent="x"):
    write_manifest(directory, schema_agent, rules_toml)
    return load_manifests([directory])


def test_a_well_formed_manifest_loads(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\ncontains = ["a"]\n')
    assert set(manifests) == {"x"}
    assert manifests["x"].rules[0].id == "r"


def test_region_defaults_to_whole_recent(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\ncontains = ["a"]\n')
    assert manifests["x"].rules[0].region == "whole_recent"


def test_an_unknown_top_level_key_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\ncontains = ["a"]\n', schema_agent="x")
    write_manifest(manifest_dir, "bad", 'mystery = 1\n[[rules]]\nid = "r"\ncontains = ["a"]\n')
    manifests = load_manifests([manifest_dir])
    assert "bad" not in manifests
    assert "x" in manifests


def test_an_unknown_rule_key_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\ncontains = ["a"]\nvibes = true\n')
    assert manifests == {}


def test_an_unknown_key_inside_a_nested_gate_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(
        manifest_dir,
        '[[rules]]\nid = "r"\ncontains = ["a"]\nall = [{ contains = ["b"], vibes = true }]\n',
    )
    assert manifests == {}


def test_an_unknown_region_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(
        manifest_dir, '[[rules]]\nid = "r"\nregion = "the_vibe_zone"\ncontains = ["a"]\n'
    )
    assert manifests == {}


def test_top_non_empty_lines_needs_engine_three(manifest_dir):
    path = manifest_dir / "x.toml"
    path.write_text(
        'id = "x"\nmin_engine_version = 2\n'
        '[[rules]]\nid = "r"\nregion = "top_non_empty_lines(3)"\ncontains = ["a"]\n',
        encoding="utf-8",
    )
    assert load_manifests([manifest_dir]) == {}


def test_top_non_empty_lines_engine_check_is_skipped_when_unset(manifest_dir):
    """manifest.go only enforces this floor when min_engine_version != 0.

    A manifest with no min_engine_version key at all defaults to 0 and is
    exempt, even though a rule uses top_non_empty_lines. This is the
    README-versus-code split task 1 recorded: the README reads as an
    unconditional requirement, the code is not.
    """
    manifests = _load_one(
        manifest_dir, '[[rules]]\nid = "r"\nregion = "top_non_empty_lines(3)"\ncontains = ["a"]\n'
    )
    assert "x" in manifests


def test_a_manifest_asking_for_a_future_engine_drops_the_whole_manifest(manifest_dir):
    path = manifest_dir / "x.toml"
    path.write_text(
        'id = "x"\nmin_engine_version = 99\n[[rules]]\nid = "r"\ncontains = ["a"]\n',
        encoding="utf-8",
    )
    assert load_manifests([manifest_dir]) == {}


def test_a_rule_with_no_positive_matcher_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nnot = [{ contains = ["a"] }]\n')
    assert manifests == {}


def test_an_empty_rule_id_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = ""\ncontains = ["a"]\n')
    assert manifests == {}


def test_skip_state_update_requires_state_unknown(manifest_dir):
    manifests = _load_one(
        manifest_dir,
        '[[rules]]\nid = "r"\nstate = "idle"\nskip_state_update = true\ncontains = ["a"]\n',
    )
    assert manifests == {}


def test_skip_state_update_forbids_visible_evidence(manifest_dir):
    manifests = _load_one(
        manifest_dir,
        '[[rules]]\nid = "r"\nstate = "unknown"\nskip_state_update = true\n'
        'visible_idle = true\ncontains = ["a"]\n',
    )
    assert manifests == {}


def test_a_bad_regex_drops_the_whole_manifest_not_just_the_rule(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nregex = ["(unclosed"]\n')
    assert manifests == {}


def test_a_manifest_may_never_say_done(manifest_dir):
    """Master spec 3.1: done comes only from process or headless sources.

    "done" is not one of the four states the schema declares, so a rule
    asking for it is a load error like any other invalid state, and the
    whole manifest is dropped, not just that rule.
    """
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nstate = "done"\ncontains = ["a"]\n')
    assert manifests == {}


def test_gate_depth_over_eight_drops_the_whole_manifest(manifest_dir):
    body = '[[rules]]\nid = "r"\ncontains = ["top"]\nall = [' + "{ all = [" * 9
    body += '{ contains = ["a"] }' + " ] }" * 9 + "]\n"
    assert _load_one(manifest_dir, body) == {}


def test_gate_depth_exactly_eight_is_accepted(manifest_dir):
    body = '[[rules]]\nid = "r"\ncontains = ["top"]\nall = [' + "{ all = [" * 7
    body += '{ contains = ["a"] }' + " ] }" * 7 + "]\n"
    manifests = _load_one(manifest_dir, body)
    assert "x" in manifests


def test_too_many_rules_drops_the_whole_manifest(manifest_dir):
    body = '[[rules]]\nid = "r"\ncontains = ["a"]\n' * 129
    assert _load_one(manifest_dir, body) == {}


def test_exactly_128_rules_is_accepted(manifest_dir):
    body = '[[rules]]\nid = "r"\ncontains = ["a"]\n' * 128
    manifests = _load_one(manifest_dir, body)
    assert len(manifests["x"].rules) == 128


def test_a_matcher_over_512_chars_drops_the_whole_manifest(manifest_dir):
    body = f'[[rules]]\nid = "r"\ncontains = ["{"a" * 513}"]\n'
    assert _load_one(manifest_dir, body) == {}


def test_empty_rules_list_drops_the_whole_manifest(manifest_dir):
    path = manifest_dir / "x.toml"
    path.write_text('id = "x"\n', encoding="utf-8")
    assert load_manifests([manifest_dir]) == {}


def test_bad_toml_drops_the_whole_manifest(manifest_dir):
    path = manifest_dir / "x.toml"
    path.write_text("this is not [ valid toml\n", encoding="utf-8")
    assert load_manifests([manifest_dir]) == {}


def test_a_non_string_alias_drops_the_whole_manifest(manifest_dir):
    path = manifest_dir / "x.toml"
    path.write_text(
        'id = "x"\naliases = [1, 2]\n[[rules]]\nid = "r"\ncontains = ["a"]\n', encoding="utf-8"
    )
    assert load_manifests([manifest_dir]) == {}


def test_a_non_list_all_value_drops_only_that_manifest(manifest_dir):
    write_manifest(manifest_dir, "good", '[[rules]]\nid = "r"\ncontains = ["a"]\n')
    write_manifest(manifest_dir, "bad", '[[rules]]\nid = "r"\ncontains = ["a"]\nall = 5\n')
    manifests = load_manifests([manifest_dir])
    assert "good" in manifests
    assert "bad" not in manifests


def test_a_non_list_any_value_drops_only_that_manifest(manifest_dir):
    write_manifest(manifest_dir, "good", '[[rules]]\nid = "r"\ncontains = ["a"]\n')
    write_manifest(manifest_dir, "bad", '[[rules]]\nid = "r"\ncontains = ["a"]\nany = 5\n')
    manifests = load_manifests([manifest_dir])
    assert "good" in manifests
    assert "bad" not in manifests


def test_a_non_list_not_value_drops_only_that_manifest(manifest_dir):
    write_manifest(manifest_dir, "good", '[[rules]]\nid = "r"\ncontains = ["a"]\n')
    write_manifest(manifest_dir, "bad", '[[rules]]\nid = "r"\ncontains = ["a"]\nnot = 5\n')
    manifests = load_manifests([manifest_dir])
    assert "good" in manifests
    assert "bad" not in manifests


def test_a_falsy_non_string_region_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nregion = 0\ncontains = ["a"]\n')
    assert manifests == {}


def test_a_non_bool_skip_state_update_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(
        manifest_dir,
        '[[rules]]\nid = "r"\nstate = "unknown"\nskip_state_update = 1\ncontains = ["a"]\n',
    )
    assert manifests == {}


def test_a_non_bool_visible_flag_drops_the_whole_manifest(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nvisible_idle = 1\ncontains = ["a"]\n')
    assert manifests == {}


def test_lookahead_is_a_load_error(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nregex = ["foo(?=bar)"]\n')
    assert manifests == {}


def test_negative_lookahead_is_a_load_error(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nregex = ["foo(?!bar)"]\n')
    assert manifests == {}


def test_lookbehind_is_a_load_error(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nregex = ["(?<=foo)bar"]\n')
    assert manifests == {}


def test_negative_lookbehind_is_a_load_error(manifest_dir):
    manifests = _load_one(manifest_dir, '[[rules]]\nid = "r"\nregex = ["(?<!foo)bar"]\n')
    assert manifests == {}


def test_an_escaped_lookahead_marker_is_accepted_like_go():
    """An escaped "(" followed by "?=" is a literal "(?=" in the text, not
    a real lookahead group; Go accepts this and so must this port."""
    compiled = mod._compile_pattern(r"a\(?=b", "test", "regex")
    assert compiled.search("a(=b")


def test_a_lookahead_marker_inside_a_character_class_is_accepted_like_go():
    """A character class holding "(", "?" and "=" is not a lookahead
    group; Go accepts this too."""
    compiled = mod._compile_pattern(r"[(?=]x", "test", "regex")
    assert compiled.search("?x")


def test_an_alias_resolves_to_its_canonical_manifest():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    manifests = load_manifests([hostfacts.MANIFEST_DIR])
    screen = "Bash command\nDo you want to proceed?\n❯ 1. Yes\n"
    direct = classify(screen, osc_progress=None, agent_hint="claude", manifests=manifests)
    via_alias = classify(screen, osc_progress=None, agent_hint="claude-code", manifests=manifests)
    assert via_alias == direct
    assert via_alias.state != "unknown"


def test_a_canonical_id_wins_over_another_manifests_alias_of_the_same_name(tmp_path):
    a = tmp_path / "a"
    a.mkdir()
    write_manifest(a, "real", '[[rules]]\nid = "r"\nstate = "working"\ncontains = ["x"]\n')
    path = a / "impostor.toml"
    path.write_text(
        'id = "impostor"\naliases = ["real"]\n'
        '[[rules]]\nid = "r"\nstate = "blocked"\ncontains = ["x"]\n',
        encoding="utf-8",
    )
    manifests = load_manifests([a])
    result = classify("x", osc_progress=None, agent_hint="real", manifests=manifests)
    assert result.state == "working"


def test_go_lower_drops_the_combining_dot_on_the_turkish_capital_i():
    assert mod._go_lower("İSTANBUL") == "istanbul"


def test_go_lower_drops_pythons_final_sigma_fold():
    """Python's whole-string str.lower() folds a capital sigma at the end
    of a word to the final-form lowercase sigma; Go's strings.ToLower is a
    simple per-rune mapping and never does this. _go_lower must not
    either, even when the string holds no Turkish capital I."""
    assert mod._go_lower("ΟΔΟΣ") == "οδοσ"
    assert mod._go_lower("ΑΣ") == "ασ"
    assert mod._go_lower("ΣΣ") == "σσ"


def test_needle_lowering_uses_go_lower_not_pythons_str_lower(manifest_dir):
    """Exercises the wiring, not just the helper: a needle carrying the
    Turkish capital I must still match a plain lowercase screen. Python's
    whole-string str.lower() on the needle would expand it to two
    characters at load time and this would never match."""
    manifests = _load_one(
        manifest_dir, '[[rules]]\nid = "r"\nstate = "blocked"\ncontains = ["İstanbul"]\n'
    )
    result = classify("istanbul", osc_progress=None, agent_hint="x", manifests=manifests)
    assert result.state == "blocked"


def test_region_text_lowering_uses_go_lower_not_pythons_str_lower(manifest_dir):
    """The other half of the wiring: the screen, not the needle, carries
    the Turkish capital I. Python's str.lower() on the region text would
    leave a stray combining dot in the lowered copy and the plain needle
    would never be found inside it."""
    manifests = _load_one(
        manifest_dir, '[[rules]]\nid = "r"\nstate = "blocked"\ncontains = ["istanbul"]\n'
    )
    result = classify("İstanbul", osc_progress=None, agent_hint="x", manifests=manifests)
    assert result.state == "blocked"


def test_no_vendored_contains_needle_uses_a_codepoint_where_lowering_diverges():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    import tomllib

    divergent = {0x0130}

    def walk(gate_dict: dict) -> None:
        for needle in gate_dict.get("contains") or []:
            for ch in needle:
                assert ord(ch) not in divergent, (
                    f"{needle!r} uses a codepoint where str.lower and Go's strings.ToLower disagree"
                )
        for key in ("all", "any", "not"):
            for sub in gate_dict.get(key) or []:
                walk(sub)

    for path in sorted(hostfacts.MANIFEST_DIR.glob("*.toml")):
        doc = tomllib.loads(path.read_text(encoding="utf-8"))
        for rule in doc.get("rules", []):
            walk(rule)


def test_parse_manifest_raises_manifest_schema_mismatch_directly(manifest_dir):
    """load_manifests catches this and drops the file; parse_manifest itself
    raises it, so a caller that wants to know why can call it directly."""
    path = write_manifest(manifest_dir, "x", '[[rules]]\nid = "r"\nvibes = true\n')
    with pytest.raises(ManifestSchemaMismatch):
        mod.parse_manifest(path, path.read_text(encoding="utf-8"))


# --- evaluation semantics, mirroring evaluate.go ----------------------------


def _compiled(rules_toml: str, agent="x"):
    import tempfile
    from pathlib import Path as _Path

    with tempfile.TemporaryDirectory() as tmp:
        d = _Path(tmp)
        write_manifest(d, agent, rules_toml)
        manifests = load_manifests([d])
    return manifests[agent]


def test_highest_priority_wins():
    m = _compiled(
        '[[rules]]\nid = "low"\nstate = "idle"\npriority = 10\ncontains = ["marker"]\n'
        '[[rules]]\nid = "high"\nstate = "blocked"\npriority = 20\ncontains = ["marker"]\n'
    )
    result = classify("marker", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert (result.state, result.rule) == ("blocked", "high")
    assert result.priority == 20


def test_match_carries_the_winning_rules_region():
    m = _compiled('[[rules]]\nid = "r"\nstate = "blocked"\nregion = "osc_title"\nregex = ["^x"]\n')
    result = classify("ignored", title="xyz", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.region == "osc_title"


def test_a_tie_keeps_the_earlier_rule():
    m = _compiled(
        '[[rules]]\nid = "first"\nstate = "working"\npriority = 5\ncontains = ["marker"]\n'
        '[[rules]]\nid = "second"\nstate = "idle"\npriority = 5\ncontains = ["marker"]\n'
    )
    result = classify("marker", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.rule == "first"


def test_contains_is_case_insensitive_and_regex_is_not():
    m = _compiled(
        '[[rules]]\nid = "ci"\nstate = "blocked"\ncontains = ["Do You Want To Proceed?"]\n'
        '[[rules]]\nid = "cs"\nstate = "working"\npriority = -1\nregex = ["^Exact$"]\n'
    )
    result = classify(
        "do you want to proceed?", osc_progress=None, agent_hint="x", manifests={"x": m}
    )
    assert result.rule == "ci"
    result = classify("exact", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.state == "unknown"


def test_each_line_regex_must_match_some_line_independently():
    m = _compiled('[[rules]]\nid = "two"\nstate = "blocked"\nline_regex = ["^alpha$", "^beta$"]\n')
    result = classify("alpha\nbeta", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.state == "blocked"
    result = classify("alpha", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.state == "unknown"


def test_any_is_ignored_when_empty_and_enforced_when_present():
    m = _compiled(
        '[[rules]]\nid = "r"\nstate = "blocked"\ncontains = ["base"]\n'
        'any = [{ contains = ["one"] }, { contains = ["two"] }]\n'
    )
    result = classify("base", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.state == "unknown"
    result = classify("base two", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.state == "blocked"


def test_not_blocks_a_match():
    m = _compiled(
        '[[rules]]\nid = "r"\nstate = "idle"\ncontains = ["prompt"]\n'
        'not = [{ contains = ["esc to cancel"] }]\n'
    )
    result = classify("prompt", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert result.state == "idle"
    result = classify(
        "prompt, esc to cancel", osc_progress=None, agent_hint="x", manifests={"x": m}
    )
    assert result.state == "unknown"


def test_nested_all_any_not_composition():
    m = _compiled(
        '[[rules]]\nid = "r"\nstate = "blocked"\ncontains = ["base"]\n'
        "all = [\n"
        "  { any = [\n"
        '    { contains = ["one"] },\n'
        '    { contains = ["two"], not = [{ contains = ["exclude"] }] },\n'
        "  ] },\n"
        "]\n"
    )
    assert (
        classify("base two", osc_progress=None, agent_hint="x", manifests={"x": m}).state
        == "blocked"
    )
    assert (
        classify("base one", osc_progress=None, agent_hint="x", manifests={"x": m}).state
        == "blocked"
    )
    assert (
        classify("base two exclude", osc_progress=None, agent_hint="x", manifests={"x": m}).state
        == "unknown"
    )
    assert (
        classify("base", osc_progress=None, agent_hint="x", manifests={"x": m}).state == "unknown"
    )


def test_skip_state_update_reports_skip_true_with_its_own_rule():
    """evaluate.go's Skip means "emit no event at all", but the winning
    rule and its state are still reported: skip is what tells a caller
    "stay silent" apart from "nothing matched".

    The skip rule still has to win the priority race against a real rule
    for this to prove anything.
    """
    m = _compiled(
        '[[rules]]\nid = "viewer"\nstate = "unknown"\npriority = 100\n'
        'skip_state_update = true\ncontains = ["showing detailed transcript"]\n'
        '[[rules]]\nid = "real"\nstate = "blocked"\npriority = 10\n'
        'contains = ["showing detailed transcript"]\n'
    )
    result = classify(
        "showing detailed transcript", osc_progress=None, agent_hint="x", manifests={"x": m}
    )
    assert result.state == "unknown"
    assert result.rule == "viewer"
    assert result.skip is True


def test_no_match_yields_unknown_not_idle():
    m = _compiled('[[rules]]\nid = "r"\nstate = "idle"\ncontains = ["never"]\n')
    result = classify("something else", osc_progress=None, agent_hint="x", manifests={"x": m})
    assert (result.state, result.rule, result.skip) == ("unknown", None, False)


# --- load_manifests: directory precedence -----------------------------------


def test_nothing_loaded_classifies_unknown():
    result = classify("$ ", osc_progress=None, agent_hint=None, manifests={})
    assert (result.state, result.rule) == ("unknown", None)


def test_a_later_directory_overrides_by_agent_id(tmp_path):
    a, b = tmp_path / "a", tmp_path / "b"
    a.mkdir(), b.mkdir()
    write_manifest(a, "x", '[[rules]]\nid = "bundled"\nstate = "working"\ncontains = ["x"]\n')
    write_manifest(b, "x", '[[rules]]\nid = "override"\nstate = "blocked"\ncontains = ["x"]\n')
    manifests = load_manifests([a, b])
    result = classify("x", osc_progress=None, agent_hint="x", manifests=manifests)
    assert (result.state, result.rule) == ("blocked", "override")


def test_a_broken_override_does_not_take_the_bundled_manifest_down(tmp_path):
    a, b = tmp_path / "a", tmp_path / "b"
    a.mkdir(), b.mkdir()
    write_manifest(a, "x", '[[rules]]\nid = "bundled"\nstate = "working"\ncontains = ["x"]\n')
    write_manifest(b, "x", 'mystery = true\n[[rules]]\nid = "r"\ncontains = ["x"]\n')
    manifests = load_manifests([a, b])
    result = classify("x", osc_progress=None, agent_hint="x", manifests=manifests)
    assert (result.state, result.rule) == ("working", "bundled")


def test_the_agent_hint_picks_the_manifest(tmp_path):
    a = tmp_path / "a"
    a.mkdir()
    write_manifest(a, "alpha", '[[rules]]\nid = "a"\nstate = "working"\ncontains = ["busy"]\n')
    write_manifest(a, "beta", '[[rules]]\nid = "b"\nstate = "blocked"\ncontains = ["busy"]\n')
    manifests = load_manifests([a])
    assert (
        classify("busy", osc_progress=None, agent_hint="beta", manifests=manifests).state
        == "blocked"
    )
    assert (
        classify("busy", osc_progress=None, agent_hint="alpha", manifests=manifests).state
        == "working"
    )


def test_no_hint_scans_every_manifest_and_refuses_an_ambiguous_answer(tmp_path):
    a = tmp_path / "a"
    a.mkdir()
    write_manifest(a, "alpha", '[[rules]]\nid = "a"\nstate = "working"\ncontains = ["busy"]\n')
    write_manifest(a, "beta", '[[rules]]\nid = "b"\nstate = "blocked"\ncontains = ["busy"]\n')
    manifests = load_manifests([a])
    result = classify("busy", osc_progress=None, agent_hint=None, manifests=manifests)
    assert (result.state, result.rule) == ("unknown", None)


def test_no_hint_with_a_single_real_answer_reports_it(tmp_path):
    a = tmp_path / "a"
    a.mkdir()
    write_manifest(a, "alpha", '[[rules]]\nid = "a"\nstate = "working"\ncontains = ["busy"]\n')
    write_manifest(a, "beta", '[[rules]]\nid = "b"\nstate = "blocked"\ncontains = ["idle chat"]\n')
    manifests = load_manifests([a])
    result = classify("busy", osc_progress=None, agent_hint=None, manifests=manifests)
    assert (result.state, result.rule) == ("working", "a")


def test_no_hint_dedupes_on_the_answer_not_the_whole_match():
    """Four vendored manifests (amp, claude, codex, grok) all assert the
    same answer for this screen -- working, osc_title_working -- and
    differ only in priority. That must not read as a disagreement: the
    no-hint path used to dedupe on the whole Match, including priority,
    so this screen returned unknown with no hint even though every
    manifest that matched it agreed."""
    reason = hostfacts.skip_reason("vendored_manifests") or hostfacts.skip_reason("screen_fixtures")
    if reason:
        pytest.skip(reason)
    manifests = load_manifests([hostfacts.MANIFEST_DIR])
    path = hostfacts.SCREEN_FIXTURE_DIR / "codex" / "working-1.txt"
    headers, screen = _split_fixture_headers(path.read_text(encoding="utf-8"))
    result = classify(
        screen,
        title=headers.get("osc_title", ""),
        osc_progress=headers.get("osc_progress"),
        agent_hint=None,
        manifests=manifests,
    )
    assert (result.state, result.rule) == ("working", "osc_title_working")


# --- default_manifest_dirs: no COPPICE_MANIFESTS ----------------------------


def test_bundled_dir_comes_before_the_override_dir():
    dirs = default_manifest_dirs()
    assert dirs[0] == hostfacts.MANIFEST_DIR
    assert dirs[-1].parts[-2:] == ("coppice", "agent-detection")


def test_there_is_no_coppice_manifests_environment_override(monkeypatch, tmp_path):
    monkeypatch.setenv("COPPICE_MANIFESTS", str(tmp_path))
    dirs = default_manifest_dirs()
    # R9: no COPPICE_MANIFESTS. The bundled directory is always the fixed
    # repo path, not whatever this environment variable happens to name.
    assert dirs[0] == hostfacts.MANIFEST_DIR
    assert dirs[0] != tmp_path


# --- the vendored manifests and the shared screen fixtures ------------------


def test_every_vendored_manifest_loads():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    manifests = load_manifests([hostfacts.MANIFEST_DIR])
    assert len(manifests) == 21, (
        f"{len(manifests)} manifests loaded, want the 21 vendored: {sorted(manifests)}"
    )


_HEADER_PREFIXES = ("#rule:", "#osc_title:", "#osc_progress:")


def _split_fixture_headers(body: str) -> tuple[dict[str, str], str]:
    """Strip the header run testdata/screens/README.md documents.

    Headers are only headers at the top of the file, one per line. The
    first line that does not start with one of the three recognized
    prefixes ends the header run; everything from there on is screen text,
    even a line that happens to start with "#".

    Go's two readers disagree on a duplicate: fixtureExpectedRule returns
    on the first "#rule:" match while scanning in order, but
    inputFromFixture's OSCTitle/OSCProgress assignment runs to the end of
    the header run, so the last "#osc_title:"/"#osc_progress:" wins. This
    reader keeps that same asymmetry.
    """
    lines = body.split("\n")
    headers: dict[str, str] = {}
    i = 0
    while i < len(lines):
        prefix = next((p for p in _HEADER_PREFIXES if lines[i].startswith(p)), None)
        if prefix is None:
            break
        key = prefix[1:-1]
        value = lines[i][len(prefix) :].strip()
        if key == "rule":
            headers.setdefault(key, value)
        else:
            headers[key] = value
        i += 1
    return headers, "\n".join(lines[i:])


def test_fixture_headers_are_stripped_before_evaluation():
    body = "#rule: should_not_leak\n#osc_title: also stripped\n#osc_progress: 1;2\nreal screen text"
    headers, screen = _split_fixture_headers(body)
    assert screen == "real screen text"
    assert headers["osc_title"] == "also stripped"
    assert headers["osc_progress"] == "1;2"
    assert headers["rule"] == "should_not_leak"


def test_a_header_prefix_after_screen_text_starts_is_not_stripped():
    body = "#rule: real_header\nfirst screen line\n#osc_title: this is screen text, not a header"
    headers, screen = _split_fixture_headers(body)
    assert screen == "first screen line\n#osc_title: this is screen text, not a header"
    assert "osc_title" not in headers


def test_duplicate_rule_header_the_first_one_wins():
    """Go's fixtureExpectedRule returns on the first #rule: match while
    scanning the header run in order; the Python reader must agree."""
    body = "#rule: first\n#rule: second\nscreen text"
    headers, _ = _split_fixture_headers(body)
    assert headers["rule"] == "first"


def test_duplicate_osc_title_header_the_last_one_wins():
    """Go's inputFromFixture assigns OSCTitle on every match while
    scanning in order, so the last one in the header run wins."""
    body = "#osc_title: first\n#osc_title: second\nscreen text"
    headers, _ = _split_fixture_headers(body)
    assert headers["osc_title"] == "second"


def _discover_screen_fixtures() -> list:
    root = hostfacts.SCREEN_FIXTURE_DIR
    out = []
    for agent_dir in sorted(p for p in root.iterdir() if p.is_dir()):
        out.extend(sorted(agent_dir.glob("*.txt")))
    return out


def test_python_and_go_agree_on_every_shared_fixture():
    """The conformance pattern this repo already uses for the verifier.

    internal/detect/manifests_test.go reads these same files with the same
    header algorithm and asserts the same agent, state, and rule. This
    proves the Python evaluator agrees with the Go one on every one of them.
    """
    reason = hostfacts.skip_reason("screen_fixtures") or hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    manifests = load_manifests([hostfacts.MANIFEST_DIR])
    assert manifests, "the vendored manifests loaded to nothing"
    fixtures = _discover_screen_fixtures()
    assert fixtures, "no screen fixtures found under testdata/screens"
    for path in fixtures:
        agent = path.parent.name
        want_state = path.stem.split("-", 1)[0]
        headers, screen = _split_fixture_headers(path.read_text(encoding="utf-8"))
        result = classify(
            screen,
            title=headers.get("osc_title", ""),
            osc_progress=headers.get("osc_progress"),
            agent_hint=agent,
            manifests=manifests,
        )
        assert result.state == want_state, (
            f"{path}: python got state {result.state!r}, want {want_state!r}"
        )
        want_rule = headers.get("rule")
        if want_rule:
            assert result.rule == want_rule, (
                f"{path}: python got rule {result.rule!r}, want {want_rule!r}"
            )
