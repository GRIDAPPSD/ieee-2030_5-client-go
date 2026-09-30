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
    [
        "not-a-time",
        "2020-01-01 00:05:00",
        "2020-01-01T00:05:00",
        "2020-01-01T00:05:00+00:00",
        "2020-02-30T00:00:00Z",  # matches the format, not a real calendar date
        "2020-01-01T00:05:00Z\n",  # would pass a $ -anchored regex (probed 2026-09-29)
        20200101,  # not a string at all
    ],
)
def test_step_to_rejects_anything_that_is_not_strict_rfc3339_utc(adapter, bad_time):
    a, _ = adapter
    # A malformed time is a status-0 "success" at the gridlabd layer that
    # jams the clock to a garbage value (probed 2026-09-29); the adapter
    # must refuse it before it ever reaches step_to.
    with pytest.raises(protocol.ProtocolError) as exc_info:
        a.step_to(bad_time)
    assert exc_info.value.code == "bad_time"


def test_construction_fails_when_the_glm_file_does_not_exist(tmp_path):
    with pytest.raises(ModelError):
        Adapter(
            model_path=str(tmp_path / "no_such.glm"),
            fleet="f",
            expected_version="6.0.0a1",
            expected_objects={},
        )


def test_construction_fails_when_the_glm_is_malformed(tmp_path):
    # load_glm reports a nonzero status for a bad property (probed
    # 2026-09-29: status 6) and setup_after_load/start "come up OK"
    # regardless; the Adapter must check load_glm's own status itself.
    bad_glm = tmp_path / "bad.glm"
    bad_glm.write_text(
        "clock {\n"
        "  starttime '2020-01-01 00:00:00';\n"
        "  stoptime '2020-01-01 02:00:00';\n"
        "}\n"
        "module powerflow;\n"
        "object meter {\n"
        "  name m1;\n"
        "  bogus_property_that_does_not_exist 5;\n"
        "}\n"
    )
    with pytest.raises(ModelError):
        Adapter(model_path=str(bad_glm), fleet="f", expected_version="6.0.0a1", expected_objects={})


def test_set_is_atomic_a_bad_item_leaves_no_earlier_item_applied(adapter):
    a, fleet_data = adapter
    inv = fleet_data["devices"][0]["objects"]["inverter"]
    initial = a.get([{"object": inv, "property": "P_Out"}])[0]["value"]

    with pytest.raises(ModelError):
        a.set(
            [
                {"object": inv, "property": "P_Out", "value": 4242.0},
                {"object": "no_such_object", "property": "P_Out", "value": 1.0},
            ]
        )

    after = a.get([{"object": inv, "property": "P_Out"}])[0]["value"]
    assert after == initial


class _StubNative:
    """A minimal stand-in for gridlabd.GridLabD, used only to drive a
    specific non-OK native status through the Adapter's own checks
    (set_property/step_to/get_property) without needing a real gridlabd
    failure to reproduce it. Never used by production code (Adapter only
    accepts one through its native= test seam)."""

    def __init__(self, *, set_status=0, step_status=0, get_status=0, clock="2020-01-01T00:00:00"):
        self._set_status = set_status
        self._step_status = step_status
        self._get_status = get_status
        self._clock = clock

    def get_property(self, obj, prop):
        return self._get_status, 0.0

    def set_property(self, obj, prop, value):
        return self._set_status

    def step_to(self, time):
        return self._step_status, self._clock

    def get_clock(self):
        return self._clock

    def get_object_names_by_class(self, class_name):
        return []

    def stop(self):
        pass


def test_set_raises_when_the_native_set_property_call_itself_fails():
    # get_status stays 0 so the atomicity precheck passes; only
    # set_property's own status (the check this drives) is nonzero.
    stub = _StubNative(set_status=5)
    a = Adapter(model_path="unused", fleet="f", expected_version="6.0.0a1", expected_objects={}, native=stub)
    with pytest.raises(ModelError) as exc_info:
        a.set([{"object": "obj", "property": "P_Out", "value": 1.0}])
    assert exc_info.value.status == 5


def test_step_to_raises_when_the_native_step_to_call_itself_fails():
    stub = _StubNative(step_status=7)
    a = Adapter(model_path="unused", fleet="f", expected_version="6.0.0a1", expected_objects={}, native=stub)
    with pytest.raises(ModelError) as exc_info:
        a.step_to("2020-01-01T00:05:00Z")
    assert exc_info.value.status == 7


def test_get_raises_when_the_native_get_property_call_itself_fails():
    stub = _StubNative(get_status=9)
    a = Adapter(model_path="unused", fleet="f", expected_version="6.0.0a1", expected_objects={}, native=stub)
    with pytest.raises(ModelError) as exc_info:
        a.get([{"object": "obj", "property": "P_Out"}])
    assert exc_info.value.status == 9
