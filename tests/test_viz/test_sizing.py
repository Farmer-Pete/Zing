"""Tests for viz/sizing.py."""

from __future__ import annotations

from zing_ai.viz.sizing import size_for_node


def test_rect_short_label_uses_minimum_width() -> None:
    sz = size_for_node({"shape": "rect", "label": "go"})
    assert sz["width"] == 160
    assert sz["height"] == 44


def test_rect_long_label_scales_with_length() -> None:
    label = "x" * 40
    sz = size_for_node({"shape": "rect", "label": label})
    assert sz["width"] == 40 * 7 + 24
    assert sz["height"] == 44


def test_rect_default_used_for_unknown_shape() -> None:
    sz = size_for_node({"shape": "unknown-future-shape", "label": "abc"})
    assert sz["width"] == 160
    assert sz["height"] == 44


def test_diamond_height_is_70_and_width_scales() -> None:
    sz = size_for_node({"shape": "diamond", "label": "exception?"})
    assert sz["height"] == 70
    short = size_for_node({"shape": "diamond", "label": ""})
    assert short["width"] == 140
    long_label = size_for_node({"shape": "diamond", "label": "x" * 30})
    assert long_label["width"] == 30 * 7.5 + 28


def test_hexagon_dimensions() -> None:
    short = size_for_node({"shape": "hexagon", "label": "x"})
    assert short["width"] == 180
    assert short["height"] == 48
    long_label = size_for_node({"shape": "hexagon", "label": "x" * 30})
    assert long_label["width"] == 30 * 7 + 60


def test_parallelogram_dimensions() -> None:
    short = size_for_node({"shape": "parallelogram", "label": "x"})
    assert short["width"] == 180
    assert short["height"] == 44
    long_label = size_for_node({"shape": "parallelogram", "label": "x" * 30})
    assert long_label["width"] == 30 * 7 + 30


def test_diverged_width_scales_with_longest_of_today_proposed_concern() -> None:
    # longest field drives width
    sz = size_for_node(
        {
            "shape": "diverged",
            "label": "",
            "concern": "short",
            "today_label": "medium-length-today",
            "proposed_label": "x" * 50,
        }
    )
    assert sz["height"] == 100
    assert sz["width"] == max(280, min(440, 50 * 6.5 + 40))


def test_diverged_width_clamps_to_minimum_280() -> None:
    sz = size_for_node(
        {
            "shape": "diverged",
            "label": "",
            "concern": "a",
            "today_label": "b",
            "proposed_label": "c",
        }
    )
    assert sz["width"] == 280


def test_diverged_width_clamps_to_maximum_440() -> None:
    sz = size_for_node(
        {
            "shape": "diverged",
            "label": "",
            "concern": "x" * 200,
            "today_label": "x" * 200,
            "proposed_label": "x" * 200,
        }
    )
    assert sz["width"] == 440


def test_diverged_uses_concern_when_longest() -> None:
    sz = size_for_node(
        {
            "shape": "diverged",
            "label": "",
            "concern": "x" * 50,
            "today_label": "a",
            "proposed_label": "b",
        }
    )
    assert sz["width"] == max(280, min(440, 50 * 6.5 + 40))


def test_missing_label_defaults_to_empty() -> None:
    sz = size_for_node({"shape": "rect"})
    assert sz["width"] == 160
    assert sz["height"] == 44
