"""Exercises the Adapter against a real gridlabd model; no mock of the engine.

Every value asserted below is read back from the running model after a real
step_to, never from a request the sidecar merely accepted.
"""

import pytest

from gldsidecar import protocol
from gldsidecar.adapter import Adapter, ModelError
from models import battery_fleet


@pytest.fixture
def fleet(tmp_path):
    glm_path, fleet_path = battery_fleet.write(str(tmp_path), "test_fleet", count=2, pen="00000042")
    import json

    with open(fleet_path, encoding="utf-8") as f:
        fleet_data = json.load(f)
    return glm_path, fleet_data


def _expected_objects(fleet_data):
    expected = {}
    for device in fleet_data["devices"]:
        for class_name, obj_name in device["objects"].items():
            expected.setdefault(class_name, []).append(obj_name)
    return expected


@pytest.fixture
def adapter(fleet):
    glm_path, fleet_data = fleet
    a = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version=fleet_data["gridlabd_version"],
        expected_objects=_expected_objects(fleet_data),
    )
    yield a, fleet_data
    a.shutdown()


def test_hello_reports_the_pinned_version_and_the_fleet_name(adapter):
    a, fleet_data = adapter
    result = a.hello()
    assert result["protocol"] == protocol.PROTOCOL_VERSION
    assert result["gridlabd_version"] == "6.0.0a1"
    assert result["fleet"] == "test_fleet"
    assert set(result["objects"]["inverter"]) == {
        d["objects"]["inverter"] for d in fleet_data["devices"]
    }


def test_hello_raises_when_gridlabd_version_does_not_match_fleet_file(fleet):
    glm_path, fleet_data = fleet
    a = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version="9.9.9",
        expected_objects=_expected_objects(fleet_data),
    )
    try:
        with pytest.raises(ModelError):
            a.hello()
    finally:
        a.shutdown()


def test_hello_raises_when_a_mapped_object_is_missing(fleet):
    glm_path, fleet_data = fleet
    expected = _expected_objects(fleet_data)
    expected["inverter"].append("no_such_inverter")
    a = Adapter(
        model_path=glm_path,
        fleet=fleet_data["fleet"],
        expected_version=fleet_data["gridlabd_version"],
        expected_objects=expected,
    )
    try:
        with pytest.raises(ModelError):
            a.hello()
    finally:
        a.shutdown()


def test_charge_setpoint_holds_across_step_to(adapter):
    a, fleet_data = adapter
    inv = fleet_data["devices"][0]["objects"]["inverter"]
    a.set([{"object": inv, "property": "P_Out", "value": 1234.0}])
    a.step_to("2020-01-01T00:05:00Z")
    readback = a.get([{"object": inv, "property": "P_Out"}])
    assert readback[0]["value"] == 1234.0
    assert readback[0]["time"] == "2020-01-01T00:05:00"


def test_state_of_charge_moves_under_discharge(adapter):
    a, fleet_data = adapter
    inv = fleet_data["devices"][0]["objects"]["inverter"]
    bat = fleet_data["devices"][0]["objects"]["battery"]
    a.set([{"object": inv, "property": "P_Out", "value": 1234.0}])
    before = a.get([{"object": bat, "property": "state_of_charge"}])[0]["value"]
    a.step_to("2020-01-01T00:05:00Z")
    a.step_to("2020-01-01T00:10:00Z")
    after = a.get([{"object": bat, "property": "state_of_charge"}])[0]["value"]
    assert after < before


def test_step_to_past_the_models_last_event_does_not_crash(adapter):
    a, _ = adapter
    # The GLM's stoptime is 02:00:00; both calls land after it. A bare
    # step() in this situation crashes the worker (probed 2026-09-29); the
    # adapter only ever calls step_to.
    first = a.step_to("2020-01-01T02:05:00Z")
    second = a.step_to("2020-01-01T02:10:00Z")
    assert first == "2020-01-01T02:05:00"
    assert second == "2020-01-01T02:10:00"


def test_step_to_an_earlier_time_is_a_no_op(adapter):
    a, fleet_data = adapter
    inv = fleet_data["devices"][0]["objects"]["inverter"]
    a.set([{"object": inv, "property": "P_Out", "value": 500.0}])
    a.step_to("2020-01-01T00:10:00Z")
    before = a.get([{"object": inv, "property": "P_Out"}])[0]
    reached = a.step_to("2020-01-01T00:05:00Z")
    after = a.get([{"object": inv, "property": "P_Out"}])[0]
    assert reached == before["time"]
    assert after["time"] == before["time"]
    assert after["value"] == before["value"]


@pytest.mark.parametrize(
    "bad_time",
    ["not-a-time", "2020-01-01 00:05:00", "2020-01-01T00:05:00", "2020-01-01T00:05:00+00:00"],
)
def test_step_to_rejects_anything_that_is_not_strict_rfc3339_utc(adapter, bad_time):
    a, _ = adapter
    # A malformed time is a status-0 "success" at the gridlabd layer that
    # jams the clock to a garbage value (probed 2026-09-29); the adapter
    # must refuse it before it ever reaches step_to.
    with pytest.raises(protocol.ProtocolError) as exc_info:
        a.step_to(bad_time)
    assert exc_info.value.code == "bad_time"
