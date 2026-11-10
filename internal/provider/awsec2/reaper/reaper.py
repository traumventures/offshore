import os
import time
import boto3

GRACE = 300  # seconds after expiry, the orchestrator goes first
PARAM_GRACE = 900  # secrets are written shortly before their machine starts
LIVE = ["pending", "running", "stopping", "stopped"]


def machines(ec2):
    """Return (instance id, tags, spot request id) of every live machine."""
    out = []
    pages = ec2.get_paginator("describe_instances").paginate(Filters=[
        {"Name": "tag:cloudsourced.kind", "Values": ["machine"]},
        {"Name": "instance-state-name", "Values": LIVE},
    ])
    for page in pages:
        for r in page["Reservations"]:
            for i in r["Instances"]:
                tags = {t["Key"]: t["Value"] for t in i.get("Tags", [])}
                out.append((i["InstanceId"], tags, i.get("SpotInstanceRequestId")))
    return out


def expired(tags, now):
    try:
        expires = int(tags.get("cloudsourced.expires-at", "0"))
    except ValueError:
        expires = 0
    return bool(expires) and now > expires + GRACE


def orphaned_parameters(ssm, prefix, alive, now):
    """Secret parameters under prefix whose machine is gone."""
    out = []
    pages = ssm.get_paginator("get_parameters_by_path").paginate(Path=prefix, Recursive=True)
    for page in pages:
        for p in page["Parameters"]:
            machine = p["Name"].rsplit("/", 1)[-1]
            modified = p.get("LastModifiedDate")
            age = now - modified.timestamp() if modified else PARAM_GRACE + 1
            if machine not in alive and age > PARAM_GRACE:
                out.append(p["Name"])
    return out


def handler(event, context, ec2=None, ssm=None, now=None):
    ec2 = ec2 or boto3.client("ec2")
    now = time.time() if now is None else now
    doomed, alive, requests = [], set(), []
    for instance_id, tags, request in machines(ec2):
        if expired(tags, now):
            doomed.append(instance_id)
            if request:
                requests.append(request)
        elif tags.get("cloudsourced.machine"):
            alive.add(tags["cloudsourced.machine"])
    # A persistent spot request would relaunch its terminated instance.
    for k in range(0, len(requests), 50):
        ec2.cancel_spot_instance_requests(SpotInstanceRequestIds=requests[k:k + 50])
    for k in range(0, len(doomed), 50):
        ec2.terminate_instances(InstanceIds=doomed[k:k + 50])
    deleted = []
    prefix = os.environ.get("MACHINE_PARAMETERS", "")
    if prefix:
        ssm = ssm or boto3.client("ssm")
        for name in orphaned_parameters(ssm, prefix, alive, now):
            try:
                ssm.delete_parameter(Name=name)
                deleted.append(name)
            except ssm.exceptions.ParameterNotFound:
                pass
    result = {"terminated": doomed, "deleted_parameters": deleted}
    print(result)
    return result
