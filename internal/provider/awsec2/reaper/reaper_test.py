"""Tests of the reaper Lambda with fake AWS clients (no boto3 needed)."""
import datetime
import os
import sys
import types
import unittest

sys.modules.setdefault("boto3", types.ModuleType("boto3"))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import reaper  # noqa: E402

NOW = 1_800_000_000


class Pager:
    def __init__(self, pages, calls):
        self.pages, self.calls = pages, calls

    def paginate(self, **kw):
        self.calls.append(kw)
        return self.pages


class FakeEC2:
    def __init__(self, instances):
        self.instances, self.calls, self.terminated, self.canceled = instances, [], [], []

    def get_paginator(self, op):
        assert op == "describe_instances"
        half = len(self.instances) // 2
        pages = [{"Reservations": [{"Instances": self.instances[:half]}]},
                 {"Reservations": [{"Instances": self.instances[half:]}]}]
        return Pager(pages, self.calls)

    def terminate_instances(self, InstanceIds):
        self.terminated.extend(InstanceIds)

    def cancel_spot_instance_requests(self, SpotInstanceRequestIds):
        assert not self.terminated, "spot requests are canceled before terminating"
        self.canceled.extend(SpotInstanceRequestIds)


class NotFound(Exception):
    pass


class FakeSSM:
    def __init__(self, params):
        self.params, self.calls, self.deleted = params, [], []
        self.exceptions = types.SimpleNamespace(ParameterNotFound=NotFound)

    def get_paginator(self, op):
        assert op == "get_parameters_by_path"
        return Pager([{"Parameters": self.params[:1]}, {"Parameters": self.params[1:]}], self.calls)

    def delete_parameter(self, Name):
        if Name.endswith("/vanished"):
            raise NotFound(Name)
        self.deleted.append(Name)


def inst(iid, machine, expires):
    return {"InstanceId": iid, "Tags": [
        {"Key": "cloudsourced.kind", "Value": "machine"},
        {"Key": "cloudsourced.machine", "Value": machine},
        {"Key": "cloudsourced.expires-at", "Value": str(expires)},
    ]}


def param(machine, age):
    return {"Name": "/cloudsourced/st/machines/" + machine,
            "LastModifiedDate": datetime.datetime.fromtimestamp(NOW - age, tz=datetime.timezone.utc)}


class ReaperTest(unittest.TestCase):
    def setUp(self):
        os.environ["MACHINE_PARAMETERS"] = "/cloudsourced/st/machines"

    def test_terminates_expired_and_deletes_orphaned_secrets(self):
        ec2 = FakeEC2([
            inst("i-live", "live", NOW + 3600),
            inst("i-old", "old", NOW - reaper.GRACE - 1),
            inst("i-grace", "grace", NOW - 10),
            {"InstanceId": "i-bad", "Tags": [{"Key": "cloudsourced.expires-at", "Value": "x"}]},
        ])
        ssm = FakeSSM([
            param("live", 7200),            # machine running: keep
            param("old", 7200),             # machine terminated now: delete
            param("gone", 7200),            # no instance: delete
            param("fresh", 60),             # machine not started yet: keep
            param("grace", 7200),           # expired within grace: keep
            param("vanished", 7200),        # deleted concurrently: ignored
        ])
        res = reaper.handler({}, None, ec2=ec2, ssm=ssm, now=NOW)
        self.assertEqual(ec2.terminated, ["i-old"])
        self.assertEqual(sorted(ssm.deleted), ["/cloudsourced/st/machines/gone", "/cloudsourced/st/machines/old"])
        self.assertEqual(res["terminated"], ["i-old"])
        self.assertEqual(sorted(res["deleted_parameters"]), sorted(ssm.deleted))
        self.assertEqual(ssm.calls, [{"Path": "/cloudsourced/st/machines", "Recursive": True}])
        states = ec2.calls[0]["Filters"][1]["Values"]
        self.assertNotIn("terminated", states)

    def test_cancels_spot_requests_of_expired_machines(self):
        old = inst("i-spot", "spot", NOW - 3600)
        old["SpotInstanceRequestId"] = "sir-old"
        live = inst("i-live", "live", NOW + 3600)
        live["SpotInstanceRequestId"] = "sir-live"
        ec2 = FakeEC2([old, live, inst("i-od", "od", NOW - 3600)])
        reaper.handler({}, None, ec2=ec2, ssm=FakeSSM([]), now=NOW)
        self.assertEqual(ec2.canceled, ["sir-old"])
        self.assertEqual(sorted(ec2.terminated), ["i-od", "i-spot"])

    def test_batches_terminations(self):
        ec2 = FakeEC2([inst("i-%d" % n, "m%d" % n, NOW - 3600) for n in range(120)])
        calls = []
        ec2.terminate_instances = lambda InstanceIds: calls.append(len(InstanceIds))
        reaper.handler({}, None, ec2=ec2, ssm=FakeSSM([]), now=NOW)
        self.assertEqual(calls, [50, 50, 20])

    def test_without_prefix_leaves_parameters(self):
        os.environ.pop("MACHINE_PARAMETERS")
        ssm = FakeSSM([param("gone", 7200)])
        res = reaper.handler({}, None, ec2=FakeEC2([]), ssm=ssm, now=NOW)
        self.assertEqual(ssm.deleted, [])
        self.assertEqual(res["deleted_parameters"], [])


if __name__ == "__main__":
    unittest.main()
