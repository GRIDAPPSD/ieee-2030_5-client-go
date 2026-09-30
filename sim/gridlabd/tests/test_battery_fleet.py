import re

import pytest

from models import battery_fleet

_HEX40 = re.compile(r"^[0-9A-F]{40}$")


def test_lfdi_is_40_uppercase_hex_digits_with_pen_right_concatenated():
    _, fleet = battery_fleet.generate("fleetA", count=4, pen="0A0B0C0D")
    for device in fleet["devices"]:
        lfdi = device["lfdi"]
        assert _HEX40.match(lfdi), lfdi
        assert lfdi.endswith("0A0B0C0D")


def test_lfdi_stable_across_runs_with_same_arguments():
    _, fleet1 = battery_fleet.generate("fleetA", count=4, pen="0A0B0C0D", seed="s1")
    _, fleet2 = battery_fleet.generate("fleetA", count=4, pen="0A0B0C0D", seed="s1")
    assert [d["lfdi"] for d in fleet1["devices"]] == [d["lfdi"] for d in fleet2["devices"]]


def test_lfdi_depends_only_on_its_own_index_not_on_fleet_size():
    _, small = battery_fleet.generate("fleetA", count=2, pen="0A0B0C0D", seed="s1")
    _, large = battery_fleet.generate("fleetA", count=5, pen="0A0B0C0D", seed="s1")
    assert small["devices"][0]["lfdi"] == large["devices"][0]["lfdi"]
    assert small["devices"][1]["lfdi"] == large["devices"][1]["lfdi"]


def test_lfdi_differs_by_pen():
    _, fleet1 = battery_fleet.generate("fleetA", count=1, pen="00000000", seed="s1")
    _, fleet2 = battery_fleet.generate("fleetA", count=1, pen="FFFFFFFF", seed="s1")
    assert fleet1["devices"][0]["lfdi"] != fleet2["devices"][0]["lfdi"]
    assert fleet1["devices"][0]["lfdi"][:32] == fleet2["devices"][0]["lfdi"][:32]


def test_rejects_a_pen_that_is_not_8_hex_digits():
    with pytest.raises(ValueError):
        battery_fleet.generate("fleetA", count=1, pen="ABC")
    with pytest.raises(ValueError):
        battery_fleet.generate("fleetA", count=1, pen="ZZZZZZZZ")


def test_glm_holds_one_battery_and_inverter_pair_per_device():
    glm_text, fleet = battery_fleet.generate("fleetB", count=3, pen="00000001")
    assert glm_text.count("object battery {") == 3
    assert glm_text.count("object inverter {") == 3
    assert glm_text.count("four_quadrant_control_mode CONSTANT_PQ;") == 3
    for device in fleet["devices"]:
        assert device["objects"]["inverter"] in glm_text
        assert device["objects"]["battery"] in glm_text


def test_write_creates_glm_and_fleet_file(tmp_path):
    glm_path, fleet_path = battery_fleet.write(str(tmp_path), "fleetC", count=2, pen="00000002")
    assert (tmp_path / "fleetC.glm").is_file()
    assert (tmp_path / "fleetC.fleet.json").is_file()
    assert glm_path == str(tmp_path / "fleetC.glm")
    assert fleet_path == str(tmp_path / "fleetC.fleet.json")
